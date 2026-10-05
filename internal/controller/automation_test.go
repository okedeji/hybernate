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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

type stubForecaster struct {
	phase            forecast.Phase
	dailyConfidence  int
	weeklyConfidence int
	dataPoints       int
	predictValue     float64
	predictByHour    map[int]float64 // overrides predictValue for an hour ahead
	observed         []float64
	observedHours    []time.Time
	observeCalls     int
	observeErr       error
	regimeChanged    bool
	anomalyDetected  bool
	state            string
	exportErr        error
	settings         forecast.Settings
}

func (f *stubForecaster) Observe(actual float64, hour time.Time) (float64, error) {
	if f.observeErr != nil {
		return 0, f.observeErr
	}
	f.observeCalls++
	f.observed = append(f.observed, actual)
	f.observedHours = append(f.observedHours, hour)
	return f.predictValue, nil
}
func (f *stubForecaster) Observed(hour time.Time) bool {
	return slices.ContainsFunc(f.observedHours, hour.Equal)
}
func (f *stubForecaster) Predict(h int, _ time.Time) float64 {
	if v, ok := f.predictByHour[h]; ok {
		return v
	}
	return f.predictValue
}
func (f *stubForecaster) Export() (string, error)              { return f.state, f.exportErr }
func (f *stubForecaster) Configure(settings forecast.Settings) { f.settings = settings }
func (f *stubForecaster) GetPhase() forecast.Phase             { return f.phase }
func (f *stubForecaster) DailyConfidence() int                 { return f.dailyConfidence }
func (f *stubForecaster) WeeklyConfidence() int                { return f.weeklyConfidence }
func (f *stubForecaster) GetDataPoints() int                   { return f.dataPoints }
func (f *stubForecaster) RegimeChanged() bool                  { return f.regimeChanged }
func (f *stubForecaster) AnomalyDetected() bool                { return f.anomalyDetected }

type stubMetrics struct {
	cpuMillis        float64
	cpuPerReplica    float64
	memoryPerReplica float64
	replicas         int32
	pvcBytes         float64
	err              error
}

func (m *stubMetrics) WorkloadCPUMillis(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuMillis, m.err
}

func (m *stubMetrics) CPURequestPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, error) {
	return m.cpuPerReplica, m.err
}

func (m *stubMetrics) PodRequestsPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (cpuMillis, memBytes float64, err error) {
	return m.cpuPerReplica, m.memoryPerReplica, m.err
}

func (m *stubMetrics) Replicas(_ context.Context, _ *v1alpha1.ManagedWorkload) (int32, error) {
	if m.replicas > 0 {
		return m.replicas, m.err
	}
	return 1, m.err
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

	reg := newEngineRegistry(func() forecaster { return engine })
	reg.engines[workload.UID] = engine

	r := &Reconciler{
		Client:   builder.Build(),
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		pauser:   opts.pauser,
		engines:  reg,
		clock:    func() time.Time { return fixedTime },
	}
	if opts.metrics != nil {
		r.metrics = opts.metrics
	}
	if r.pauser == nil {
		r.pauser = &stubPauser{}
	}
	return r
}

