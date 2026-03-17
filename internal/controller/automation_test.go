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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	"github.com/okedeji/hybernate/internal/policy"
	"github.com/okedeji/hybernate/internal/signal"
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
	cpuMillis     float64
	cpuPerReplica float64
	memoryBytes   float64
	pvcBytes      float64
	err           error
}

func (m *stubMetrics) CPUUsage(_ context.Context, _ *v1alpha1.ManagedWorkload) (resource.Quantity, error) {
	if m.err != nil {
		return resource.Quantity{}, m.err
	}
	return *resource.NewMilliQuantity(int64(m.cpuMillis), resource.DecimalSI), nil
}

func (m *stubMetrics) TotalCPUMillis(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuMillis, m.err
}

func (m *stubMetrics) CPURequestPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuPerReplica, m.err
}

func (m *stubMetrics) TotalMemoryBytes(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.memoryBytes, m.err
}

func (m *stubMetrics) TotalPVCBytes(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.pvcBytes, m.err
}

type stubIdleEvaluator struct {
	eval            policy.IdleEvaluation
	err             error
	evalCalls       int
	startGraceCalls int
	resetCalls      int
}

func (s *stubIdleEvaluator) Evaluate(_ context.Context, _, _ string, _ []signal.Checker, _ time.Duration) (policy.IdleEvaluation, error) {
	s.evalCalls++
	return s.eval, s.err
}

func (s *stubIdleEvaluator) StartGracePeriod(_, _ string) {
	s.startGraceCalls++
}

func (s *stubIdleEvaluator) Reset(_, _ string) {
	s.resetCalls++
}

type stubScaleEvaluator struct {
	decision     policy.ScaleDecision
	err          error
	evalCalls    int
	lastProposed int32
}

func (s *stubScaleEvaluator) Evaluate(_ context.Context, _, _ string, proposed, _ int32, _ policy.ScaleConstraints, _ []signal.Checker) (policy.ScaleDecision, error) {
	s.evalCalls++
	s.lastProposed = proposed
	return s.decision, s.err
}

type stubLifecycleScaler struct {
	done       bool
	err        error
	scaleCalls int
	lastTarget int32
}

func (s *stubLifecycleScaler) Scale(_ context.Context, _ *v1alpha1.ManagedWorkload, target int32) (bool, error) {
	s.scaleCalls++
	s.lastTarget = target
	return s.done, s.err
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
		Client:          builder.Build(),
		Scheme:          scheme,
		Recorder:        record.NewFakeRecorder(10),
		pauser:          opts.pauser,
		destroyer:       opts.destroyer,
		lifecycleScaler: opts.lifecycleScaler,
		idle:            opts.idle,
		scale:           opts.scale,
		engines:         reg,
		clock:           func() time.Time { return fixedTime },
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
	pauser          *stubPauser
	destroyer       *stubDestroyer
	lifecycleScaler *stubLifecycleScaler
	idle            *stubIdleEvaluator
	scale           *stubScaleEvaluator
	metrics         *stubMetrics
	needsFeed       bool
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

			result, err := r.reconcileAutomation(context.Background(), workload)
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

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	// Prediction status should be updated even though desiredState is set.
	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Suggesting", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, 72, workload.Status.Prediction.DailyConfidence)
}

func TestAutomation_ObservingRequeuesHourly(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing, dataPoints: 10}
	r := newAutomationReconciler(t, workload, engine, automationOpts{})

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)

	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Observing", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, "Observing", workload.Status.Prediction.WeeklyPhase)
}

func TestAutomation_SuggestingEmitsDryRunEvents(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		Action: v1alpha1.IdleActionPause,
	}

	engine := &stubForecaster{
		phase:        forecast.DailySuggesting,
		predictValue: 10.0, // below idleThreshold
	}
	idle := &stubIdleEvaluator{
		eval: policy.IdleEvaluation{
			Status:  policy.IdleStatusIdle,
			IdleFor: 45 * time.Minute,
		},
	}
	r := newAutomationReconciler(t, workload, engine, automationOpts{idle: idle})

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Hour, result.RequeueAfter)
	assert.Equal(t, 1, idle.evalCalls)

	// Phase should NOT change — suggesting mode doesn't act.
	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
}

