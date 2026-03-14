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

// Scaler executes replica count changes on a target workload.
type Scaler struct {
	client client.Client
	scaler WorkloadScaler
	clock  func() metav1.Time
}

func NewScaler(c client.Client) *Scaler {
	return &Scaler{
		client: c,
		scaler: &subResourceScaler{client: c},
		clock:  metav1.Now,
	}
}

// Scale sets the target workload to the desired replica count and updates
// status. Returns true when the workload has reached the desired count.
func (s *Scaler) Scale(ctx context.Context, workload *v1alpha1.ManagedWorkload, target int32) (bool, error) {
	obj, err := getTarget(ctx, s.client, workload)
	if err != nil {
		return false, fmt.Errorf("getting target workload: %w", err)
	}

	current, err := s.scaler.GetScale(ctx, obj)
	if err != nil {
		return false, fmt.Errorf("getting current replicas: %w", err)
	}

	previous := current.Spec.Replicas

	if err := scaleTo(ctx, s.scaler, obj, target); err != nil {
		return false, fmt.Errorf("scaling to %d: %w", target, err)
	}

	now := s.clock()
	workload.Status.Scale = &v1alpha1.ScaleStatus{
		PreviousReplicas: previous,
		CurrentReplicas:  target,
		ScaledAt:         &now,
	}

	ready, err := checkReady(ctx, s.scaler, obj, target)
	if err != nil {
		return false, fmt.Errorf("checking readiness: %w", err)
	}

	return ready, nil
}