type automationOpts struct {
	pauser  *stubPauser
	metrics *stubMetrics
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

func TestAutomation_SkipsTransitions(t *testing.T) {
	for _, phase := range []v1alpha1.WorkloadPhase{v1alpha1.PhasePausing, v1alpha1.PhaseResuming} {
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

// A paused workload is looked at again by time alone, or autoResume never
// fires, the forecast never learns its quiet hours, and savings only accrue
// when something unrelated happens to touch it.
func TestPausedRecheck(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	tests := []struct {
		name string
		at   time.Time
		loc  *time.Location
		want time.Duration
	}{
		{name: "at the flush interval", at: fixedTime.Add(10 * time.Minute), want: statusFlushInterval},
		{name: "when autoResume next looks ahead", at: fixedTime.Add(42 * time.Minute), want: 3 * time.Minute},
		{name: "on the hour", at: fixedTime.Add(58 * time.Minute), want: 2 * time.Minute},
		{name: "ahead of a local hour at half past in UTC", at: fixedTime.Add(12 * time.Minute), loc: kolkata,
			want: 3 * time.Minute},
		{name: "on a local hour at half past in UTC", at: fixedTime.Add(28 * time.Minute), loc: kolkata,
			want: 2 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{Timezone: tt.loc, clock: func() time.Time { return tt.at }}
			assert.Equal(t, tt.want, r.pausedRecheck())
		})
	}
}

// In a timezone whose hours begin at half past in UTC, the lead before a
// busy hour is counted to the local hour, which is the hour the forecast
// predicts.
func TestAutoResume_LeadIsCountedToTheLocalHour(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	workload := pausedForecastWorkload()
	engine := &stubForecaster{phase: forecast.DailyActive, predictByHour: map[int]float64{0: 1, 1: 50}}
	pauser := &stubPauser{resumeDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{pauser: pauser})
	r.Timezone = kolkata
	r.clock = func() time.Time { return fixedTime.Add(20 * time.Minute) } // 17:50 in Kolkata

	_, err = r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.resumeCalls, "woken 10 minutes before the busy local hour")
}

func TestAutomation_PausedIsRequeued(t *testing.T) {
	for _, desired := range []*v1alpha1.DesiredState{nil, desiredState(v1alpha1.DesiredStatePaused)} {
		workload := pausedForecastWorkload()
		workload.Spec.DesiredState = desired
		r := newAutomationReconciler(t, workload, &stubForecaster{phase: forecast.Observing}, automationOpts{})
		r.clock = func() time.Time { return fixedTime.Add(10 * time.Minute) }

		result, err := r.reconcileAutomation(context.Background(), workload, nil)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, statusFlushInterval, result.RequeueAfter)
	}
}

// autoResume fires through the requeues a paused workload schedules itself,
// with nothing else touching it.
func TestAutoResume_FiresOnTimeAlone(t *testing.T) {
	workload := pausedForecastWorkload()
	workload.Finalizers = []string{finalizerName}
	engine := &stubForecaster{phase: forecast.DailyActive, predictByHour: map[int]float64{0: 1, 1: 50}}
	pauser := &stubPauser{resumeDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{pauser: pauser})
	require.NoError(t, r.Create(context.Background(), targetDeploymentWithReplicas("api", "default", 0)))
	now := fixedTime.Add(3 * time.Minute)
	r.clock = func() time.Time { return now }

	for range 20 {
		result, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)
		if pauser.resumeCalls > 0 {
			break
		}
		require.Positive(t, result.RequeueAfter, "a paused workload at %s must be requeued", now.Format("15:04"))
		now = now.Add(result.RequeueAfter)
	}

	require.Equal(t, 1, pauser.resumeCalls, "autoResume never fired")
	assert.Equal(t, fixedTime.Add(time.Hour-autoResumeLead), now, "woken as the lead before the busy hour begins")
}

func TestAutomation_DesiredStateStillUpdatesStatus(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.DesiredState = desiredState(v1alpha1.DesiredStatePaused)

	engine := &stubForecaster{
		phase:           forecast.DailySuggesting,
		dailyConfidence: 72,
		dataPoints:      30,
	}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: &stubMetrics{}})

	result, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, activityCheckInterval, result.RequeueAfter, "the forecast goes on learning")

	// Prediction status should be updated even though desiredState is set.
	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Suggesting", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, 72, workload.Status.Prediction.DailyConfidence)
}

func TestAutomation_NoIdlePolicyOnlyLearns(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing, dataPoints: 10}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: &stubMetrics{}})

	result, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, activityCheckInterval, result.RequeueAfter, "the forecast goes on learning")

	assert.NotNil(t, workload.Status.Prediction)
	assert.Equal(t, "Observing", workload.Status.Prediction.DailyPhase)
	assert.Equal(t, "Observing", workload.Status.Prediction.WeeklyPhase)
}

