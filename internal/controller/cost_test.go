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
	"strconv"
	"strings"
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

// costClock is a reconciler on a clock the test moves.
func costClock(start time.Time, m *stubMetrics) (*Reconciler, *time.Time) {
	now := start
	r := &Reconciler{clock: func() time.Time { return now }}
	if m != nil {
		r.metrics = m
	}
	return r, &now
}

func costReconciler(now time.Time, m *stubMetrics) *Reconciler {
	r, _ := costClock(now, m)
	return r
}

func accumulatedAt(t time.Time) *v1alpha1.CostStatus {
	return &v1alpha1.CostStatus{LastAccumulatedAt: &metav1.Time{Time: t}}
}

func pausedWith(w *v1alpha1.ManagedWorkload, rs v1alpha1.ResourceSnapshot) {
	w.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: rs.Replicas, Resources: &rs}
}

func dollars(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.TrimPrefix(s, "$"), 64)
	require.NoError(t, err, "a dollar figure: %q", s)
	return v
}

// flushFor accumulates a workload's cost at every status flush for d.
func flushFor(r *Reconciler, now *time.Time, w *v1alpha1.ManagedWorkload, d time.Duration) {
	for end := now.Add(d); now.Before(end); {
		*now = now.Add(statusFlushInterval)
		r.accumulateCost(context.Background(), w)
	}
}

func TestAccumulateCost_SkipsWhenNoMetrics(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)

	costReconciler(fixedTime, nil).accumulateCost(context.Background(), w)

	assert.Nil(t, w.Status.Cost)
}

func TestAccumulateCost_StartsTracking(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)

	costReconciler(fixedTime, &stubMetrics{cpuPerReplica: 1000}).accumulateCost(context.Background(), w)

	require.NotNil(t, w.Status.Cost)
	assert.True(t, w.Status.Cost.LastAccumulatedAt.Time.Equal(fixedTime))
	assert.Zero(t, w.Status.Cost.Tracked.Duration)
	assert.Equal(t, "$0.00", w.Status.Cost.CostThisMonth)
}

// An awake workload costs what its replicas request, sidecars included,
// whatever they happen to use.
func TestAccumulateCost_AwakeIsPricedOnRequests(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = accumulatedAt(fixedTime.Add(-time.Hour))
	m := &stubMetrics{cpuMillis: 5, cpuPerReplica: 500, memoryPerReplica: 2 * bytesPerGiB, replicas: 3, pvcBytes: 10 * bytesPerGiB}

	costReconciler(fixedTime, m).accumulateCost(context.Background(), w)

	c := w.Status.Cost
	assert.InDelta(t, 1.5, c.AwakeCPUHours.AsApproximateFloat64(), 1e-9, "3 replicas of 500m for an hour")
	assert.InDelta(t, 6, c.AwakeMemoryHours.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 10, c.StorageHours.AsApproximateFloat64(), 1e-9)
	assert.Zero(t, c.PausedCPUHours.AsApproximateFloat64())
	assert.Equal(t, time.Hour, c.Tracked.Duration)
	require.NotNil(t, c.Running)
	assert.Equal(t, v1alpha1.ResourceSnapshot{Replicas: 3, CPUMillis: 500, MemoryBytes: 2 * bytesPerGiB,
		StorageBytes: 10 * bytesPerGiB}, *c.Running)
}

func TestAccumulateCost_PausedSavesComputeButNotStorage(t *testing.T) {
	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = accumulatedAt(fixedTime.Add(-time.Hour))
	pausedWith(w, v1alpha1.ResourceSnapshot{Replicas: 3, CPUMillis: 500, MemoryBytes: 2 * bytesPerGiB,
		StorageBytes: 20 * bytesPerGiB})

	costReconciler(fixedTime, &stubMetrics{}).accumulateCost(context.Background(), w)

	c := w.Status.Cost
	assert.Zero(t, c.AwakeCPUHours.AsApproximateFloat64(), "a paused workload runs nothing")
	assert.InDelta(t, 1.5, c.PausedCPUHours.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 6, c.PausedMemoryHours.AsApproximateFloat64(), 1e-9)
	assert.InDelta(t, 20, c.StorageHours.AsApproximateFloat64(), 1e-9, "its claims still cost while it's paused")
	assert.Equal(t, cost.FormatDollars(1.5*0.031+6*0.004), c.SavedThisMonth)
	require.NotNil(t, c.ResourceReduction)
	assert.Equal(t, int64(1500), c.ResourceReduction.CPUMillis)
}

