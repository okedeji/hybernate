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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

// --- Stubs ---

type stubForecaster struct {
	phase            forecast.Phase
	dailyConfidence  int
	weeklyConfidence int
	dataPoints       int
	predictValue     float64
	observeCalls     int
	regimeChanged    bool
	anomalyDetected  bool
}

func (f *stubForecaster) Observe(actual float64, _ time.Time) float64 {
	f.observeCalls++
	return f.predictValue
}
func (f *stubForecaster) Predict(_ int, _ time.Time) float64 { return f.predictValue }
func (f *stubForecaster) Export() ([]byte, error)            { return []byte("{}"), nil }
func (f *stubForecaster) GetPhase() forecast.Phase           { return f.phase }
func (f *stubForecaster) DailyConfidence() int               { return f.dailyConfidence }
func (f *stubForecaster) WeeklyConfidence() int              { return f.weeklyConfidence }
func (f *stubForecaster) GetDataPoints() int                 { return f.dataPoints }
func (f *stubForecaster) RegimeChanged() bool                { return f.regimeChanged }
func (f *stubForecaster) AnomalyDetected() bool              { return f.anomalyDetected }

type stubMetrics struct {
	cpuMillis        float64
	cpuPerReplica    float64
	memoryBytes      float64
	memoryPerReplica float64
	replicas         int32
	pvcBytes         float64
	err              error
}

func (m *stubMetrics) CPUUsage(_ context.Context, _ *v1alpha1.ManagedWorkload) (resource.Quantity, error) {
	if m.err != nil {
		return resource.Quantity{}, m.err
	}
	return *resource.NewMilliQuantity(int64(m.cpuMillis), resource.DecimalSI), nil
}

func (m *stubMetrics) MemoryUsage(_ context.Context, _ *v1alpha1.ManagedWorkload) (resource.Quantity, error) {
	if m.err != nil {
		return resource.Quantity{}, m.err
	}
	return *resource.NewQuantity(int64(m.memoryBytes), resource.BinarySI), nil
}

func (m *stubMetrics) TotalCPUMillis(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuMillis, m.err
}

func (m *stubMetrics) CPURequestPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuPerReplica, m.err
}

func (m *stubMetrics) MemoryRequestPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.memoryPerReplica, m.err
}

func (m *stubMetrics) Replicas(_ context.Context, _ *v1alpha1.ManagedWorkload) (int32, error) {
	if m.replicas > 0 {
		return m.replicas, m.err
	}
	return 1, m.err
}

func (m *stubMetrics) TotalMemoryBytes(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.memoryBytes, m.err
}

func (m *stubMetrics) TotalPVCBytes(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.pvcBytes, m.err
}

func newAutomationReconciler(t *testing.T, workload *v1alpha1.ManagedWorkload, engine *stubForecaster, opts automationOpts) *Reconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.ManagedWorkload{})
	if workload != nil {
		builder = builder.WithObjects(workload)
	}

	reg := newEngineRegistry(func(_ int) forecaster { return engine })
	// Pre-populate the engine so getOrCreate returns our stub.
	key := workload.Namespace + "/" + workload.Name
	reg.engines[key] = engine
	// Mark as just fed so the reconciler doesn't try to read metrics
	// (unless the test explicitly wants to test feeding).
	if !opts.needsFeed {
		reg.lastFed[key] = fixedTime
	}

	r := &Reconciler{
		Client:    builder.Build(),
		Scheme:    scheme,
		Recorder:  events.NewFakeRecorder(10),
		pauser:    opts.pauser,
		destroyer: opts.destroyer,
		engines:   reg,
		clock:     func() time.Time { return fixedTime },
	}
	if opts.metrics != nil {
		r.metrics = opts.metrics
	}
	if r.pauser == nil {
		r.pauser = &stubPauser{}
	}
	if r.destroyer == nil {
		r.destroyer = &stubDestroyer{}
	}
	return r
}

type automationOpts struct {
	pauser    *stubPauser
	destroyer *stubDestroyer
	metrics   *stubMetrics
	needsFeed bool
}

func automationWorkload(phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
		Status: v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
}

// --- Tests ---

func TestAutomation_SkipsNonRunningPhase(t *testing.T) {
	for _, phase := range []v1alpha1.WorkloadPhase{v1alpha1.PhasePaused, v1alpha1.PhaseDestroyed, v1alpha1.PhasePausing} {
		t.Run(string(phase), func(t *testing.T) {
			workload := automationWorkload(phase)
			engine := &stubForecaster{phase: forecast.DailyActive}
			r := newAutomationReconciler(t, workload, engine, automationOpts{})

			result, err := r.reconcileAutomation(context.Background(), workload, nil)
			require.NoError(t, err)
			assert.Nil(t, result)
		})
	}
}

