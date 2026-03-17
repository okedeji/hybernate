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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
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
func (s *Scanner) Scan(ctx context.Context, namespace string, kinds []v1alpha1.TargetKind, th Thresholds) (*ScanResult, error) {
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
			info := s.buildInfo(ctx, namespace, kind, obj, managed)
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

func (s *Scanner) buildInfo(ctx context.Context, namespace string, kind v1alpha1.TargetKind, obj client.Object, managed map[string]bool) WorkloadInfo {
	name := obj.GetName()
	info := WorkloadInfo{
		Name:    name,
		Kind:    kind,
		Managed: managed[string(kind)+"/"+name],
		Ignored: obj.GetLabels()[v1alpha1.LabelIgnore] == "true",
	}

	replicas, containers, matchLabels := workloadFields(obj)

	if replicas != nil {
		info.Replicas = *replicas
	} else {
		info.Replicas = 1
	}

	for _, c := range containers {
		if cpu := c.Resources.Requests.Cpu(); cpu != nil {
			info.CPURequestMillis += cpu.MilliValue()
		}
		if mem := c.Resources.Requests.Memory(); mem != nil {
			info.MemoryRequestBytes += mem.Value()
		}
	}

	if len(matchLabels) > 0 {
		sel := labels.SelectorFromSet(matchLabels)
		info.CPUUsageMillis, info.MemoryUsageBytes = s.podMetrics(ctx, namespace, sel)
		info.StorageBytes = s.pvcBytes(ctx, namespace, sel)
	}

	return info
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
		key := string(mw.Spec.Target.Kind) + "/" + mw.Spec.Target.Name
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

// workloadFields extracts the common fields from a Deployment or StatefulSet.
func workloadFields(obj client.Object) (replicas *int32, containers []corev1.Container, matchLabels map[string]string) {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		replicas = t.Spec.Replicas
		containers = t.Spec.Template.Spec.Containers
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	case *appsv1.StatefulSet:
		replicas = t.Spec.Replicas
		containers = t.Spec.Template.Spec.Containers
		if t.Spec.Selector != nil {
			matchLabels = t.Spec.Selector.MatchLabels
		}
	}
	return
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