// lookEveryMinute reconciles the workload once a minute from from to until,
// inclusive, as the activity clock does, with cpu the usage at each look.
func lookEveryMinute(t *testing.T, r *Reconciler, workload *v1alpha1.ManagedWorkload, metrics *stubMetrics,
	from, until time.Time, cpu func(now time.Time) float64) {
	t.Helper()
	for now := from; !now.After(until); now = now.Add(time.Minute) {
		r.clock = func() time.Time { return now }
		metrics.cpuMillis = cpu(now)
		_, err := r.reconcileAutomation(context.Background(), workload, nil)
		require.NoError(t, err)
	}
}

// The forecast is fed an hour once it's over, at the busiest minute seen
// in it: an hour busy only for its last ten minutes is an hour the workload
// had to be awake for.
func TestAutomation_FeedsEachHourItsPeak(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics})
	lastTen := func(now time.Time) float64 {
		if now.Minute() >= 50 {
			return 400
		}
		return 5
	}

	lookEveryMinute(t, r, workload, metrics, fixedTime, fixedTime.Add(59*time.Minute), lastTen)
	assert.Zero(t, engine.observeCalls, "an hour is fed once it's over")

	lookEveryMinute(t, r, workload, metrics, fixedTime.Add(time.Hour), fixedTime.Add(time.Hour), lastTen)
	assert.Equal(t, []float64{400}, engine.observed)
	assert.Equal(t, []time.Time{fixedTime}, engine.observedHours, "as the hour it was seen in")
}

// A workload's first minutes awake, starting up, aren't demand. Counted,
// an autoResume wake at 12:45 would teach the forecast that 12:00 is busy,
// and it would wake the workload an hour earlier the next week.
func TestAutomation_WarmUpAfterAWakeIsNotDemand(t *testing.T) {
	woke := fixedTime.Add(time.Hour - autoResumeLead)
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics})
	warmingUp := func(now time.Time) float64 {
		if now.Before(woke.Add(3 * time.Minute)) {
			return 800
		}
		return 5
	}

	lookEveryMinute(t, r, workload, metrics, fixedTime, woke.Add(-time.Minute), func(time.Time) float64 { return 5 })
	workload.Status.LastActedAt = ptr.To(metav1.NewTime(woke))
	lookEveryMinute(t, r, workload, metrics, woke, fixedTime.Add(time.Hour), warmingUp)

	assert.Equal(t, []float64{5}, engine.observed)
}

// An hour is fed only if it was seen from start to end. Partway through an
// operator restart, its busiest minutes may have gone unseen, and the
// forecast would learn a busy hour as a quiet one.
func TestAutomation_FeedsOnlyHoursSeenWhole(t *testing.T) {
	tests := []struct {
		name  string
		looks [][2]time.Duration
		want  []float64
	}{
		{name: "seen whole", looks: [][2]time.Duration{{0, time.Hour}}, want: []float64{400}},
		{name: "first seen partway through, as after a restart",
			looks: [][2]time.Duration{{20 * time.Minute, time.Hour}}},
		{name: "not seen to its end", looks: [][2]time.Duration{{0, 40 * time.Minute}, {time.Hour, time.Hour}}},
		{name: "a gap while it wakes or pauses", looks: [][2]time.Duration{{0, 10 * time.Minute}, {40 * time.Minute, time.Hour}},
			want: []float64{400}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := automationWorkload(v1alpha1.PhaseRunning)
			engine := &stubForecaster{phase: forecast.Observing}
			metrics := &stubMetrics{}
			r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics})

			for _, span := range tt.looks {
				lookEveryMinute(t, r, workload, metrics, fixedTime.Add(span[0]), fixedTime.Add(span[1]),
					func(time.Time) float64 { return 400 })
			}

			assert.Equal(t, tt.want, engine.observed)
		})
	}
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

	w := workload
	require.NotNil(t, w.Status.Prediction)
	assert.Equal(t, "Active", w.Status.Prediction.DailyPhase)
	assert.Equal(t, "Suggesting", w.Status.Prediction.WeeklyPhase)
	assert.Equal(t, 91, w.Status.Prediction.DailyConfidence)
	assert.Equal(t, 72, w.Status.Prediction.WeeklyConfidence)
}