func TestAutomation_DesiredStateStillUpdatesStatus(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.DesiredState = desiredState(v1alpha1.DesiredStatePaused)

	engine := &stubForecaster{
		phase:           forecast.DailySuggesting,
		dailyConfidence: 72,
		dataPoints:      30,
	}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})

	result, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	// Prediction status should be updated even though desiredState is set.
	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Suggesting", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, 72, workload.Status.Prediction.DailyConfidence)
}

func TestAutomation_NoIdlePolicyOnlyLearns(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing, dataPoints: 10}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})

	result, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Observing", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, "Observing", workload.Status.Prediction.WeeklyPhase)
}

func TestAutomation_FeedsEngineHourly(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{cpuMillis: 250.0}

	r := newAutomationReconciler(t, workload, engine, automationOpts{
		metrics:   metrics,
		needsFeed: true,
	})

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, engine.observeCalls)

	// Second call within the hour should not feed.
	_, err = r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, engine.observeCalls)
}

func TestAutomation_SeasonPhasesMapping(t *testing.T) {
	tests := []struct {
		phase  forecast.Phase
		daily  string
		weekly string
	}{
		{forecast.Observing, "Observing", "Observing"},
		{forecast.DailySuggesting, "Suggesting", "Observing"},
		{forecast.DailyActive, "Active", "Observing"},
		{forecast.WeeklySuggesting, "Active", "Suggesting"},
		{forecast.FullyActive, "Active", "Active"},
	}

	for _, tt := range tests {
		t.Run(tt.phase.String(), func(t *testing.T) {
			daily, weekly := seasonPhases(tt.phase)
			assert.Equal(t, tt.daily, daily)
			assert.Equal(t, tt.weekly, weekly)
		})
	}
}

func TestAutomation_PredictionStatusUpdated(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{
		phase:            forecast.WeeklySuggesting,
		dailyConfidence:  91,
		weeklyConfidence: 72,
		dataPoints:       96,
		predictValue:     200.0,
	}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	w := &v1alpha1.ManagedWorkload{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, w))

	require.NotNil(t, w.Status.Prediction)
	assert.Equal(t, "Active", w.Status.Prediction.DailyPhase)
	assert.Equal(t, "Suggesting", w.Status.Prediction.WeeklyPhase)
	assert.Equal(t, 91, w.Status.Prediction.DailyConfidence)
	assert.Equal(t, 72, w.Status.Prediction.WeeklyConfidence)
}

func metricsCondition(t *testing.T, r *Reconciler) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(getWorkload(t, r, "api").Status.Conditions, conditionMetricsAvailable)
}

func TestAutomation_MissingMetricsSurfacesCondition(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{name: "no pod metrics", err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics), wantReason: "NoPodMetrics"},
		{name: "metrics API down", err: errors.New("the server could not find the requested resource"), wantReason: "MetricsUnavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := automationWorkload(v1alpha1.PhaseRunning)
			engine := &stubForecaster{phase: forecast.Observing}
			r := newAutomationReconciler(t, workload, engine, automationOpts{
				metrics:   &stubMetrics{err: tt.err, replicas: 2},
				needsFeed: true,
			})

			for range 2 {
				result, err := r.reconcileAutomation(context.Background(), workload, nil)
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, 1*time.Minute, result.RequeueAfter)
			}

			assert.Equal(t, 0, engine.observeCalls)
			cond := metricsCondition(t, r)
			require.NotNil(t, cond, "the user must be able to see why the forecast isn't learning")
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)

			recorder, ok := r.Recorder.(*events.FakeRecorder)
			require.True(t, ok)
			assert.Len(t, recorder.Events, 1, "the warning fires once, not on every retry")
		})
	}
}

func TestAutomation_ZeroReplicasFeedsZeroDemand(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	r := newAutomationReconciler(t, workload, engine, automationOpts{needsFeed: true})
	r.metrics = &zeroReplicaMetrics{stubMetrics{err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics)}}

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, engine.observeCalls, "a target scaled to zero is an observation of zero demand")
	cond := metricsCondition(t, r)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestAutomation_MetricsConditionRecovers(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics), replicas: 2}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics, needsFeed: true})

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.Equal(t, metav1.ConditionFalse, metricsCondition(t, r).Status)

	metrics.err = nil
	metrics.cpuMillis = 120
	workload = getWorkload(t, r, "api")
	_, err = r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, engine.observeCalls)
	assert.Equal(t, metav1.ConditionTrue, metricsCondition(t, r).Status)
}