func TestAutomation_ActiveIdlePauses(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		Action: v1alpha1.IdleActionPause,
	}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 10.0,
	}
	idle := &stubIdleEvaluator{
		eval: policy.IdleEvaluation{
			Status:  policy.IdleStatusIdle,
			IdleFor: 45 * time.Minute,
		},
	}
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		idle:   idle,
		pauser: pauser,
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, 1, idle.evalCalls)
	assert.Equal(t, 1, pauser.pauseCalls)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, w.Status.Phase)
}

func TestAutomation_ActiveIdleDestroys(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		Action: v1alpha1.IdleActionDestroy,
	}

	engine := &stubForecaster{
		phase:        forecast.FullyActive,
		predictValue: 10.0,
	}
	idle := &stubIdleEvaluator{
		eval: policy.IdleEvaluation{
			Status:  policy.IdleStatusIdle,
			IdleFor: 45 * time.Minute,
		},
	}
	destroyer := &stubDestroyer{destroyDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		idle:      idle,
		destroyer: destroyer,
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, 1, destroyer.destroyCalls)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseDestroyed, w.Status.Phase)
}

func TestAutomation_ActiveNotIdleDoesNotAct(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		Action: v1alpha1.IdleActionPause,
	}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 10.0,
	}
	idle := &stubIdleEvaluator{
		eval: policy.IdleEvaluation{Status: policy.IdleStatusActive},
	}
	pauser := &stubPauser{}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		idle:   idle,
		pauser: pauser,
	})

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1*time.Minute, result.RequeueAfter)
	assert.Equal(t, 0, pauser.pauseCalls)
}

func TestAutomation_ActiveIdleFlukePredictionDisagrees(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		Action: v1alpha1.IdleActionPause,
	}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 500.0, // well above idleThreshold — prediction disagrees
	}
	idle := &stubIdleEvaluator{
		eval: policy.IdleEvaluation{Status: policy.IdleStatusSignalsConfirm},
	}
	pauser := &stubPauser{}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		idle:   idle,
		pauser: pauser,
	})

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 5*time.Minute, result.RequeueAfter)
	assert.Equal(t, 1, idle.evalCalls)
	assert.Equal(t, 0, pauser.pauseCalls)
}

func TestAutomation_ActiveScalesUp(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.ScalePolicy = &v1alpha1.ScalePolicySpec{
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	workload.Status.Scale = &v1alpha1.ScaleStatus{CurrentReplicas: 3}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 800.0, // above idle threshold, maps to ~8 replicas
	}
	scaleEval := &stubScaleEvaluator{
		decision: policy.ScaleDecision{
			Target:    8,
			Current:   3,
			Direction: policy.ScaleUp,
		},
	}
	scaler := &stubLifecycleScaler{done: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		scale:           scaleEval,
		lifecycleScaler: scaler,
		metrics:         &stubMetrics{cpuPerReplica: 100},
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, 1, scaleEval.evalCalls)
	assert.Equal(t, 1, scaler.scaleCalls)
	assert.Equal(t, int32(8), scaler.lastTarget)

	w := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
}

func TestAutomation_ActiveScaleNotReadyRequeues(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.ScalePolicy = &v1alpha1.ScalePolicySpec{
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	workload.Status.Scale = &v1alpha1.ScaleStatus{CurrentReplicas: 3}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 800.0,
	}
	scaleEval := &stubScaleEvaluator{
		decision: policy.ScaleDecision{
			Target:    8,
			Current:   3,
			Direction: policy.ScaleUp,
		},
	}
	scaler := &stubLifecycleScaler{done: false}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		scale:           scaleEval,
		lifecycleScaler: scaler,
		metrics:         &stubMetrics{cpuPerReplica: 100},
	})

	result, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 5*time.Second, result.RequeueAfter)
}

