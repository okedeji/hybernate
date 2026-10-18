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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// WorkloadScaler reads and updates the scale subresource of a workload.
type WorkloadScaler interface {
	GetScale(ctx context.Context, obj client.Object) (*autoscalingv1.Scale, error)
	UpdateScale(ctx context.Context, obj client.Object, scale *autoscalingv1.Scale) error
}

type subResourceScaler struct {
	client client.Client
}

func (s *subResourceScaler) GetScale(ctx context.Context, obj client.Object) (*autoscalingv1.Scale, error) {
	scale := &autoscalingv1.Scale{}
	if err := s.client.SubResource("scale").Get(ctx, obj, scale); err != nil {
		return nil, err
	}
	return scale, nil
}

func (s *subResourceScaler) UpdateScale(ctx context.Context, obj client.Object, scale *autoscalingv1.Scale) error {
	return s.client.SubResource("scale").Update(ctx, obj, client.WithSubResourceBody(scale))
}

func getTarget(ctx context.Context, c client.Client, workload *v1alpha1.ManagedWorkload) (client.Object, error) {
	ref := workload.Spec.Target
	nn := types.NamespacedName{Name: ref.Name, Namespace: workload.Namespace}

	var obj client.Object
	switch ref.Kind {
	case v1alpha1.TargetKindDeployment:
		obj = &appsv1.Deployment{}
	case v1alpha1.TargetKindStatefulSet:
		obj = &appsv1.StatefulSet{}
	default:
		return nil, fmt.Errorf("unsupported kind: %s", ref.Kind)
	}

	if err := c.Get(ctx, nn, obj); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, ref.Name, err)
	}

	return obj, nil
}

// scaleTo sets the target's replicas to n, and returns its scale as of
// then, whose status still counts the pods from before.
func scaleTo(ctx context.Context, scaler WorkloadScaler, target client.Object, n int32) (*autoscalingv1.Scale, error) {
	scale, err := scaler.GetScale(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("getting scale subresource: %w", err)
	}

	if scale.Spec.Replicas == n {
		return scale, nil
	}

	scale.Spec.Replicas = n
	if err := scaler.UpdateScale(ctx, target, scale); err != nil {
		return nil, fmt.Errorf("updating scale to %d: %w", n, err)
	}
	return scale, nil
}

// readyOf re-reads the target and returns how many of the desired replicas
// it's scaled to are Ready. The scale subresource's status counts pods that
// exist, which includes pods still starting, such as a database replaying
// its log. A read that doesn't yet show the target scaled to desired, or a
// status its controller wrote before it saw that, counts none: what it
// counts are the pods from before, those of a pause just made, which may
// not have stopped yet.
func readyOf(ctx context.Context, c client.Client, target client.Object, desired int32) (int32, error) {
	if err := c.Get(ctx, client.ObjectKeyFromObject(target), target); err != nil {
		return 0, fmt.Errorf("getting %s: %w", target.GetName(), err)
	}
	if specReplicas(target) != desired || !observed(target) {
		return 0, nil
	}
	return min(ReadyReplicas(target), desired), nil
}

// defaultProgressDeadline is how long a Deployment's controller allows its
// pods to become available when it doesn't say, and how long a
// StatefulSet's, which can't say, is given.
const defaultProgressDeadline = 10 * time.Minute

// progressDeadline is how long the target's own controller gives its
// replicas to start before it reports them stuck.
func progressDeadline(target client.Object) time.Duration {
	if d, ok := target.(*appsv1.Deployment); ok && d.Spec.ProgressDeadlineSeconds != nil {
		return time.Duration(*d.Spec.ProgressDeadlineSeconds) * time.Second
	}
	return defaultProgressDeadline
}

// ReadyReplicas returns how many of the target's replicas are Ready.
func ReadyReplicas(target client.Object) int32 {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return t.Status.ReadyReplicas
	case *appsv1.StatefulSet:
		return t.Status.ReadyReplicas
	default:
		return 0
	}
}

// Pods returns how many pods the target has, Ready or not.
func Pods(target client.Object) int32 {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return t.Status.Replicas
	case *appsv1.StatefulSet:
		return t.Status.Replicas
	default:
		return 0
	}
}

// IsReady reports whether every replica the target asks for is Ready, and
// it asks for at least one, by a status written for its current spec.
func IsReady(target client.Object) bool {
	desired := specReplicas(target)
	return desired > 0 && observed(target) && ReadyReplicas(target) >= desired
}

// specReplicas is the replicas the target asks for, one when unset as the
// API server defaults it.
func specReplicas(target client.Object) int32 {
	var replicas *int32
	switch t := target.(type) {
	case *appsv1.Deployment:
		replicas = t.Spec.Replicas
	case *appsv1.StatefulSet:
		replicas = t.Spec.Replicas
	}
	if replicas == nil {
		return 1
	}
	return *replicas
}

// observed reports whether the target's status was written by its
// controller after it saw the current spec. A StatefulSet's revisions
// aren't compared: under the OnDelete strategy they never converge on
// their own, and a rollout still under way has Ready pods that serve.
func observed(target client.Object) bool {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return t.Status.ObservedGeneration >= t.Generation
	case *appsv1.StatefulSet:
		return t.Status.ObservedGeneration >= t.Generation
	default:
		return false
	}
}
