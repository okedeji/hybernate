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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/metrics"
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

func costReconciler(now time.Time, m *stubMetrics) *Reconciler {
	r := &Reconciler{
		clock: func() time.Time { return now },
	}
	if m != nil {
		r.metrics = m
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

// An injected sidecar's CPU isn't activity, but it is cost.
func TestAccumulateCost_RunningIncludesSidecars(t *testing.T) {
	lastMeta := metav1.NewTime(fixedTime.Add(-1 * time.Hour))
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = &v1alpha1.CostStatus{LastAccumulatedAt: &lastMeta}
	m := &stubMetrics{cpuMillis: 1000, sidecarCPUMillis: 500}

	r := costReconciler(fixedTime, m)
	r.accumulateCost(context.Background(), w)

	assert.InDelta(t, 1.5, w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64(), 0.01)
}

func TestAccumulateCost_RunningAccumulatesUsage(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		EstimatedMonthlySavings:  "$0.00",
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
		EstimatedMonthlySavings:  "$0.00",
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

	savings := parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings)
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
		EstimatedMonthlySavings:  "$0.00",
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

	savings := parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings)
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
		EstimatedMonthlySavings:  "$0.00",
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

	savings := parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings)
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
		EstimatedMonthlySavings:  "$42.00",
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

func TestAccumulateCost_EstimatedCostWithoutManagement(t *testing.T) {
	lastAccumulated := fixedTime.Add(-1 * time.Hour)
	lastMeta := metav1.NewTime(lastAccumulated)

	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		EstimatedMonthlySavings:  "$0.00",
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

	costWithout := parseDollarAmount(w.Status.Cost.EstimatedCostWithoutManagement)
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
		EstimatedMonthlySavings:  "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}

	m := &stubMetrics{cpuMillis: 1000}
	r := costReconciler(fixedTime, m)
	r.accumulateCost(context.Background(), w)

	costWithout := parseDollarAmount(w.Status.Cost.EstimatedCostWithoutManagement)
	assert.Greater(t, costWithout, 0.0)

	// With default rate ($0.031/cpu-hour) 1 core × 1h = $0.031
	// With custom rate ($0.1/cpu-hour) 1 core × 1h = $0.10
	// So custom rate should produce a higher cost.
	wDefault := costWorkload(v1alpha1.PhaseRunning)
	wDefault.Status.Cost = &v1alpha1.CostStatus{
		CurrentMonthCPUHours:     *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthMemoryHours:  *resource.NewMilliQuantity(0, resource.DecimalSI),
		CurrentMonthStorageHours: *resource.NewMilliQuantity(0, resource.DecimalSI),
		EstimatedMonthlySavings:  "$0.00",
		LastAccumulatedAt:        &lastMeta,
	}

	r2 := costReconciler(fixedTime, m)
	r2.accumulateCost(context.Background(), wDefault)

	defaultCost := parseDollarAmount(wDefault.Status.Cost.EstimatedCostWithoutManagement)
	assert.Greater(t, costWithout, defaultCost, "custom rate ($0.10) should cost more than default ($0.031)")
}

type zeroReplicaMetrics struct{ stubMetrics }

func (*zeroReplicaMetrics) Replicas(_ context.Context, _ *v1alpha1.ManagedWorkload) (int32, error) {
	return 0, nil
}

func TestCaptureResourceSnapshot_PricesMemoryOnRequest(t *testing.T) {
	workload := costWorkload(v1alpha1.PhaseRunning)
	r := newTestReconciler(t, workload, &stubPauser{}, &stubDestroyer{})
	r.metrics = &stubMetrics{
		cpuPerReplica:    500,
		memoryPerReplica: 512 << 20,
		memoryBytes:      96 << 20, // live usage across all pods, deliberately far from the request
		replicas:         3,
	}

	snap := r.captureResourceSnapshot(context.Background(), workload)

	require.NotNil(t, snap)
	assert.Equal(t, int32(3), snap.Replicas)
	assert.Equal(t, int64(500), snap.CPUMillis)
	assert.Equal(t, int64(512<<20), snap.MemoryBytes, "memory per replica must come from the request, like CPU")
}

func TestCaptureResourceSnapshot_ZeroReplicas(t *testing.T) {
	workload := costWorkload(v1alpha1.PhaseRunning)
	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, &stubDestroyer{}, 0)
	r.metrics = &zeroReplicaMetrics{stubMetrics{cpuPerReplica: 500, memoryPerReplica: 512 << 20, memoryBytes: 64 << 20}}

	snap := r.captureResourceSnapshot(context.Background(), workload)

	require.NotNil(t, snap)
	assert.Equal(t, int32(0), snap.Replicas)
	assert.Equal(t, int64(512<<20), snap.MemoryBytes)
}

func TestCaptureResourceSnapshot_MissingTargetEmitsNoEvent(t *testing.T) {
	workload := costWorkload(v1alpha1.PhaseRunning)
	r := newTestReconcilerWithTarget(t, workload, &stubPauser{}, &stubDestroyer{}, false)
	r.metrics = &stubMetrics{cpuPerReplica: 500, memoryPerReplica: 512 << 20}

	r.captureResourceSnapshot(context.Background(), workload)

	recorder, ok := r.Recorder.(*events.FakeRecorder)
	require.True(t, ok)
	assert.Empty(t, recorder.Events, "capturing a snapshot must not report TargetNotFound as a side effect")
}

