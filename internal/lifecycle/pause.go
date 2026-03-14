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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Pauser handles pausing and resuming workloads. Each method is safe to call
// multiple times — progress is checkpointed in the ManagedWorkload status.
type Pauser struct {
	client client.Client
	scaler WorkloadScaler
	clock  func() metav1.Time
}

func NewPauser(c client.Client) *Pauser {
	return &Pauser{
		client: c,
		scaler: &subResourceScaler{client: c},
		clock:  metav1.Now,
	}
}

// Pause drives a workload toward the Paused state. Returns true when complete.
// PVCs are left intact — they persist independently of the workload's replica count.
func (p *Pauser) Pause(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return false, fmt.Errorf("getting target workload: %w", err)
	}

	if workload.Status.Pause == nil {
		scale, err := p.scaler.GetScale(ctx, target)
		if err != nil {
			return false, fmt.Errorf("getting current replicas: %w", err)
		}
		workload.Status.Pause = &v1alpha1.PauseStatus{
			PreviousReplicas: scale.Spec.Replicas,
		}
	}

	if err := scaleTo(ctx, p.scaler, target, 0); err != nil {
		return false, fmt.Errorf("scaling to zero: %w", err)
	}

	if workload.Status.Pause.PausedAt == nil {
		now := p.clock()
		workload.Status.Pause.PausedAt = &now
	}

	return true, nil
}

// Resume drives a workload from Paused back to Running. Returns true when complete.
func (p *Pauser) Resume(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	if workload.Status.Pause == nil {
		return true, nil
	}

	target, err := getTarget(ctx, p.client, workload)
	if err != nil {
		return false, fmt.Errorf("getting target workload: %w", err)
	}

	replicas := workload.Status.Pause.PreviousReplicas
	if replicas == 0 {
		replicas = 1
	}

	if err := scaleTo(ctx, p.scaler, target, replicas); err != nil {
		return false, fmt.Errorf("scaling to %d: %w", replicas, err)
	}

	ready, err := checkReady(ctx, p.scaler, target, replicas)
	if err != nil {
		return false, fmt.Errorf("checking readiness: %w", err)
	}
	if !ready {
		return false, nil
	}

	workload.Status.Pause = nil
	return true, nil
}
