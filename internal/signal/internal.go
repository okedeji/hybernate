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

package signal

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

type CompareMode int

const (
	Below CompareMode = iota
	Above
)

// MetricsReader reads current resource usage for a workload.
type MetricsReader interface {
	CPUUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error)
	MemoryUsage(ctx context.Context, workload *v1alpha1.ManagedWorkload) (resource.Quantity, error)
}

// Internal implements Checker by reading CPU from the K8s Metrics API
// and comparing against a threshold. Use Below for idle confirmation
// (CPU below threshold = confirm) and Above for scale-up confirmation
// (CPU above threshold = confirm).
type Internal struct {
	Metrics   MetricsReader
	Workload  *v1alpha1.ManagedWorkload
	Threshold resource.Quantity
	Mode      CompareMode
}

func NewInternal(metrics MetricsReader, workload *v1alpha1.ManagedWorkload, threshold resource.Quantity, mode CompareMode) *Internal {
	return &Internal{
		Metrics:   metrics,
		Workload:  workload,
		Threshold: threshold,
		Mode:      mode,
	}
}

func (i *Internal) Check(ctx context.Context, namespace, name string) (Result, error) {
	cpu, err := i.Metrics.CPUUsage(ctx, i.Workload)
	if err != nil {
		return Result{}, fmt.Errorf("reading cpu usage for %s/%s: %w", namespace, name, err)
	}
	return compareQuantity("cpu", cpu, i.Threshold, i.Mode)
}

// MemoryInternal implements Checker by reading memory from the K8s Metrics API
// and comparing against a threshold.
type MemoryInternal struct {
	Metrics   MetricsReader
	Workload  *v1alpha1.ManagedWorkload
	Threshold resource.Quantity
	Mode      CompareMode
}

func NewMemoryInternal(metrics MetricsReader, workload *v1alpha1.ManagedWorkload, threshold resource.Quantity, mode CompareMode) *MemoryInternal {
	return &MemoryInternal{
		Metrics:   metrics,
		Workload:  workload,
		Threshold: threshold,
		Mode:      mode,
	}
}

func (m *MemoryInternal) Check(ctx context.Context, namespace, name string) (Result, error) {
	mem, err := m.Metrics.MemoryUsage(ctx, m.Workload)
	if err != nil {
		return Result{}, fmt.Errorf("reading memory usage for %s/%s: %w", namespace, name, err)
	}
	return compareQuantity("memory", mem, m.Threshold, m.Mode)
}

func compareQuantity(resource string, usage, threshold resource.Quantity, mode CompareMode) (Result, error) {
	cmp := usage.Cmp(threshold)

	switch mode {
	case Below:
		if cmp <= 0 {
			return Result{
				Confirm: true,
				Reason:  fmt.Sprintf("%s %s <= threshold %s", resource, &usage, &threshold),
			}, nil
		}
		return Result{
			Confirm: false,
			Reason:  fmt.Sprintf("%s %s > threshold %s", resource, &usage, &threshold),
		}, nil
	case Above:
		if cmp > 0 {
			return Result{
				Confirm: true,
				Reason:  fmt.Sprintf("%s %s > threshold %s", resource, &usage, &threshold),
			}, nil
		}
		return Result{
			Confirm: false,
			Reason:  fmt.Sprintf("%s %s <= threshold %s", resource, &usage, &threshold),
		}, nil
	default:
		return Result{}, fmt.Errorf("unknown compare mode %d", mode)
	}
}