func TestAutomation_OverrideReplicasSkipsPrediction(t *testing.T) {
	override := int32(5)
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.ScalePolicy = &v1alpha1.ScalePolicySpec{
		MinReplicas:      1,
		MaxReplicas:      10,
		OverrideReplicas: &override,
	}
	workload.Status.Scale = &v1alpha1.ScaleStatus{CurrentReplicas: 3}

	engine := &stubForecaster{
		phase:        forecast.DailyActive,
		predictValue: 800.0, // would map to 8 replicas — should be ignored
	}
	scaleEval := &stubScaleEvaluator{
		decision: policy.ScaleDecision{
			Target:    5,
			Current:   3,
			Direction: policy.ScaleUp,
		},
	}
	scaler := &stubLifecycleScaler{done: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		scale:           scaleEval,
		lifecycleScaler: scaler,
		metrics:         &stubMetrics{cpuPerReplica: 100},
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)

	// Evaluator should have been called with proposed=5 (override), not 8 (prediction).
	assert.Equal(t, int32(5), scaleEval.lastProposed)
	assert.Equal(t, 1, scaler.scaleCalls)
	assert.Equal(t, int32(5), scaler.lastTarget)
}

func TestAutomation_OverrideReplicasClampedToBounds(t *testing.T) {
	override := int32(20) // exceeds maxReplicas
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.ScalePolicy = &v1alpha1.ScalePolicySpec{
		MinReplicas:      2,
		MaxReplicas:      10,
		OverrideReplicas: &override,
	}
	workload.Status.Scale = &v1alpha1.ScaleStatus{CurrentReplicas: 3}

	engine := &stubForecaster{phase: forecast.DailyActive}
	scaleEval := &stubScaleEvaluator{
		decision: policy.ScaleDecision{
			Target:    10,
			Current:   3,
			Direction: policy.ScaleUp,
		},
	}
	scaler := &stubLifecycleScaler{done: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{
		scale:           scaleEval,
		lifecycleScaler: scaler,
		metrics:         &stubMetrics{cpuPerReplica: 100},
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)

	// Should be clamped to maxReplicas=10, not 20.
	assert.Equal(t, int32(10), scaleEval.lastProposed)
}

func TestAutomation_FeedsEngineHourly(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{cpuMillis: 250.0}

	r := newAutomationReconciler(t, workload, engine, automationOpts{
		metrics:   metrics,
		needsFeed: true,
	})

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, 1, engine.observeCalls)

	// Second call within the hour should not feed.
	_, err = r.reconcileAutomation(context.Background(), workload)
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

func TestAutomation_DemandToReplicas(t *testing.T) {
	tests := []struct {
		demand        float64
		cpuPerReplica float64
		min, max      int
		want          int32
	}{
		{0, 100, 1, 10, 1},
		{50, 100, 1, 10, 1},
		{100, 100, 1, 10, 1},
		{150, 100, 1, 10, 2},
		{800, 100, 1, 10, 8},
		{1500, 100, 1, 10, 10},
		{-10, 100, 1, 10, 1},
		{750, 250, 1, 10, 3}, // 250m per replica: ceil(750/250) = 3
		{500, 500, 1, 10, 1}, // 500m per replica: ceil(500/500) = 1
		{0, 0, 1, 10, 1},     // zero cpuPerReplica returns min
	}

	for _, tt := range tests {
		got := demandToReplicas(tt.demand, tt.cpuPerReplica, tt.min, tt.max)
		assert.Equal(t, tt.want, got, "demand=%.0f cpu/replica=%.0f min=%d max=%d", tt.demand, tt.cpuPerReplica, tt.min, tt.max)
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

	_, err := r.reconcileAutomation(context.Background(), workload)
	require.NoError(t, err)

	w := &v1alpha1.ManagedWorkload{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "api", Namespace: "default"}, w))

	require.NotNil(t, w.Status.Prediction)
	assert.Equal(t, "Active", w.Status.Prediction.DailyPhase)
	assert.Equal(t, "Suggesting", w.Status.Prediction.WeeklyPhase)
	assert.Equal(t, 91, w.Status.Prediction.DailyConfidence)
	assert.Equal(t, 72, w.Status.Prediction.WeeklyConfidence)
}