type stubPricer struct {
	rates  cost.Rates
	listed bool
	err    error
}

func (p stubPricer) ListRates(_ context.Context, _ *v1alpha1.ManagedWorkload) (cost.Rates, bool, error) {
	return p.rates, p.listed, p.err
}

var nodeRates = cost.Rates{CPUPerHour: 0.05, MemoryPerHour: 0.006}

// recordedRates are list rates Hybernate recorded for a workload's nodes.
func recordedRates() *v1alpha1.CostRates {
	c, m := resource.MustParse("0.05"), resource.MustParse("0.006")
	return &v1alpha1.CostRates{CPUPerHour: &c, MemoryPerHour: &m}
}

// What the nodes a workload runs on cost is recorded while it runs, and
// kept when there's nothing new to price it at.
func TestAccumulateCost_RecordsListRates(t *testing.T) {
	tests := []struct {
		name   string
		phase  v1alpha1.WorkloadPhase
		pricer stubPricer
		before *v1alpha1.CostRates
		want   *v1alpha1.CostRates
	}{
		{name: "running on listed nodes", phase: v1alpha1.PhaseRunning,
			pricer: stubPricer{rates: nodeRates, listed: true}, want: recordedRates()},
		{name: "moved to nodes not in the table", phase: v1alpha1.PhaseRunning,
			pricer: stubPricer{}, before: recordedRates()},
		{name: "no pod on a node yet", phase: v1alpha1.PhaseRunning,
			pricer: stubPricer{err: metrics.ErrNoScheduledPods}, before: recordedRates(),
			want: recordedRates()},
		{name: "nodes can't be read", phase: v1alpha1.PhaseIdle,
			pricer: stubPricer{err: errors.New("forbidden")}, before: recordedRates(),
			want: recordedRates()},
		{name: "paused: priced where it ran", phase: v1alpha1.PhasePaused,
			pricer: stubPricer{rates: cost.Rates{CPUPerHour: 1}, listed: true}, before: recordedRates(),
			want: recordedRates()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := costWorkload(tt.phase)
			w.Status.Cost = &v1alpha1.CostStatus{ListRates: tt.before}
			r := costReconciler(fixedTime, &stubMetrics{cpuMillis: 1000})
			r.prices = tt.pricer

			r.accumulateCost(context.Background(), w)

			if tt.want == nil {
				assert.Nil(t, w.Status.Cost.ListRates)
				return
			}
			require.NotNil(t, w.Status.Cost.ListRates)
			assert.InDelta(t, tt.want.CPUPerHour.AsApproximateFloat64(),
				w.Status.Cost.ListRates.CPUPerHour.AsApproximateFloat64(), 1e-6)
			assert.InDelta(t, tt.want.MemoryPerHour.AsApproximateFloat64(),
				w.Status.Cost.ListRates.MemoryPerHour.AsApproximateFloat64(), 1e-6)
		})
	}
}

// A paused workload's savings are priced at the nodes it ran on.
func TestAccumulateCost_PausedSavingsAtListRates(t *testing.T) {
	saved := func(rates *v1alpha1.CostRates) float64 {
		last := metav1.NewTime(fixedTime.Add(-time.Hour))
		w := costWorkload(v1alpha1.PhasePaused)
		w.Status.Cost = &v1alpha1.CostStatus{LastAccumulatedAt: &last, ListRates: rates}
		w.Status.Pause = &v1alpha1.PauseStatus{Resources: &v1alpha1.ResourceSnapshot{
			Replicas: 1, CPUMillis: 2000, MemoryBytes: 8 * bytesPerGiB}}
		costReconciler(fixedTime, &stubMetrics{}).accumulateCost(context.Background(), w)
		return parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings)
	}

	assert.InDelta(t, 2*0.031+8*0.004, saved(nil), 0.005, "default rates without list rates")
	assert.InDelta(t, 2*0.05+8*0.006, saved(recordedRates()), 0.005, "the nodes' list rates")
}

func TestAccumulateCost_MonthlyResetKeepsListRates(t *testing.T) {
	lastMonth := metav1.NewTime(time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC))
	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = &v1alpha1.CostStatus{LastAccumulatedAt: &lastMonth, EstimatedMonthlySavings: "$42.00",
		ListRates: recordedRates()}

	costReconciler(fixedTime, &stubMetrics{}).accumulateCost(context.Background(), w)

	assert.NotNil(t, w.Status.Cost.ListRates)
	assert.Less(t, parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings), 42.0, "the month's totals reset")
}

// Rates a workload sets win over its nodes' list rates, part by part.
func TestResolveCostRates(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = &v1alpha1.CostStatus{ListRates: recordedRates()}
	cpu := resource.MustParse("0.1")
	w.Spec.CostTracking.Rates = &v1alpha1.CostRates{CPUPerHour: &cpu}

	got := resolveCostRates(w)

	assert.InDelta(t, 0.1, got.CPUPerHour, 1e-9, "its own CPU rate")
	assert.InDelta(t, 0.006, got.MemoryPerHour, 1e-9, "its nodes' memory rate")
	assert.InDelta(t, cost.DefaultRates.StoragePerMonth, got.StoragePerMonth, 1e-9, "the default storage rate")
}
