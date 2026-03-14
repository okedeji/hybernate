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

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func getTarget(ctx context.Context, c client.Client, workload *v1alpha1.ManagedWorkload) (*unstructured.Unstructured, error) {
	ref := workload.Spec.Target

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing api version %q: %w", ref.APIVersion, err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(ref.Kind))

	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: workload.Namespace}, obj); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, ref.Name, err)
	}

	return obj, nil
}

func scaleTo(ctx context.Context, scaler WorkloadScaler, target *unstructured.Unstructured, n int32) error {
	scale, err := scaler.GetScale(ctx, target)
	if err != nil {
		return fmt.Errorf("getting scale subresource: %w", err)
	}

	if scale.Spec.Replicas == n {
		return nil
	}

	scale.Spec.Replicas = n
	if err := scaler.UpdateScale(ctx, target, scale); err != nil {
		return fmt.Errorf("updating scale to %d: %w", n, err)
	}
	return nil
}

func checkReady(ctx context.Context, scaler WorkloadScaler, target *unstructured.Unstructured, desired int32) (bool, error) {
	scale, err := scaler.GetScale(ctx, target)
	if err != nil {
		return false, fmt.Errorf("getting scale subresource: %w", err)
	}
	return scale.Status.Replicas >= desired, nil
}
