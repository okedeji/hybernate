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

package discovery

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// hoursPerMonth is the average month, for monthly costs.
const hoursPerMonth = 730

// Scanner reads a cluster's workloads and judges whether they're in use.
type Scanner struct {
	client client.Client
	pods   client.Reader
}

// NewScanner creates a Scanner backed by the given client, reading pods
// through pods so they needn't be cached.
func NewScanner(c client.Client, pods client.Reader) *Scanner {
	return &Scanner{client: c, pods: pods}
}

func (s *Scanner) listWorkloads(ctx context.Context, namespace string, kind v1alpha1.TargetKind) ([]client.Object, error) {
	switch kind {
	case v1alpha1.TargetKindDeployment:
		var list appsv1.DeploymentList
		if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		out := make([]client.Object, len(list.Items))
		for i := range list.Items {
			out[i] = &list.Items[i]
		}
		return out, nil
	case v1alpha1.TargetKindStatefulSet:
		var list appsv1.StatefulSetList
		if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		out := make([]client.Object, len(list.Items))
		for i := range list.Items {
			out[i] = &list.Items[i]
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", kind)
	}
}

// workloadPods are the workload's pods, or none when they can't be listed,
// which prices it from its template at the scan's rates.
func (s *Scanner) workloadPods(ctx context.Context, namespace string, sel labels.Selector) []corev1.Pod {
	var pods corev1.PodList
	if err := s.pods.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil
	}
	return pods.Items
}

// workloadFields extracts the common fields from a Deployment or StatefulSet.
func workloadFields(obj client.Object) (replicas *int32, spec corev1.PodSpec, matchLabels map[string]string) {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		replicas = t.Spec.Replicas
		spec = t.Spec.Template.Spec
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	case *appsv1.StatefulSet:
		replicas = t.Spec.Replicas
		spec = t.Spec.Template.Spec
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	}
	return
}

func podTemplate(obj client.Object) corev1.PodTemplateSpec {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		return t.Spec.Template
	case *appsv1.StatefulSet:
		return t.Spec.Template
	}
	return corev1.PodTemplateSpec{}
}