// Totals are kept to a billionth of an hour, so they add up to what the
// workload ran however small it is: rounding each five-minute flush to the
// cent, or to a milli-unit, used to drop them to nothing.
func TestAccumulateCost_FullPrecisionOverADayOfFlushes(t *testing.T) {
	const tolerance = 0.001
	tests := []struct {
		name      string
		phase     v1alpha1.WorkloadPhase
		cpuMillis int64
		memBytes  int64
	}{
		{name: "1 vCPU and 2 GiB paused", phase: v1alpha1.PhasePaused, cpuMillis: 1000, memBytes: 2 * bytesPerGiB},
		{name: "10m and 64Mi paused", phase: v1alpha1.PhasePaused, cpuMillis: 10, memBytes: 64 << 20},
		{name: "10m and 64Mi running", phase: v1alpha1.PhaseRunning, cpuMillis: 10, memBytes: 64 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := costWorkload(tt.phase)
			w.Status.Cost = accumulatedAt(fixedTime)
			pausedWith(w, v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: tt.cpuMillis, MemoryBytes: tt.memBytes})
			r, now := costClock(fixedTime, &stubMetrics{cpuPerReplica: float64(tt.cpuMillis),
				memoryPerReplica: float64(tt.memBytes), replicas: 1})

			flushFor(r, now, w, 24*time.Hour)

			c := w.Status.Cost
			wantCPU := float64(tt.cpuMillis) / 1000 * 24
			wantMem := float64(tt.memBytes) / bytesPerGiB * 24
			cpu, mem := c.PausedCPUHours, c.PausedMemoryHours
			if tt.phase != v1alpha1.PhasePaused {
				cpu, mem = c.AwakeCPUHours, c.AwakeMemoryHours
			}
			assert.InEpsilon(t, wantCPU, cpu.AsApproximateFloat64(), tolerance)
			assert.InEpsilon(t, wantMem, mem.AsApproximateFloat64(), tolerance)
			assert.Equal(t, 24*time.Hour, c.Tracked.Duration)
			if tt.phase == v1alpha1.PhasePaused {
				want := cost.ComputeHourly(float64(tt.cpuMillis)/1000, float64(tt.memBytes)/bytesPerGiB, cost.DefaultRates) * 24
				assert.Equal(t, cost.FormatDollars(want), c.SavedThisMonth)
			}
		})
	}
}

// Cost and savings are on the same basis, requests, over the same time, so
// they add up to what the workload would have cost without Hybernate.
func TestAccumulateCost_CostAndSavingsAddUp(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = accumulatedAt(fixedTime)
	snapshot := v1alpha1.ResourceSnapshot{Replicas: 2, CPUMillis: 1000, MemoryBytes: 4 * bytesPerGiB}
	r, now := costClock(fixedTime, &stubMetrics{cpuPerReplica: 1000, memoryPerReplica: 4 * bytesPerGiB, replicas: 2})

	flushFor(r, now, w, 8*time.Hour)
	w.Status.Phase = v1alpha1.PhasePaused
	pausedWith(w, snapshot)
	flushFor(r, now, w, 16*time.Hour)

	c := w.Status.Cost
	hourly := cost.ComputeHourly(2, 8, cost.DefaultRates)
	assert.Equal(t, cost.FormatDollars(8*hourly), c.CostThisMonth)
	assert.Equal(t, cost.FormatDollars(16*hourly), c.SavedThisMonth)
	assert.Equal(t, cost.FormatDollars(24*hourly), c.CostWithoutHybernateThisMonth)
	assert.InDelta(t, dollars(t, c.CostThisMonth)+dollars(t, c.SavedThisMonth),
		dollars(t, c.CostWithoutHybernateThisMonth), 0.01)
}

