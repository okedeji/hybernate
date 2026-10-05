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
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// hoursPerMonth is the average month, for monthly costs.
const hoursPerMonth = 730

// pageSize bounds each list the scan makes, so a namespace with thousands of
// pods is read in pieces the API server can serve without strain.
const pageSize = 500

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

// listAll lists a page at a time, handing each page to each. Every page is
// decoded into a fresh list, since decoding into a used one merges maps such
// as labels from one page's items into the next's.
func listAll[T any, L interface {
	*T
	client.ObjectList
}](ctx context.Context, r client.Reader, each func(L), opts ...client.ListOption) error {
	var next string
	for {
		pageOpts := append(slices.Clip(opts), client.Limit(pageSize))
		if next != "" {
			pageOpts = append(pageOpts, client.Continue(next))
		}
		list := L(new(T))
		if err := r.List(ctx, list, pageOpts...); err != nil {
			return err
		}
		each(list)
		if next = list.GetContinue(); next == "" {
			return nil
		}
	}
}

// workload is the part of a Deployment or StatefulSet the scan reads.
type workload struct {
	obj      client.Object
	kind     v1alpha1.TargetKind
	replicas *int32
	template corev1.PodTemplateSpec
	selector *metav1.LabelSelector
}

func deploymentWorkload(d *appsv1.Deployment) workload {
	return workload{obj: d, kind: v1alpha1.TargetKindDeployment, replicas: d.Spec.Replicas,
		template: d.Spec.Template, selector: d.Spec.Selector}
}

func statefulSetWorkload(s *appsv1.StatefulSet) workload {
	return workload{obj: s, kind: v1alpha1.TargetKindStatefulSet, replicas: s.Spec.Replicas,
		template: s.Spec.Template, selector: s.Spec.Selector}
}

// podSelector is the selector the workload's controller adopts pods by,
// match expressions included. A missing or empty one selects nothing, as
// the API server rejects one for a Deployment or StatefulSet anyway.
func (w workload) podSelector() (labels.Selector, error) {
	sel, err := metav1.LabelSelectorAsSelector(w.selector)
	if err != nil {
		return nil, err
	}
	if sel.Empty() {
		return labels.Nothing(), nil
	}
	return sel, nil
}
