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

package export

import (
	"fmt"
	"strings"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Filter struct {
	Classifications []v1alpha1.Classification
	Name            string
	IncludeManaged  bool
}

type Result struct {
	Workloads []v1alpha1.ManagedWorkload
	Skipped   []SkippedWorkload
}

type SkippedWorkload struct {
	Name   string
	Kind   v1alpha1.TargetKind
	Reason string
}

// Generate builds ManagedWorkload specs from a WorkloadPolicy's discovered
// workloads. Callers control which workloads are included via Filter.
// Ignored workloads are always skipped. Managed workloads are skipped by
// default unless IncludeManaged is set.
// When no classifications are specified, all classifications are included.
func Generate(policy *v1alpha1.WorkloadPolicy, f Filter) Result {
	allowed := make(map[v1alpha1.Classification]bool, len(f.Classifications))
	for _, c := range f.Classifications {
		allowed[c] = true
	}

	var result Result

	for _, d := range policy.Status.Discovered {
		if d.Ignored {
			result.Skipped = append(result.Skipped, SkippedWorkload{
				Name:   d.Name,
				Kind:   d.Kind,
				Reason: "ignored (hybernate.io/ignore label)",
			})
			continue
		}
		if d.Managed && !f.IncludeManaged {
			result.Skipped = append(result.Skipped, SkippedWorkload{
				Name:   d.Name,
				Kind:   d.Kind,
				Reason: "already has a ManagedWorkload",
			})
			continue
		}
		if f.Name != "" && d.Name != f.Name {
			continue
		}
		if len(allowed) > 0 && !allowed[d.Classification] {
			continue
		}

		mw := v1alpha1.ManagedWorkload{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "ManagedWorkload",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      ResourceName(d.Kind, d.Name),
				Namespace: policy.Namespace,
				Labels: map[string]string{
					v1alpha1.LabelAutoDiscovered: "true",
				},
				Annotations: map[string]string{
					v1alpha1.AnnotationWorkloadPolicy: policy.Name,
				},
			},
			Spec: v1alpha1.ManagedWorkloadSpec{
				Target: v1alpha1.WorkloadRef{
					Kind: d.Kind,
					Name: d.Name,
				},
				DryRun: policy.Spec.DryRun,
				Prediction: func() v1alpha1.PredictionSpec {
					if policy.Spec.Prediction != nil {
						return *policy.Spec.Prediction
					}
					return v1alpha1.PredictionSpec{}
				}(),
				IdlePolicy:     policy.Spec.IdlePolicy,
				ScalePolicy:    policy.Spec.ScalePolicy,
				Pause:          policy.Spec.Pause,
				Destroy:        policy.Spec.Destroy,
				CostTracking:   policy.Spec.CostTracking,
				ConflictAction: policy.Spec.ConflictAction,
			},
		}

		result.Workloads = append(result.Workloads, mw)
	}

	return result
}

// ResourceName produces the ManagedWorkload name for a given target.
// Exported so the controller's autoManage uses the same logic.
func ResourceName(kind v1alpha1.TargetKind, name string) string {
	return strings.ToLower(fmt.Sprintf("%s-%s", kind, name))
}
