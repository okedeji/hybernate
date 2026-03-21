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

package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

const bytesPerGiB = 1024 * 1024 * 1024

func (r *Reconciler) accumulateCost(ctx context.Context, workload *v1alpha1.ManagedWorkload) {
	if r.metrics == nil {
		return
	}

	logger := log.FromContext(ctx)
	now := r.now()
	rates := resolveCostRates(workload)

	if workload.Status.Cost == nil {
		workload.Status.Cost = &v1alpha1.CostStatus{}
	}

	// Monthly reset.
	if workload.Status.Cost.LastAccumulatedAt != nil &&
		workload.Status.Cost.LastAccumulatedAt.Month() != now.Month() {
		workload.Status.Cost = &v1alpha1.CostStatus{}
	}

	elapsed := time.Duration(0)
	if workload.Status.Cost.LastAccumulatedAt != nil {
		elapsed = now.Sub(workload.Status.Cost.LastAccumulatedAt.Time)
	}

	snap := cost.Snapshot{
		CPUHours:     workload.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64(),
		MemoryHours:  workload.Status.Cost.CurrentMonthMemoryHours.AsApproximateFloat64(),
		StorageHours: workload.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64(),
		EstimatedSavedCost: parseDollarAmount(workload.Status.Cost.EstimatedMonthlySavings),
	}

	phase := workload.Status.Phase

	switch phase {
	case v1alpha1.PhasePaused:
		// No compute usage, but PVCs still cost.
		storageGiB := float64(0)
		if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
			storageGiB = float64(workload.Status.Pause.Resources.StorageBytes) / bytesPerGiB
		}
		snap = cost.Accumulate(snap, 0, 0, storageGiB, elapsed)

		// Savings: what compute would have cost if still running.
		if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
			rs := workload.Status.Pause.Resources
			replicas := float64(rs.Replicas)
			cpuCores := replicas * float64(rs.CPUMillis) / 1000
			memGiB := replicas * float64(rs.MemoryBytes) / bytesPerGiB
			snap = cost.AccumulateSavings(snap, cpuCores, memGiB, 0, elapsed, rates)
		}

	case v1alpha1.PhaseDestroyed:
		// Target is deleted — use the snapshot captured at destroy time.
		if workload.Status.Destroy != nil && workload.Status.Destroy.Resources != nil {
			rs := workload.Status.Destroy.Resources
			replicas := float64(rs.Replicas)
			cpuCores := replicas * float64(rs.CPUMillis) / 1000
			memGiB := replicas * float64(rs.MemoryBytes) / bytesPerGiB
			storageGiB := float64(rs.StorageBytes) / bytesPerGiB

			pvcsCleanedUp := workload.Status.Destroy.PVCRetentionExpiresAt == nil ||
				!now.Before(workload.Status.Destroy.PVCRetentionExpiresAt.Time)

			// Still paying for storage while PVCs are retained.
			if !pvcsCleanedUp {
				snap = cost.Accumulate(snap, 0, 0, storageGiB, elapsed)
			}

			// Compute is always saved. Storage saved only after PVCs cleaned up.
			savedStorage := float64(0)
			if pvcsCleanedUp {
				savedStorage = storageGiB
			}
			snap = cost.AccumulateSavings(snap, cpuCores, memGiB, savedStorage, elapsed, rates)
		}

	default:
		// Running/Idle/Scaling — accumulate actual usage.
		cpuMillis, err := r.metrics.TotalCPUMillis(ctx, workload)
		if err != nil {
			logger.V(1).Info("skipping cpu cost accumulation", "error", err)
			return
		}
		memBytes, err := r.metrics.TotalMemoryBytes(ctx, workload)
		if err != nil {
			logger.V(1).Info("skipping memory cost accumulation", "error", err)
			memBytes = 0
		}
		pvcBytes, err := r.metrics.TotalPVCBytes(ctx, workload)
		if err != nil {
			logger.V(1).Info("skipping pvc cost accumulation", "error", err)
			pvcBytes = 0
		}

		cpuCores := cpuMillis / 1000
		memGiB := memBytes / bytesPerGiB
		storageGiB := pvcBytes / bytesPerGiB

		snap = cost.Accumulate(snap, cpuCores, memGiB, storageGiB, elapsed)
	}

	// Write back to status.
	nowMeta := metav1.NewTime(now)
	workload.Status.Cost.CurrentMonthCPUHours = *resource.NewMilliQuantity(int64(snap.CPUHours*1000), resource.DecimalSI)
	workload.Status.Cost.CurrentMonthMemoryHours = *resource.NewMilliQuantity(int64(snap.MemoryHours*1000), resource.DecimalSI)
	workload.Status.Cost.CurrentMonthStorageHours = *resource.NewMilliQuantity(int64(snap.StorageHours*1000), resource.DecimalSI)
	workload.Status.Cost.EstimatedMonthlySavings = cost.FormatDollars(snap.EstimatedSavedCost)
	workload.Status.Cost.LastAccumulatedAt = &nowMeta

	estimate := cost.EstimateMonthlyCost(snap, rates, now.Day(), daysInMonth(now))
	if estimate < 0 {
		workload.Status.Cost.EstimatedMonthlyCost = "pending"
	} else {
		workload.Status.Cost.EstimatedMonthlyCost = cost.FormatDollars(estimate)
	}

	workload.Status.Cost.EstimatedCostWithoutManagement = cost.FormatDollars(cost.EstimatedCostWithoutManagement(snap, rates))

	// Populate resource reduction from the snapshot captured at pause/destroy time.
	switch phase {
	case v1alpha1.PhasePaused:
		if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
			rs := workload.Status.Pause.Resources
			workload.Status.Cost.ResourceReduction = &v1alpha1.ResourceReduction{
				CPUMillis:   rs.CPUMillis * int64(rs.Replicas),
				MemoryBytes: rs.MemoryBytes * int64(rs.Replicas),
				Replicas:    rs.Replicas,
			}
		}
	case v1alpha1.PhaseDestroyed:
		if workload.Status.Destroy != nil && workload.Status.Destroy.Resources != nil {
			rs := workload.Status.Destroy.Resources
			workload.Status.Cost.ResourceReduction = &v1alpha1.ResourceReduction{
				CPUMillis:   rs.CPUMillis * int64(rs.Replicas),
				MemoryBytes: rs.MemoryBytes * int64(rs.Replicas),
				Replicas:    rs.Replicas,
			}
		}
	default:
		workload.Status.Cost.ResourceReduction = nil
	}
}

