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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func costWorkload(phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Prediction:   v1alpha1.PredictionSpec{Confidence: 85},
			CostTracking: &v1alpha1.CostTrackingSpec{},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
}

func costReconciler(now time.Time, metrics *stubMetrics) *Reconciler {
	r := &Reconciler{
		clock: func() time.Time { return now },
	}
	if metrics != nil {
		r.metrics = metrics
	}
	return r
}

func TestAccumulateCost_SkipsWhenNoMetrics(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)

	r := costReconciler(fixedTime, nil)
	r.accumulateCost(context.Background(), w)

	assert.Nil(t, w.Status.Cost)
}

func TestAccumulateCost_InitializesCostStatus(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	m := &stubMetrics{cpuMillis: 2000, memoryBytes: 2 * bytesPerGiB, pvcBytes: 10 * bytesPerGiB}

	r := costReconciler(fixedTime, m)
	r.accumulateCost(context.Background(), w)

	require.NotNil(t, w.Status.Cost)
	assert.NotNil(t, w.Status.Cost.LastAccumulatedAt)
}

func TestAccumulateCost_RunningAccumulatesUsage(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}

	m := &stubMetrics{
		cpuMillis:   2000,
		memoryBytes: 4 * bytesPerGiB,
		pvcBytes:    10 * bytesPerGiB,
	}

	r := costReconciler(fixedTime, m)
	r.accumulateCost(context.Background(), w)

	cpuHours := w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64()
	memHours := w.Status.Cost.CurrentMonthMemoryHours.AsApproximateFloat64()
	storageHours := w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()

	assert.InDelta(t, 2.0, cpuHours, 0.01, "2000m = 2 cores × 1h = 2 cpu-hours")
	assert.InDelta(t, 4.0, memHours, 0.01, "4 GiB × 1h = 4 mem-hours")
	assert.InDelta(t, 10.0, storageHours, 0.01, "10 GiB × 1h = 10 storage-hours")
}

func TestAccumulateCost_PausedAccumulatesStorageAndSavings(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}
	w.Status.Pause = &v1alpha1.PauseStatus{
		PreviousReplicas: 3,
		Resources: &v1alpha1.ResourceSnapshot{
			Replicas:     3,
			CPUMillis:    500,
			MemoryBytes:  2 * bytesPerGiB,
			StorageBytes: 20 * bytesPerGiB,
		},
	}

	r := costReconciler(fixedTime, &stubMetrics{})
	r.accumulateCost(context.Background(), w)

	cpuHours := w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64()
	storageHours := w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()

	assert.InDelta(t, 0.0, cpuHours, 0.001, "paused workload has no compute")
	assert.InDelta(t, 20.0, storageHours, 0.01, "PVCs still cost while paused")

	savings := parseDollarAmount(w.Status.Cost.MonthlySavings)
	assert.Greater(t, savings, 0.0, "should show savings from paused compute")
}

func TestAccumulateCost_DestroyedWithRetainedPVCs(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)
	pvcExpiry := metav1.NewTime(fixedTime.Add(24 * time.Hour))

	w := costWorkload(v1alpha1.PhaseDestroyed)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}
	w.Status.Destroy = &v1alpha1.DestroyStatus{
		Resources: &v1alpha1.ResourceSnapshot{
			Replicas:     2,
			CPUMillis:    1000,
			MemoryBytes:  4 * bytesPerGiB,
			StorageBytes: 50 * bytesPerGiB,
		},
		PVCRetentionExpiresAt: &pvcExpiry,
	}

	r := costReconciler(fixedTime, &stubMetrics{})
	r.accumulateCost(context.Background(), w)

	storageHours := w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()
	assert.InDelta(t, 50.0, storageHours, 0.01, "retained PVCs still cost")

	savings := parseDollarAmount(w.Status.Cost.MonthlySavings)
	assert.Greater(t, savings, 0.0, "compute savings from destroyed workload")
}

