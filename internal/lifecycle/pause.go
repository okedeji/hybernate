/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lifecycle

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

// ErrNotPrepared means Pause was called without the record Prepare makes,
// from which the pause would be undone.
var ErrNotPrepared = errors.New("pause not prepared")

// Pauser pauses and resumes workloads. A pause is recorded in the
// ManagedWorkload's status by Prepare, and the caller persists that record
// before Pause changes anything, so every step can be repeated after an
// interruption and undone exactly.
type Pauser struct {
	client      client.Client
	scaler      WorkloadScaler
	autoscalers *autoscaler.Finder
	clock       func() metav1.Time
}

// NewPauser returns a Pauser that finds workloads' autoscalers with
// autoscalers, so a KEDA one is held at zero while they're paused.
func NewPauser(c client.Client, autoscalers *autoscaler.Finder) *Pauser {
	return &Pauser{
		client:      c,
		scaler:      &subResourceScaler{client: c},
		autoscalers: autoscalers,
		clock:       metav1.Now,
	}
}

// Prepare records in workload.Status.Pause what pausing will change: the
// replicas, and the KEDA ScaledObject with its own paused-replicas
// annotation. It changes nothing in the cluster.
func (p *Pauser) Prepare(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return fmt.Errorf("getting target workload: %w", err)
	}
	scale, err := p.scaler.GetScale(ctx, target)
	if err != nil {
		return fmt.Errorf("getting current replicas: %w", err)
	}
	pause := &v1alpha1.PauseStatus{PreviousReplicas: scale.Spec.Replicas}
	a, found, err := p.autoscalers.FindNow(ctx, workload.Namespace, workload.Spec.Target.Kind, workload.Spec.Target.Name)
	if err != nil {
		return fmt.Errorf("finding the workload's autoscaler: %w", err)
	}
	if found && a.Kind == autoscaler.KEDA {
		pause.ScaledObject = a.Name
		pause.ScaledObjectPausedReplicas = a.PausedReplicas
	}
	workload.Status.Pause = pause
	return nil
}

// Pause scales a prepared workload to zero, holding its KEDA ScaledObject
// there, and returns true when it's done. PVCs are left intact: they persist
// independently of the replica count.
func (p *Pauser) Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	pause := workload.Status.Pause
	if pause == nil {
		return false, ErrNotPrepared
	}
	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return false, fmt.Errorf("getting target workload: %w", err)
	}
	if so := pause.ScaledObject; so != "" {
		if err := autoscaler.HoldKEDA(ctx, p.client, workload.Namespace, so, 0); err != nil {
			return false, err
		}
	}
	if err := scaleTo(ctx, p.scaler, target, 0); err != nil {
		return false, fmt.Errorf("scaling to zero: %w", err)
	}
	if pause.PausedAt == nil {
		now := p.clock()
		pause.PausedAt = &now
	}
	return true, nil
}

// Resume scales a paused workload back up and returns true once its pods are
// Ready. The pause record is left for the caller to clear as it records the
// workload Running, so a retry after that write fails can resume again.
func (p *Pauser) Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	pause := workload.Status.Pause
	if pause == nil {
		return true, nil
	}

	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return false, fmt.Errorf("getting target workload: %w", err)
	}

	replicas, a, err := p.resumeReplicas(ctx, workload)
	if err != nil {
		return false, err
	}

	// KEDA holds the workload at the restored replicas until they're Ready.
	// Released any earlier, a ScaledObject that may scale to zero, with no
	// trigger active, could take it back down while it starts, and the two
	// would fight over the replicas for as long as that lasted.
	held := a.Kind == autoscaler.KEDA && a.Name == pause.ScaledObject && a.HeldAt(replicas)
	if so := pause.ScaledObject; so != "" && !held {
		if err := autoscaler.HoldKEDA(ctx, p.client, workload.Namespace, so, replicas); err != nil {
			return false, err
		}
	}
	if err := scaleTo(ctx, p.scaler, target, replicas); err != nil {
		return false, fmt.Errorf("scaling to %d: %w", replicas, err)
	}

	ready, err := checkReady(ctx, p.client, target, replicas)
	if err != nil {
		return false, fmt.Errorf("checking readiness: %w", err)
	}
	if !ready {
		return false, nil
	}

	if err := p.releaseKEDA(ctx, workload); err != nil {
		return false, err
	}
	return true, nil
}

// Restore hands a paused workload back without waiting for it: scaled back
// to its recorded replicas if it's still at zero, and its ScaledObject
// released. It's for when Hybernate stops managing the workload, or someone
// else has already woken it, so it never scales down what's running. It
// returns the replicas the target is left with: zero when it's gone, or was
// at zero before the pause. The pause record is left for the caller to
// clear once it has persisted the outcome.
func (p *Pauser) Restore(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error) {
	if workload.Status.Pause == nil {
		return 0, nil
	}
	var replicas int32
	target, err := getTarget(ctx, p.client, workload)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return 0, fmt.Errorf("getting target workload: %w", err)
	default:
		if replicas, err = p.scaleUpFromZero(ctx, workload, target); err != nil {
			return 0, err
		}
	}
	return replicas, p.releaseKEDA(ctx, workload)
}

func (p *Pauser) scaleUpFromZero(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (int32, error) {
	scale, err := p.scaler.GetScale(ctx, target)
	if err != nil {
		return 0, fmt.Errorf("getting current replicas: %w", err)
	}
	if scale.Spec.Replicas > 0 {
		return scale.Spec.Replicas, nil
	}
	replicas, _, err := p.resumeReplicas(ctx, workload)
	if err != nil || replicas == 0 {
		return 0, err
	}
	scale.Spec.Replicas = replicas
	if err := p.scaler.UpdateScale(ctx, target, scale); err != nil {
		return 0, fmt.Errorf("updating scale to %d: %w", replicas, err)
	}
	return replicas, nil
}

func (p *Pauser) releaseKEDA(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	pause := workload.Status.Pause
	if pause.ScaledObject == "" {
		return nil
	}
	return autoscaler.ReleaseKEDA(ctx, p.client, workload.Namespace, pause.ScaledObject, pause.ScaledObjectPausedReplicas)
}

// resumeReplicas is what a paused workload is scaled back to, with the
// autoscaler found for it, if any: what it ran before, kept within that
// autoscaler's range should it have changed, and at least one. A workload
// that was at zero before its pause goes back to zero: whoever scaled it
// there meant it to stay off, and Hybernate never starts what it didn't
// stop.
func (p *Pauser) resumeReplicas(ctx context.Context, workload *v1alpha1.ManagedWorkload) (
	int32, autoscaler.Autoscaler, error) {
	a, found, err := p.autoscalers.FindNow(ctx, workload.Namespace, workload.Spec.Target.Kind,
		workload.Spec.Target.Name)
	if err != nil {
		return 0, autoscaler.Autoscaler{}, fmt.Errorf("finding the workload's autoscaler: %w", err)
	}
	replicas := workload.Status.Pause.PreviousReplicas
	if replicas == 0 {
		return 0, a, nil
	}
	if found {
		replicas = a.Clamp(replicas)
	}
	return max(replicas, 1), a, nil
}
