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

package metrics

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Reader reads workload metrics from the Kubernetes Metrics API.
// All methods aggregate across every pod belonging to the workload
// using the target's spec.selector.matchLabels to find pods.
type Reader struct {
	client client.Client
}

func NewReader(c client.Client) *Reader {
	return &Reader{client: c}
}

// CPUUsage returns aggregate CPU usage across all pods for a workload.
func (r *Reader) CPUUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error) {
	millis, err := r.TotalCPUMillis(ctx, workload)
	if err != nil {
		return resource.Quantity{}, err
	}
	return *resource.NewMilliQuantity(int64(millis), resource.DecimalSI), nil
}

// TotalCPUMillis returns aggregate CPU in millicores across all pods
// for the workload.
func (r *Reader) TotalCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}

	selector, err := selectorFromTarget(target)
	if err != nil {
		return 0, err
	}

	var podMetrics metricsv1beta1.PodMetricsList
	err = r.client.List(ctx, &podMetrics,
		client.InNamespace(workload.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	)
	if err != nil {
		return 0, fmt.Errorf("listing pod metrics for %s/%s: %w", workload.Namespace, workload.Spec.Target.Name, err)
	}

	if len(podMetrics.Items) == 0 {
		return 0, fmt.Errorf("no pod metrics found for %s/%s", workload.Namespace, workload.Spec.Target.Name)
	}

	var total float64
	for _, pod := range podMetrics.Items {
		for _, container := range pod.Containers {
			total += float64(container.Usage.Cpu().MilliValue())
		}
	}
	return total, nil
}

// CPURequestPerReplica returns the total CPU request (in millicores) for one
// replica by summing requests across all containers in the pod template.
func (r *Reader) CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}

	containers, found, err := unstructured.NestedSlice(target.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return 0, fmt.Errorf("reading containers from %s %s", workload.Spec.Target.Kind, workload.Spec.Target.Name)
	}

	var total float64
	for _, c := range containers {
		container, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		resources, found, err := unstructured.NestedMap(container, "resources", "requests")
		if err != nil || !found {
			continue
		}
		cpuStr, ok := resources["cpu"]
		if !ok {
			continue
		}
		q, err := resource.ParseQuantity(fmt.Sprintf("%v", cpuStr))
		if err != nil {
			continue
		}
		total += float64(q.MilliValue())
	}

	if total == 0 {
		return 0, fmt.Errorf("no cpu requests found in pod template for %s %s", workload.Spec.Target.Kind, workload.Spec.Target.Name)
	}

	return total, nil
}

func (r *Reader) getTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*unstructured.Unstructured, error) {
	ref := workload.Spec.Target

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing api version %q: %w", ref.APIVersion, err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(ref.Kind))

	if err := r.client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: workload.Namespace}, obj); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, ref.Name, err)
	}

	return obj, nil
}

func selectorFromTarget(target *unstructured.Unstructured) (labels.Selector, error) {
	matchLabels, found, err := unstructured.NestedStringMap(target.Object, "spec", "selector", "matchLabels")
	if err != nil || !found || len(matchLabels) == 0 {
		return nil, fmt.Errorf("reading selector from %s %s", target.GetKind(), target.GetName())
	}
	return labels.SelectorFromSet(matchLabels), nil
}
