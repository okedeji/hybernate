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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
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

	var total float64
	for _, c := range containersFromTarget(target) {
		if cpu := c.Resources.Requests.Cpu(); cpu != nil {
			total += float64(cpu.MilliValue())
		}
	}

	if total == 0 {
		return 0, fmt.Errorf("no cpu requests found in pod template for %s %s", workload.Spec.Target.Kind, workload.Spec.Target.Name)
	}

	return total, nil
}

// TotalMemoryBytes returns aggregate memory usage in bytes across all pods
// for the workload.
func (r *Reader) TotalMemoryBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
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
			total += float64(container.Usage.Memory().Value())
		}
	}
	return total, nil
}

// TotalPVCBytes returns the total provisioned PVC capacity in bytes for
// the workload by listing PVCs matching the target's selector labels.
func (r *Reader) TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}

	selector, err := selectorFromTarget(target)
	if err != nil {
		return 0, err
	}

	var pvcList corev1.PersistentVolumeClaimList
	err = r.client.List(ctx, &pvcList,
		client.InNamespace(workload.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	)
	if err != nil {
		return 0, fmt.Errorf("listing pvcs for %s/%s: %w", workload.Namespace, workload.Spec.Target.Name, err)
	}

	var total float64
	for _, pvc := range pvcList.Items {
		if cap, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			total += float64(cap.Value())
		}
	}
	return total, nil
}

func (r *Reader) getTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (client.Object, error) {
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

	if err := r.client.Get(ctx, nn, obj); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, ref.Name, err)
	}

	return obj, nil
}

func containersFromTarget(obj client.Object) []corev1.Container {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		return t.Spec.Template.Spec.Containers
	case *appsv1.StatefulSet:
		return t.Spec.Template.Spec.Containers
	default:
		return nil
	}
}

func selectorFromTarget(obj client.Object) (labels.Selector, error) {
	var matchLabels map[string]string
	switch t := obj.(type) {
	case *appsv1.Deployment:
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	case *appsv1.StatefulSet:
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	}
	if len(matchLabels) == 0 {
		return nil, fmt.Errorf("no selector found on %s %s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	}
	return labels.SelectorFromSet(matchLabels), nil
}