// metricsCondition reads the condition from the in-memory workload: the
// automation step sets it, and the reconcile writes status once at the end.
func metricsCondition(workload *v1alpha1.ManagedWorkload) *metav1.Condition {
	return meta.FindStatusCondition(workload.Status.Conditions, conditionMetricsAvailable)
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
				metrics: &stubMetrics{err: tt.err, replicas: 2},
			})

			for range 2 {
				result, err := r.reconcileAutomation(context.Background(), workload, nil)
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, 1*time.Minute, result.RequeueAfter)
			}

			assert.Equal(t, 0, engine.observeCalls)
			cond := metricsCondition(workload)
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
	r := newAutomationReconciler(t, workload, engine, automationOpts{})
	r.metrics = &zeroReplicaMetrics{stubMetrics{err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics)}}

	for now := fixedTime; !now.After(fixedTime.Add(time.Hour)); now = now.Add(time.Minute) {
		r.clock = func() time.Time { return now }
		_, err := r.reconcileAutomation(context.Background(), workload, nil)
		require.NoError(t, err)
	}

	assert.Equal(t, []float64{0}, engine.observed, "a target scaled to zero is an observation of zero demand")
	cond := metricsCondition(workload)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestAutomation_MetricsConditionRecovers(t *testing.T) {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	engine := &stubForecaster{phase: forecast.Observing}
	metrics := &stubMetrics{err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics), replicas: 2}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: metrics})

	_, err := r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)
	require.Equal(t, metav1.ConditionFalse, metricsCondition(workload).Status)

	metrics.err = nil
	metrics.cpuMillis = 120
	_, err = r.reconcileAutomation(context.Background(), workload, nil)
	require.NoError(t, err)

	assert.Equal(t, metav1.ConditionTrue, metricsCondition(workload).Status)
}

func pausedForecastWorkload() *v1alpha1.ManagedWorkload {
	w := automationWorkload(v1alpha1.PhasePaused)
	w.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{AutoResume: true}
	w.Status.Pause = &v1alpha1.PauseStatus{
		PreviousReplicas: 1,
		PausedAt:         ptr.To(metav1.NewTime(fixedTime.Add(-6 * time.Hour))),
		Resources:        &v1alpha1.ResourceSnapshot{CPUMillis: 100, Replicas: 1},
	}
	return w
}

// Behind the doorman a request would wake the workload, so an hour spent
// paused is an hour nobody asked for it, and an hour a request woke it in
// is busy, however little it went on to use: at least twice the activity
// threshold of what it requested, 2 x 10% of 100m.
func TestPausedHour_DemandBehindTheDoorman(t *testing.T) {
	tests := []struct {
		name   string
		routed bool
		manual bool
		wakeAt time.Duration
		want   []float64
	}{
		{name: "paused all hour", routed: true, want: []float64{0}},
		{name: "woken by a request", routed: true, wakeAt: 10 * time.Minute, want: []float64{20}},
		{name: "not routed, so demand can't be seen", routed: false},
		{name: "paused by desiredState, which a request doesn't end", routed: true, manual: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := pausedForecastWorkload()
			workload.Spec.IdlePolicy.AutoResume = false
			if tt.manual {
				workload.Spec.DesiredState = desiredState(v1alpha1.DesiredStatePaused)
			}
			if tt.routed {
				meta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{
					Type: conditionWakeOnRequest, Status: metav1.ConditionTrue, Reason: "DoormanRouted"})
			}
			engine := &stubForecaster{phase: forecast.Observing}
			metrics := &stubMetrics{cpuMillis: 1, cpuPerReplica: 100}
			r := newAutomationReconciler(t, workload, engine, automationOpts{
				metrics: metrics, pauser: &stubPauser{resumeDone: true}})

			for now := fixedTime; !now.After(fixedTime.Add(time.Hour)); now = now.Add(time.Minute) {
				r.clock = func() time.Time { return now }
				if tt.wakeAt > 0 && now.Equal(fixedTime.Add(tt.wakeAt)) {
					workload.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: now.Format(time.RFC3339)}
				}
				_, err := r.reconcileAutomation(context.Background(), workload, nil)
				require.NoError(t, err)
			}

			assert.Equal(t, tt.want, engine.observed)
		})
	}
}