// A workload created late in the month is projected from the time it has
// been tracked, at the rate it has run, not from the day of the month.
func TestAccumulateCost_ProjectsFromTheTimeTracked(t *testing.T) {
	march20 := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	w := costWorkload(v1alpha1.PhaseRunning)
	r, now := costClock(march20, &stubMetrics{cpuPerReplica: 1000, replicas: 1})
	r.accumulateCost(context.Background(), w)

	flushFor(r, now, w, 23*time.Hour)
	assert.Equal(t, "pending", w.Status.Cost.ProjectedMonthlyCost, "less than a day tracked")

	flushFor(r, now, w, time.Hour)
	assert.Equal(t, cost.FormatDollars(0.031*24*31), w.Status.Cost.ProjectedMonthlyCost, "a vCPU for all of March")
	assert.Equal(t, "$0.00", w.Status.Cost.ProjectedMonthlySavings)
}

// The interval that spans the start of a month counts toward the new
// month from its first instant, in UTC.
func TestAccumulateCost_MonthRolloverKeepsTheNewMonthsShare(t *testing.T) {
	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = accumulatedAt(time.Date(2026, 2, 28, 23, 50, 0, 0, time.UTC))
	w.Status.Cost.PausedCPUHours = resource.MustParse("500")
	w.Status.Cost.ListRates = recordedRates()
	pausedWith(w, v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: 1000})

	costReconciler(time.Date(2026, 3, 1, 0, 5, 0, 0, time.UTC), &stubMetrics{}).accumulateCost(context.Background(), w)

	c := w.Status.Cost
	assert.InDelta(t, 5.0/60, c.PausedCPUHours.AsApproximateFloat64(), 1e-9, "last month's totals reset, the 5 minutes of March kept")
	assert.Equal(t, 5*time.Minute, c.Tracked.Duration)
	assert.NotNil(t, c.ListRates, "where it ran carries over")
}

// A gap longer than cost.MaxInterval, such as operator downtime, counts as
// cost.MaxInterval, and so does the time tracked, keeping projections true.
func TestAccumulateCost_DowntimeCountsAsTheLimit(t *testing.T) {
	w := costWorkload(v1alpha1.PhasePaused)
	w.Status.Cost = accumulatedAt(fixedTime.Add(-10 * time.Hour))
	pausedWith(w, v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: 1000})

	costReconciler(fixedTime, &stubMetrics{}).accumulateCost(context.Background(), w)

	assert.InDelta(t, cost.MaxInterval.Hours(), w.Status.Cost.PausedCPUHours.AsApproximateFloat64(), 1e-9)
	assert.Equal(t, cost.MaxInterval, w.Status.Cost.Tracked.Duration)
	assert.True(t, w.Status.Cost.LastAccumulatedAt.Time.Equal(fixedTime))
}

// Cost is brought up to date only when status is due a flush: between
// flushes it would be dropped unwritten.
func TestAccumulateCost_WaitsForTheFlush(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Cost = accumulatedAt(fixedTime.Add(-time.Minute))

	costReconciler(fixedTime, &stubMetrics{cpuPerReplica: 1000}).accumulateCost(context.Background(), w)

	assert.True(t, w.Status.Cost.LastAccumulatedAt.Time.Equal(fixedTime.Add(-time.Minute)))
	assert.Zero(t, w.Status.Cost.AwakeCPUHours.AsApproximateFloat64())
}

type countingPricer struct {
	stubPricer
	calls int
}

func (p *countingPricer) ListRates(ctx context.Context, w *v1alpha1.ManagedWorkload) (cost.Rates, bool, error) {
	p.calls++
	return p.stubPricer.ListRates(ctx, w)
}

