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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// Suspension is how far a pause has got: whether it's over, and how many of
// the target's pods are still running. A pause that's over with pods still
// running stopped waiting for them.
type Suspension struct {
	Done    bool
	Running int32
}

// Pause scales a prepared workload to zero, holding its KEDA ScaledObject
// there, and is done once its pods have stopped, or once it has waited
// for them as long as stopWait allows. PausedAt records when it was scaled
// to zero, from which that's timed. Before anything is scaled, the record
// is copied onto the target, where RecordOn finds it should the
// ManagedWorkload's status be lost. Its PVCs stay only while its
// controller keeps them: a StatefulSet whose
// persistentVolumeClaimRetentionPolicy.whenScaled is Delete deletes them as
// it scales down, so the caller must never pause one.
func (p *Pauser) Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (Suspension, error) {
	pause := workload.Status.Pause
	if pause == nil {
		return Suspension{}, ErrNotPrepared
	}
	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return Suspension{}, fmt.Errorf("getting target workload: %w", err)
	}
	if err := copyRecord(ctx, p.client, target, pause); err != nil {
		return Suspension{}, err
	}
	if so := pause.ScaledObject; so != "" {
		if err := autoscaler.HoldKEDA(ctx, p.client, workload.Namespace, so, 0); err != nil {
			return Suspension{}, err
		}
	}
	scale, err := scaleTo(ctx, p.scaler, target, 0)
	if err != nil {
		return Suspension{}, fmt.Errorf("scaling to zero: %w", err)
	}
	now := p.clock()
	if pause.PausedAt == nil {
		pause.PausedAt = &now
	}
	running := scale.Status.Replicas
	waited := now.Sub(pause.PausedAt.Time)
	return Suspension{Done: running == 0 || waited >= stopWait(target, pause.PreviousReplicas), Running: running}, nil
}

// stopWait is how long a pause waits for the target's pods to stop once
// it's scaled to zero: each pod's termination grace period, and a minute
// more for its controller to see them go. A Deployment's pods stop
// together, and a StatefulSet's one after another. One that never stops,
// such as a StatefulSet pod that isn't Ready, which OrderedReady pod
// management waits on before stopping the rest, would otherwise hold the
// pause for good.
func stopWait(target client.Object, replicas int32) time.Duration {
	grace := int64(corev1.DefaultTerminationGracePeriodSeconds)
	var pods int64 = 1
	switch t := target.(type) {
	case *appsv1.Deployment:
		if g := t.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
			grace = *g
		}
	case *appsv1.StatefulSet:
		if g := t.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil {
			grace = *g
		}
		pods = int64(max(replicas, 1))
	}
	return time.Duration(pods*grace)*time.Second + time.Minute
}

// ServingWait is how long a resume waits for the rest of its replicas from
// when it first saw one Ready. Those serve, and one that's crash-looping, or
// was never Ready before the pause, would otherwise hold the resume, and
// KEDA, for good. It's timed from the first Ready replica, not the scale-up,
// so replicas that are slow to start together, such as pods waiting for new
// nodes, aren't cut short just as they come up.
const ServingWait = 2 * time.Minute

// Resumption is how far a resume has got: the replicas the target is
// scaled back to, how many of them are Ready, and whether the resume is
// over, with all of them Ready or with as many as became Ready in the time
// they were given. A resume is also over once something other than
// Hybernate has scaled the target back to zero, which leaves it off.
type Resumption struct {
	Replicas, Ready int32
	Settled         bool
	ScaledToZero    bool
}

// Waited is how long a resume has waited for the workload's replicas:
// since it scaled the workload up, and since it first saw one of them
// Ready, zero until it has.
type Waited struct {
	ScaledUp, Serving time.Duration
}