// Pre-waking starts before a predicted busy hour, so the workload is Ready
// when people arrive rather than starting as they do.
func TestAutoResume_WakesAheadOfPredictedDemand(t *testing.T) {
	busy, quiet := 50.0, 1.0 // millicores of the 100m requested: 50% and 1%
	tests := []struct {
		name       string
		at         time.Time
		phase      forecast.Phase
		thisHour   float64
		nextHour   float64
		wantResume bool
	}{
		{name: "busy this hour", at: fixedTime.Add(5 * time.Minute), phase: forecast.DailyActive,
			thisHour: busy, nextHour: quiet, wantResume: true},
		{name: "busy next hour, 10 minutes before it", at: fixedTime.Add(50 * time.Minute), phase: forecast.DailyActive,
			thisHour: quiet, nextHour: busy, wantResume: true},
		{name: "busy next hour, still 30 minutes away", at: fixedTime.Add(30 * time.Minute), phase: forecast.DailyActive,
			thisHour: quiet, nextHour: busy},
		{name: "quiet", at: fixedTime.Add(50 * time.Minute), phase: forecast.DailyActive,
			thisHour: quiet, nextHour: quiet},
		{name: "forecast not confident yet", at: fixedTime.Add(50 * time.Minute), phase: forecast.Observing,
			thisHour: busy, nextHour: busy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := pausedForecastWorkload()
			engine := &stubForecaster{phase: tt.phase, predictByHour: map[int]float64{0: tt.thisHour, 1: tt.nextHour}}
			pauser := &stubPauser{resumeDone: true}
			r := newAutomationReconciler(t, workload, engine, automationOpts{pauser: pauser})
			r.clock = func() time.Time { return tt.at }

			_, err := r.reconcileAutomation(context.Background(), workload, nil)
			require.NoError(t, err)

			assert.Equal(t, tt.wantResume, pauser.resumeCalls == 1)
		})
	}
}

// officeDay is a weekday's traffic: people arrive at 09:10 and leave at
// 17:30, keeping the workload at 40% of the 1000m it requests meanwhile.
func officeDay(now time.Time) bool {
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return false
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := now.Sub(day)
	return since >= 9*time.Hour+10*time.Minute && since < 17*time.Hour+30*time.Minute
}

// nextArrival is when people next arrive after now.
func nextArrival(now time.Time) time.Time {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for at := day.Add(9*time.Hour + 10*time.Minute); ; at = at.AddDate(0, 0, 1) {
		if at.After(now) && officeDay(at) {
			return at
		}
	}
}

// clockedPauser records when a pause begins, as the real one does, so
// that only activity since then wakes the workload.
type clockedPauser struct {
	stubPauser
	now func() time.Time
}

func (p *clockedPauser) Prepare(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if err := p.stubPauser.Prepare(ctx, workload); err != nil {
		return err
	}
	workload.Status.Pause.PausedAt = ptr.To(metav1.NewTime(p.now()))
	return nil
}

type lifecycleEvent struct {
	at     time.Time
	reason string
}