func TestAccumulateCost_DestroyedPVCsCleanedUp(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)
	pvcExpiry := metav1.NewTime(fixedTime.Add(-1 * time.Hour))

	w := costWorkload(v1alpha1.PhaseDestroyed)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}
	w.Status.Destroy = &v1alpha1.DestroyStatus{
		Resources: &v1alpha1.ResourceSnapshot{
			Replicas:     2,
			CPUMillis:    1000,
			MemoryBytes:  4 * bytesPerGiB,
			StorageBytes: 50 * bytesPerGiB,
		},
		PVCRetentionExpiresAt: &pvcExpiry,
	}

	r := costReconciler(fixedTime, &stubMetrics{})
	r.accumulateCost(context.Background(), w)

	storageHours := w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()
	assert.InDelta(t, 0.0, storageHours, 0.001, "PVCs cleaned up, no storage cost")

	savings := parseDollarAmount(w.Status.Cost.MonthlySavings)
	assert.Greater(t, savings, 0.0, "savings include storage after cleanup")
}

func TestAccumulateCost_MonthlyReset(t *testing.T) {
	lastMonth := time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC)
	lastMeta := metav1.NewTime(lastMonth)

	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(50000, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(30000, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(10000, resource.DecimalSI),
		MonthlySavings:           "$42.00",
		LastAccumulatedAt:        &lastMeta,
	}

	r := costReconciler(fixedTime, &stubMetrics{cpuMillis: 1000})
	r.accumulateCost(context.Background(), w)

	cpuHours := w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64()
	assert.Less(t, cpuHours, 1.0, "should have reset, not 50+ hours")
}

func TestAccumulateCost_EstimatedCostPendingDay1(t *testing.T) {
	day1 := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	w := costWorkload(v1alpha1.PhaseRunning)

	r := costReconciler(day1, &stubMetrics{cpuMillis: 1000})
	r.accumulateCost(context.Background(), w)

	assert.Equal(t, "pending", w.Status.Cost.EstimatedMonthlyCost)
}

func TestAccumulateCost_CostWithoutManagement(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}
	w.Status.Pause = &v1alpha1.PauseStatus{
		PreviousReplicas: 2,
		Resources: &v1alpha1.ResourceSnapshot{
			Replicas:     2,
			CPUMillis:    1000,
			MemoryBytes:  2 * bytesPerGiB,
			StorageBytes: 10 * bytesPerGiB,
		},
	}

	r := costReconciler(fixedTime, &stubMetrics{})
	r.accumulateCost(context.Background(), w)

	costWithout := parseDollarAmount(w.Status.Cost.CostWithoutManagement)
	assert.Greater(t, costWithout, 0.0, "should show what it would cost unmanaged")
}

func TestAccumulateCost_CustomRates(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhaseRunning)
	cpuRate := resource.MustParse("0.1")
	w.Spec.CostTracking.Rates = &v1alpha1.CostRates{
		CPUPerHour: &cpuRate,
	}
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}

	m := &stubMetrics{cpuMillis: 1000}
	r := costReconciler(fixedTime, m)
	r.accumulateCost(context.Background(), w)

	costWithout := parseDollarAmount(w.Status.Cost.CostWithoutManagement)
	assert.Greater(t, costWithout, 0.0)

	// With default rate ($0.031/cpu-hour) 1 core × 1h = $0.031
	// With custom rate ($0.1/cpu-hour) 1 core × 1h = $0.10
	// So custom rate should produce a higher cost.
	wDefault := costWorkload(v1alpha1.PhaseRunning)
	wDefault.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		MonthlySavings:           "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}

	r2 := costReconciler(fixedTime, m)
	r2.accumulateCost(context.Background(), wDefault)

	defaultCost := parseDollarAmount(wDefault.Status.Cost.CostWithoutManagement)
	assert.Greater(t, costWithout, defaultCost, "custom rate ($0.10) should cost more than default ($0.031)")
}
