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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

const maxDiscovered = 500

// Scanner lists workloads in a namespace and classifies them.
type Scanner struct {
	client client.Client
}

// NewScanner creates a Scanner backed by the given client.
func NewScanner(c client.Client) *Scanner {
	return &Scanner{client: c}
}

// ScanResult holds the output of a namespace scan.
type ScanResult struct {
	Discovered []v1alpha1.DiscoveredWorkload
	Summary    v1alpha1.DiscoverySummary
}

// Scan lists workloads of the given kinds in the namespace, classifies each,
// and returns the results capped at maxDiscovered sorted by savings descending.
func (s *Scanner) Scan(ctx context.Context, namespace string, kinds []string, th Thresholds) (*ScanResult, error) {
	managed, err := s.managedTargets(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("listing managed workloads: %w", err)
	}

	var all []v1alpha1.DiscoveredWorkload
	var totalSavings float64

	for _, kind := range kinds {
		workloads, err := s.listWorkloads(ctx, namespace, kind)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", kind, err)
		}

		for _, obj := range workloads {
			info, err := s.buildInfo(ctx, namespace, kind, obj, managed)
			if err != nil {
				continue
			}
			if info.Ignored {
				continue
			}

			d := BuildDiscovered(info, th)
			savings := EstimateSavings(info, d.Classification, th)
			totalSavings += savings
			all = append(all, d)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].EstimatedSavings > all[j].EstimatedSavings
	})
	if len(all) > maxDiscovered {
		all = all[:maxDiscovered]
	}

	summary := buildSummary(all, totalSavings)
	return &ScanResult{Discovered: all, Summary: summary}, nil
}

func (s *Scanner) listWorkloads(ctx context.Context, namespace, kind string) ([]unstructured.Unstructured, error) {
	gvk := gvkForKind(kind)
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)
	if err := s.client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s *Scanner) buildInfo(ctx context.Context, namespace, kind string, obj unstructured.Unstructured, managed map[string]bool) (WorkloadInfo, error) {
	name := obj.GetName()
	info := WorkloadInfo{
		Name:    name,
		Kind:    kind,
		Managed: managed[kind+"/"+name],
		Ignored: obj.GetLabels()[v1alpha1.LabelIgnore] == "true",
	}

	replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if found {
		info.Replicas = int32(replicas)
	} else {
		info.Replicas = 1
	}

	info.CPURequestMillis, info.MemoryRequestBytes = requestsFromTemplate(obj)

	sel := selectorFromWorkload(obj)
	if sel != nil {
		info.CPUUsageMillis, info.MemoryUsageBytes = s.podMetrics(ctx, namespace, sel)
		info.StorageBytes = s.pvcBytes(ctx, namespace, sel)
	}

	return info, nil
}

// managedTargets returns a set of "Kind/Name" strings for workloads already
// covered by a ManagedWorkload CR in this namespace.
func (s *Scanner) managedTargets(ctx context.Context, namespace string) (map[string]bool, error) {
	var list v1alpha1.ManagedWorkloadList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(list.Items))
	for _, mw := range list.Items {
		key := mw.Spec.Target.Kind + "/" + mw.Spec.Target.Name
		m[key] = true
	}
	return m, nil
}

func (s *Scanner) podMetrics(ctx context.Context, namespace string, sel labels.Selector) (cpuMillis, memBytes int64) {
	var podMetrics metricsv1beta1.PodMetricsList
	if err := s.client.List(ctx, &podMetrics, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0, 0
	}
	for _, pm := range podMetrics.Items {
		for _, c := range pm.Containers {
			cpuMillis += c.Usage.Cpu().MilliValue()
			memBytes += c.Usage.Memory().Value()
		}
	}
	return cpuMillis, memBytes
}

func (s *Scanner) pvcBytes(ctx context.Context, namespace string, sel labels.Selector) int64 {
	var pvcs corev1.PersistentVolumeClaimList
	if err := s.client.List(ctx, &pvcs, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0
	}
	var total int64
	for _, pvc := range pvcs.Items {
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			total += q.Value()
		}
	}
	return total
}

func requestsFromTemplate(obj unstructured.Unstructured) (cpuMillis, memBytes int64) {
	containers, found, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if !found {
		return 0, 0
	}
	for _, c := range containers {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		res, found, _ := unstructured.NestedMap(cMap, "resources", "requests")
		if !found {
			continue
		}
		if cpu, ok := res["cpu"]; ok {
			cpuMillis += parseMilliCPU(fmt.Sprintf("%v", cpu))
		}
		if mem, ok := res["memory"]; ok {
			memBytes += parseMemoryBytes(fmt.Sprintf("%v", mem))
		}
	}
	return cpuMillis, memBytes
}

func selectorFromWorkload(obj unstructured.Unstructured) labels.Selector {
	matchLabels, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if !found || len(matchLabels) == 0 {
		return nil
	}
	return labels.SelectorFromSet(matchLabels)
}

func gvkForKind(kind string) schema.GroupVersionKind {
	switch kind {
	case "Deployment":
		return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"}
	case "StatefulSet":
		return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSetList"}
	default:
		return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind + "List"}
	}
}

func buildSummary(discovered []v1alpha1.DiscoveredWorkload, totalSavings float64) v1alpha1.DiscoverySummary {
	var s v1alpha1.DiscoverySummary
	s.Total = len(discovered)
	for _, d := range discovered {
		switch d.Classification {
		case v1alpha1.ClassificationActive:
			s.Active++
		case v1alpha1.ClassificationIdle:
			s.Idle++
		case v1alpha1.ClassificationWasteful:
			s.Wasteful++
		}
		if d.Managed {
			s.Managed++
		}
	}
	s.EstimatedMonthlySavings = cost.FormatDollars(totalSavings)
	return s
}

// parseMilliCPU converts a Kubernetes CPU string (e.g., "500m", "1") to millicores.
func parseMilliCPU(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

// parseMemoryBytes converts a Kubernetes memory string (e.g., "512Mi", "1Gi") to bytes.
func parseMemoryBytes(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}