// TestAutoResume_LearnsToWakeBeforePeopleArrive runs a workload behind the
// doorman through six weeks of office hours, reconciled as the operator
// would: at the requeue each reconcile asks for, and when a request stamped
// by the doorman wakes it. At first people wake it themselves at 09:10.
// Once the forecast has learned the day, autoResume has it awake before
// 09:00, and the veto doesn't keep it awake into the evening.
func TestAutoResume_LearnsToWakeBeforePeopleArrive(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates six weeks of reconciles")
	}
	ctx := context.Background()
	monday := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	workload := forecastWorkload()
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{AutoResume: true}
	workload.Status.Activity = nil
	meta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{
		Type: conditionWakeOnRequest, Status: metav1.ConditionTrue, Reason: "DoormanRouted"})
	fr := newForecastReconciler(t, workload, nil)
	fr.now = monday
	fr.pauser = &clockedPauser{stubPauser: stubPauser{pauseDone: true, resumeDone: true}, now: fr.Reconciler.now}
	metrics := &stubMetrics{cpuPerReplica: 1000, replicas: 1}
	fr.metrics = metrics
	recorder := events.NewFakeRecorder(100)
	fr.Recorder = recorder
	target := clockTarget("app:v1", nil)

	var history []lifecycleEvent
	for end := monday.AddDate(0, 0, 42); fr.now.Before(end); {
		require.NoError(t, fr.Get(ctx, client.ObjectKeyFromObject(workload), workload))
		paused := workload.Status.Phase == v1alpha1.PhasePaused
		metrics.cpuMillis = 5
		if officeDay(fr.now) {
			metrics.cpuMillis = 400
			if paused {
				workload.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: fr.now.Format(time.RFC3339)}
				require.NoError(t, fr.Update(ctx, workload))
			}
		}
		if woke := workload.Status.LastActedAt; !paused && woke != nil && fr.now.Sub(woke.Time) < 3*time.Minute {
			metrics.cpuMillis = 800
		}

		observed := workload.Status.DeepCopy()
		result, err := fr.reconcileAutomation(ctx, workload, target)
		require.NoError(t, err, "at %s", fr.now)
		require.NoError(t, fr.persistStatus(ctx, workload, observed))
		for len(recorder.Events) > 0 {
			e := <-recorder.Events
			for _, reason := range []string{ReasonAutoResume, ReasonWokeByActivity, ReasonPaused} {
				if strings.HasPrefix(e, "Normal "+reason+" ") {
					history = append(history, lifecycleEvent{at: fr.now, reason: reason})
				}
			}
		}

		require.NotNil(t, result, "at %s", fr.now)
		next := fr.now.Add(result.RequeueAfter)
		if arrival := nextArrival(fr.now); workload.Status.Phase == v1alpha1.PhasePaused && arrival.Before(next) {
			next = arrival
		}
		fr.now = next
	}

	for _, e := range history {
		if e.reason == ReasonAutoResume {
			t.Logf("autoResume first woke the workload ahead of demand on %s", e.at.Format(time.RFC1123))
			break
		}
	}
	day := func(d int) []lifecycleEvent {
		from := monday.AddDate(0, 0, d)
		var got []lifecycleEvent
		for _, e := range history {
			if !e.at.Before(from) && e.at.Before(from.AddDate(0, 0, 1)) {
				got = append(got, e)
			}
		}
		return got
	}
	at := func(d int, clock time.Duration) time.Time { return monday.AddDate(0, 0, d).Add(clock) }

	assert.Equal(t, []lifecycleEvent{
		{at: at(1, 9*time.Hour+10*time.Minute), reason: ReasonWokeByActivity},
		{at: at(1, 18*time.Hour+29*time.Minute), reason: ReasonPaused},
	}, day(1), "the first Tuesday, people wake it as they arrive")

	for d := 35; d < 40; d++ {
		got := day(d)
		require.Len(t, got, 2, "%s of the last week: woken once and paused once, got %v", at(d, 0).Weekday(), got)
		wake, pause := got[0], got[1]
		assert.Equal(t, ReasonAutoResume, wake.reason, "%s", wake.at)
		assert.Equal(t, at(d, 9*time.Hour-autoResumeLead), wake.at, "awake before people arrive at 09:10")
		assert.Equal(t, ReasonPaused, pause.reason)
		assert.Equal(t, at(d, 18*time.Hour+29*time.Minute), pause.at,
			"paused an idleAfter after the last busy minute: the veto doesn't hold it into the evening")
	}
	assert.Empty(t, day(40), "Saturday is left asleep")
	assert.Empty(t, day(41), "and Sunday")
}