// Resume scales a paused workload back up, and settles once its replicas
// are Ready, or once it has waited for them long enough: ServingWait from
// the first Ready replica while some are Ready, and the target's progress
// deadline from the scale-up while none are, by which its own controller
// reports them stuck. Settled, the workload's KEDA ScaledObject is
// released. ResumedAt records the scale-up, after which a target at zero
// was scaled there by someone else, and is left there. The pause record is
// left for the caller to clear as it records the workload Running, so a
// retry after that write fails can resume again.
func (p *Pauser) Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload, waited Waited) (
	Resumption, error) {
	pause := workload.Status.Pause
	if pause == nil {
		return Resumption{Settled: true}, nil
	}

	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return Resumption{}, fmt.Errorf("getting target workload: %w", err)
	}
	replicas, a, err := p.resumeReplicas(ctx, workload)
	if err != nil {
		return Resumption{}, err
	}
	scale, err := p.scaler.GetScale(ctx, target)
	if err != nil {
		return Resumption{}, fmt.Errorf("getting current replicas: %w", err)
	}
	if scale.Spec.Replicas == 0 && pause.ResumedAt != nil {
		if err := p.release(ctx, workload, target); err != nil {
			return Resumption{}, err
		}
		return Resumption{Settled: true, ScaledToZero: true}, nil
	}
	// Once it's scaled up, its replicas are left to its autoscaler, or to
	// whoever has set them since: scaling it back to the restored count on
	// each retry would fight an HPA scaling it down as it starts.
	if scale.Spec.Replicas > 0 {
		replicas = scale.Spec.Replicas
	}

	// KEDA holds the workload at the restored replicas until they're Ready.
	// Released any earlier, a ScaledObject that may scale to zero, with no
	// trigger active, could take it back down while it starts, and the two
	// would fight over the replicas for as long as that lasted.
	held := a.Kind == autoscaler.KEDA && a.Name == pause.ScaledObject && a.HeldAt(replicas)
	if so := pause.ScaledObject; so != "" && !held {
		if err := autoscaler.HoldKEDA(ctx, p.client, workload.Namespace, so, replicas); err != nil {
			return Resumption{}, err
		}
	}
	if scale.Spec.Replicas != replicas {
		if _, err := scaleTo(ctx, p.scaler, target, replicas); err != nil {
			return Resumption{}, fmt.Errorf("scaling to %d: %w", replicas, err)
		}
	}
	if pause.ResumedAt == nil && replicas > 0 {
		now := p.clock()
		pause.ResumedAt = &now
	}

	ready, err := readyOf(ctx, p.client, target, replicas)
	if err != nil {
		return Resumption{}, fmt.Errorf("checking readiness: %w", err)
	}
	r := Resumption{Replicas: replicas, Ready: ready}
	switch {
	case ready >= replicas:
	case ready > 0 && waited.Serving >= ServingWait:
	case ready == 0 && waited.ScaledUp >= progressDeadline(target):
	default:
		return r, nil
	}

	if err := p.release(ctx, workload, target); err != nil {
		return Resumption{}, err
	}
	r.Settled = true
	return r, nil
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
	target, err := getTarget(ctx, p.client, workload)
	switch {
	case apierrors.IsNotFound(err):
		return 0, p.release(ctx, workload, nil)
	case err != nil:
		return 0, fmt.Errorf("getting target workload: %w", err)
	}
	replicas, err := p.scaleUpFromZero(ctx, workload, target)
	if err != nil {
		return 0, err
	}
	return replicas, p.release(ctx, workload, target)
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
	// KEDA, still holding the target at zero, would scale it straight back
	// down between the scale-up and its release.
	if so := workload.Status.Pause.ScaledObject; so != "" {
		if err := autoscaler.HoldKEDA(ctx, p.client, workload.Namespace, so, replicas); err != nil {
			return 0, err
		}
	}
	scale.Spec.Replicas = replicas
	if err := p.scaler.UpdateScale(ctx, target, scale); err != nil {
		return 0, fmt.Errorf("updating scale to %d: %w", replicas, err)
	}
	return replicas, nil
}

// release ends a pause that's been undone: KEDA scales the target again,
// and the target, unless it's gone, no longer carries the pause record.
func (p *Pauser) release(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) error {
	if pause := workload.Status.Pause; pause.ScaledObject != "" {
		if err := autoscaler.ReleaseKEDA(ctx, p.client, workload.Namespace, pause.ScaledObject,
			pause.ScaledObjectPausedReplicas); err != nil {
			return err
		}
	}
	if target == nil {
		return nil
	}
	return dropRecord(ctx, p.client, target)
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