// What a replica requests and its nodes' rates are read from its pods,
// which aren't cached, hourly and after a deploy rather than every flush.
func TestAccumulateCost_ReadsPodsHourlyOrAfterADeploy(t *testing.T) {
	w := costWorkload(v1alpha1.PhaseRunning)
	w.Status.Activity = &v1alpha1.ActivityStatus{TemplateHash: "v1"}
	r, now := costClock(fixedTime, &stubMetrics{cpuPerReplica: 1000})
	pricer := &countingPricer{stubPricer: stubPricer{rates: nodeRates, listed: true}}
	r.prices = pricer

	r.accumulateCost(context.Background(), w)
	flushFor(r, now, w, pricingInterval-statusFlushInterval)
	assert.Equal(t, 1, pricer.calls, "read once in the first hour")

	flushFor(r, now, w, statusFlushInterval)
	assert.Equal(t, 2, pricer.calls, "and again an hour on")

	w.Status.Activity.TemplateHash = "v2"
	flushFor(r, now, w, statusFlushInterval)
	assert.Equal(t, 3, pricer.calls, "and after a deploy")
}

func TestAccumulateCost_CustomRates(t *testing.T) {
	spent := func(rates *v1alpha1.CostRates) float64 {
		w := costWorkload(v1alpha1.PhaseRunning)
		w.Spec.CostTracking.Rates = rates
		w.Status.Cost = accumulatedAt(fixedTime.Add(-time.Hour))
		costReconciler(fixedTime, &stubMetrics{cpuPerReplica: 10000}).accumulateCost(context.Background(), w)
		return dollars(t, w.Status.Cost.CostThisMonth)
	}
	cpuRate := resource.MustParse("0.1")

	assert.InDelta(t, 10*0.031, spent(nil), 0.005, "the default rate")
	assert.InDelta(t, 10*0.1, spent(&v1alpha1.CostRates{CPUPerHour: &cpuRate}), 0.005, "its own rate")
}

type zeroReplicaMetrics struct{ stubMetrics }

func (*zeroReplicaMetrics) Replicas(_ context.Context, _ *v1alpha1.ManagedWorkload) (int32, error) {
	return 0, nil
}

func TestCaptureResourceSnapshot_PricesMemoryOnRequest(t *testing.T) {
	workload := costWorkload(v1alpha1.PhaseRunning)
	r := newTestReconciler(t, workload, &stubPauser{})
	r.metrics = &stubMetrics{
		cpuPerReplica:    500,
		memoryPerReplica: 512 << 20,
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
	r := newTestReconcilerWithReplicas(t, workload, &stubPauser{}, 0)
	r.metrics = &zeroReplicaMetrics{stubMetrics{cpuPerReplica: 500, memoryPerReplica: 512 << 20}}

	snap := r.captureResourceSnapshot(context.Background(), workload)

	require.NotNil(t, snap)
	assert.Equal(t, int32(0), snap.Replicas)
	assert.Equal(t, int64(512<<20), snap.MemoryBytes)
}

func TestCaptureResourceSnapshot_MissingTargetEmitsNoEvent(t *testing.T) {
	workload := costWorkload(v1alpha1.PhaseRunning)
	r := newTestReconcilerWithTarget(t, workload, &stubPauser{}, false)
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
			r := costReconciler(fixedTime, &stubMetrics{cpuPerReplica: 1000})
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
	saved := func(rates *v1alpha1.CostRates) string {
		w := costWorkload(v1alpha1.PhasePaused)
		w.Status.Cost = accumulatedAt(fixedTime.Add(-time.Hour))
		w.Status.Cost.ListRates = rates
		pausedWith(w, v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: 2000, MemoryBytes: 8 * bytesPerGiB})
		costReconciler(fixedTime, &stubMetrics{}).accumulateCost(context.Background(), w)
		return w.Status.Cost.SavedThisMonth
	}

	assert.Equal(t, cost.FormatDollars(2*0.031+8*0.004), saved(nil), "default rates without list rates")
	assert.Equal(t, cost.FormatDollars(2*0.05+8*0.006), saved(recordedRates()), "the nodes' list rates")
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