func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
}

func (r *Reconciler) captureResourceSnapshot(ctx context.Context, workload *v1alpha1.ManagedWorkload) *v1alpha1.ResourceSnapshot {
	// If the workload was paused, pods are already gone — reuse the snapshot
	// captured at pause time rather than querying dead metrics.
	if workload.Status.Pause != nil && workload.Status.Pause.Resources != nil {
		rs := *workload.Status.Pause.Resources
		return &rs
	}

	if r.metrics == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	snap := &v1alpha1.ResourceSnapshot{Replicas: 1}

	target, err := r.checkTarget(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not fetch target for resource snapshot", "error", err)
	} else if target != nil {
		snap.Replicas = replicasFromTarget(target)
	}

	cpuMillis, err := r.metrics.CPURequestPerReplica(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture cpu for resource snapshot", "error", err)
	} else {
		snap.CPUMillis = int64(cpuMillis)
	}

	memBytes, err := r.metrics.TotalMemoryBytes(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture memory for resource snapshot", "error", err)
	} else {
		snap.MemoryBytes = int64(memBytes) / int64(snap.Replicas)
	}

	pvcBytes, err := r.metrics.TotalPVCBytes(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture pvc for resource snapshot", "error", err)
	} else {
		snap.StorageBytes = int64(pvcBytes)
	}

	return snap
}
