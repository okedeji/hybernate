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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
	"github.com/okedeji/hybernate/internal/signal"
)

// idleCPU is usage well under the 10% default threshold: 5m of 1000m.
var idleCPU = stubMetrics{cpuMillis: 5, cpuPerReplica: 1000, replicas: 1}

func clockTarget(image string, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
			},
		},
	}
}

// clockWorkload is a running workload whose last activity was lastActivity,
// last evaluated a moment ago, and already fingerprinted against target.
func clockWorkload(lastActivity time.Time, target *appsv1.Deployment) *v1alpha1.ManagedWorkload {
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{
		IdleAfter: &metav1.Duration{Duration: time.Hour},
	}
	evaluated := metav1.NewTime(fixedTime.Add(-30 * time.Second))
	workload.Status.Activity = &v1alpha1.ActivityStatus{
		LastActivityTime:   metav1.NewTime(lastActivity),
		LastActivitySource: v1alpha1.ActivitySourceCPU,
		LastEvaluatedTime:  &evaluated,
		TemplateHash:       podTemplateHash(target),
	}
	return workload
}

type clockOpts struct {
	metrics stubMetrics
	engine  *stubForecaster
	pauser  *stubPauser
}

func runClock(t *testing.T, workload *v1alpha1.ManagedWorkload, target *appsv1.Deployment, opts clockOpts) *stubPauser {
	t.Helper()
	if opts.engine == nil {
		opts.engine = &stubForecaster{phase: forecast.Observing}
	}
	if opts.pauser == nil {
		opts.pauser = &stubPauser{pauseDone: true, resumeDone: true}
	}
	metrics := opts.metrics
	r := newAutomationReconciler(t, workload, opts.engine, automationOpts{metrics: &metrics, pauser: opts.pauser})
	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	return opts.pauser
}

func TestActivityClock_NewWorkloadStartsTheClock(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := automationWorkload(v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{}

	pauser := runClock(t, workload, target, clockOpts{metrics: idleCPU})

	activity := workload.Status.Activity
	require.NotNil(t, activity)
	assert.Equal(t, v1alpha1.ActivitySourceCreated, activity.LastActivitySource)
	assert.True(t, activity.PauseAt.Time.Equal(fixedTime.Add(defaultIdleAfter)), "pause is due a full idleAfter after creation")
	assert.Equal(t, podTemplateHash(target), activity.TemplateHash)
	assert.Equal(t, 0, pauser.pauseCalls)
}

func TestActivityClock_PausesAfterIdleAfter(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)

	pauser := runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.Equal(t, 1, pauser.pauseCalls)
	assert.Equal(t, v1alpha1.PhasePaused, workload.Status.Phase)
}

func TestActivityClock_StaysAwake(t *testing.T) {
	stale := fixedTime.Add(-61 * time.Minute)

	tests := []struct {
		name       string
		workload   func(target *appsv1.Deployment) *v1alpha1.ManagedWorkload
		target     *appsv1.Deployment
		metrics    stubMetrics
		wantSource v1alpha1.ActivitySource
	}{
		{
			name:   "idleAfter not reached",
			target: clockTarget("app:v1", nil),
			workload: func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload {
				return clockWorkload(fixedTime.Add(-59*time.Minute), d)
			},
			metrics: idleCPU,
		},
		{
			name:       "CPU above threshold",
			target:     clockTarget("app:v1", nil),
			workload:   func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload { return clockWorkload(stale, d) },
			metrics:    stubMetrics{cpuMillis: 500, cpuPerReplica: 1000, replicas: 1},
			wantSource: v1alpha1.ActivitySourceCPU,
		},
		{
			name:     "one pod of three serving while the others are Pending",
			target:   clockTarget("app:v1", nil),
			workload: func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload { return clockWorkload(stale, d) },
			// 25% of the one pod's request; 8% of all three replicas'.
			metrics:    stubMetrics{cpuMillis: 250, cpuPerReplica: 1000, replicas: 3, measuredPods: 1},
			wantSource: v1alpha1.ActivitySourceCPU,
		},
		{
			name:   "rollout changed the pod template",
			target: clockTarget("app:v2", nil),
			workload: func(_ *appsv1.Deployment) *v1alpha1.ManagedWorkload {
				return clockWorkload(stale, clockTarget("app:v1", nil))
			},
			metrics:    idleCPU,
			wantSource: v1alpha1.ActivitySourceRollout,
		},
		{
			name: "last-activity annotation on the target",
			target: clockTarget("app:v1", map[string]string{
				v1alpha1.AnnotationLastActivity: fixedTime.Add(-5 * time.Minute).Format(time.RFC3339),
			}),
			workload:   func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload { return clockWorkload(stale, d) },
			metrics:    idleCPU,
			wantSource: v1alpha1.ActivitySourceAnnotation,
		},
		{
			name:   "last-activity annotation on the ManagedWorkload",
			target: clockTarget("app:v1", nil),
			workload: func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload {
				w := clockWorkload(stale, d)
				w.Annotations = map[string]string{
					v1alpha1.AnnotationLastActivity: fixedTime.Add(-5 * time.Minute).Format(time.RFC3339),
				}
				return w
			},
			metrics:    idleCPU,
			wantSource: v1alpha1.ActivitySourceAnnotation,
		},
		{
			name: "active-until hold",
			target: clockTarget("app:v1", map[string]string{
				v1alpha1.AnnotationActiveUntil: fixedTime.Add(time.Hour).Format(time.RFC3339),
			}),
			workload: func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload { return clockWorkload(stale, d) },
			metrics:  idleCPU,
		},
		{
			name:   "operator wasn't watching",
			target: clockTarget("app:v1", nil),
			workload: func(d *appsv1.Deployment) *v1alpha1.ManagedWorkload {
				w := clockWorkload(fixedTime.Add(-3*time.Hour), d)
				evaluated := metav1.NewTime(fixedTime.Add(-2 * time.Hour))
				w.Status.Activity.LastEvaluatedTime = &evaluated
				return w
			},
			metrics:    idleCPU,
			wantSource: v1alpha1.ActivitySourceUnobserved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := tt.workload(tt.target)

			pauser := runClock(t, workload, tt.target, clockOpts{metrics: tt.metrics})

			assert.Equal(t, 0, pauser.pauseCalls)
			assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
			if tt.wantSource != "" {
				assert.Equal(t, tt.wantSource, workload.Status.Activity.LastActivitySource)
				assert.True(t, workload.Status.Activity.PauseAt.After(fixedTime))
			}
		})
	}
}

func TestActivityClock_FutureLastActivityIsTreatedAsNow(t *testing.T) {
	target := clockTarget("app:v1", map[string]string{
		v1alpha1.AnnotationLastActivity: fixedTime.Add(30 * 24 * time.Hour).Format(time.RFC3339),
	})
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)

	runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.True(t, workload.Status.Activity.LastActivityTime.Time.Equal(fixedTime),
		"a far-future annotation must not hold the workload awake indefinitely")
}

func TestActivityClock_MalformedAnnotationIsIgnored(t *testing.T) {
	target := clockTarget("app:v1", map[string]string{v1alpha1.AnnotationLastActivity: "yesterday"})
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)

	pauser := runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.Equal(t, 1, pauser.pauseCalls)
}

// A value that isn't an RFC 3339 time, such as epoch seconds from a portal,
// never counts as activity, so the workload pauses while the tool thinks
// it's keeping it awake. It's reported once per value, not every check.
func TestActivityClock_MalformedAnnotationIsReportedOnce(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-30*time.Minute), target)
	workload.Annotations = map[string]string{v1alpha1.AnnotationLastActivity: "1773489600"}
	metrics := idleCPU
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics})
	check := func() {
		t.Helper()
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
	}

	check()
	check()
	emitted := drainEvents(t, r)
	assert.Equal(t, 1, strings.Count(emitted, ReasonInvalidActivityAnnotation), "reported once: %s", emitted)
	assert.Contains(t, emitted, `"1773489600"`)
	assert.Contains(t, emitted, "ManagedWorkload api")

	workload.Annotations[v1alpha1.AnnotationLastActivity] = "2026-03-14 11:59:00"
	check()
	assert.Contains(t, drainEvents(t, r), ReasonInvalidActivityAnnotation, "and again for a new value")
}

func TestActivityClock_DoesNotActWithoutCPUData(t *testing.T) {
	tests := []struct {
		name       string
		metrics    stubMetrics
		wantReason string
	}{
		{name: "metrics API down", metrics: stubMetrics{err: errors.New("metrics API unavailable")}, wantReason: "MetricsUnavailable"},
		{name: "no CPU requests", metrics: stubMetrics{cpuMillis: 50, replicas: 1, noCPURequests: true}, wantReason: "NoCPURequests"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := clockTarget("app:v1", nil)
			workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)

			pauser := runClock(t, workload, target, clockOpts{metrics: tt.metrics})

			assert.Equal(t, 0, pauser.pauseCalls, "without CPU data a busy workload looks idle, so don't act")
			cond := meta.FindStatusCondition(workload.Status.Conditions, conditionMetricsAvailable)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

// A source that can't be read hides whatever use it would have shown, so
// the time it's out isn't idle time: back, and seeing nothing, it doesn't
// find the outage idle and pause at once, but only once it has seen
// idleAfter of nothing.
func TestActivityClock_AnOutageIsNotIdleTime(t *testing.T) {
	const outage = 40 * time.Minute
	tests := []struct {
		name    string
		promQL  bool
		breakIt func(m *stubMetrics, promDown *atomic.Bool)
		fixIt   func(m *stubMetrics, promDown *atomic.Bool)
	}{
		{
			name:    "Prometheus down",
			promQL:  true,
			breakIt: func(_ *stubMetrics, promDown *atomic.Bool) { promDown.Store(true) },
			fixIt:   func(_ *stubMetrics, promDown *atomic.Bool) { promDown.Store(false) },
		},
		{
			name:    "metrics API down",
			breakIt: func(m *stubMetrics, _ *atomic.Bool) { m.err = errors.New("metrics API unavailable") },
			fixIt:   func(m *stubMetrics, _ *atomic.Bool) { m.err = nil },
		},
		{
			name:    "CPU requests unreadable",
			breakIt: func(m *stubMetrics, _ *atomic.Bool) { m.noCPURequests = true },
			fixIt:   func(m *stubMetrics, _ *atomic.Bool) { m.noCPURequests = false },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var promDown atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if promDown.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			}))
			t.Cleanup(srv.Close)

			target := clockTarget("app:v1", nil)
			workload := clockWorkload(fixedTime.Add(-50*time.Minute), target)
			if tt.promQL {
				workload.Spec.IdlePolicy.Activity = &v1alpha1.ActivitySpec{
					Prometheus: []v1alpha1.PrometheusActivity{{PromQL: `sum(rate(http_requests_total[5m]))`}},
				}
			}
			metrics := idleCPU
			pauser := &stubPauser{pauseDone: true}
			r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})
			r.prometheus = signal.NewPrometheus(srv.URL)
			now := fixedTime
			r.clock = func() time.Time { return now }
			check := func() {
				t.Helper()
				_, err := r.reconcileAutomation(context.Background(), workload, target)
				require.NoError(t, err)
			}

			tt.breakIt(&metrics, &promDown)
			for ; now.Before(fixedTime.Add(outage)); now = now.Add(activityCheckInterval) {
				check()
			}
			require.Zero(t, pauser.pauseCalls, "nothing is paused while a source is out")
			assert.Equal(t, v1alpha1.ActivitySourceUnobserved, workload.Status.Activity.LastActivitySource)

			tt.fixIt(&metrics, &promDown)
			// The client backs off a failed endpoint by the wall clock, which
			// this test's clock doesn't move.
			r.prometheus = signal.NewPrometheus(srv.URL)
			// The first check that reads every source finds nothing evaluated
			// since the outage began, so the clock starts over from it.
			recovered := now
			for ; now.Before(recovered.Add(time.Hour)); now = now.Add(activityCheckInterval) {
				check()
				require.Zero(t, pauser.pauseCalls, "paused at %s, within idleAfter of the outage ending",
					now.Sub(fixedTime))
			}
			check()
			assert.Equal(t, 1, pauser.pauseCalls, "paused once idleAfter has passed with every source seeing nothing")
		})
	}
}

// An activity annotation is stamped to the second, so one stamped in the
// second a workload went Idle ends the would-be pause: it can't be told from
// one just before, and keeping the workload awake is the safe mistake. The
// operator's own times are exact, so activity it saw before a pause request
// in the same second doesn't undo the request.
func TestActiveSinceClockRanOut_CountsTheSecondItRanOut(t *testing.T) {
	wentIdle := fixedTime.Add(400 * time.Millisecond)
	tests := []struct {
		name     string
		source   v1alpha1.ActivitySource
		activity time.Time
		want     bool
	}{
		{name: "an annotation before the second it went Idle", source: v1alpha1.ActivitySourceAnnotation,
			activity: fixedTime.Add(-time.Second)},
		{name: "an annotation stamped in that second", source: v1alpha1.ActivitySourceAnnotation,
			activity: fixedTime, want: true},
		{name: "an annotation after it", source: v1alpha1.ActivitySourceAnnotation,
			activity: fixedTime.Add(2 * time.Second), want: true},
		{name: "CPU just before it, in the same second", source: v1alpha1.ActivitySourceCPU,
			activity: fixedTime.Add(100 * time.Millisecond)},
		{name: "CPU after it", source: v1alpha1.ActivitySourceCPU, activity: wentIdle.Add(time.Second), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := clockWorkload(tt.activity, clockTarget("app:v1", nil))
			workload.Status.Activity.LastActivitySource = tt.source
			workload.Status.Phase = v1alpha1.PhaseIdle
			began := metav1.NewTime(wentIdle)
			workload.Status.LastTransitionTime = &began

			assert.Equal(t, tt.want, activeSinceClockRanOut(workload, wentIdle.Add(time.Minute), wentIdle))
		})
	}
}

// A woken workload's new pods have no metrics for the first scrape or so.
// That blind minute mustn't replace what woke it as its last activity: the
// wake by request is what status should show.
func TestActivityClock_AShortBlindSpotKeepsWhatWokeIt(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime, target)
	workload.Status.Activity.LastActivitySource = v1alpha1.ActivitySourceRequest
	evaluated := metav1.NewTime(fixedTime)
	workload.Status.Activity.LastEvaluatedTime = &evaluated
	metrics := idleCPU
	metrics.err = errors.New("no pod metrics yet")
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: &stubPauser{}})
	now := fixedTime
	r.clock = func() time.Time { return now }

	for range 2 {
		now = now.Add(activityCheckInterval / 2)
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
	}

	assert.Equal(t, v1alpha1.ActivitySourceRequest, workload.Status.Activity.LastActivitySource)
	assert.Equal(t, fixedTime, workload.Status.Activity.LastActivityTime.UTC())
}

// A gap in watching is never counted as idle time once it could bring the
// pause forward by much: a short idleAfter tolerates less of one, and after
// a restart, when the gap's start is unknown, little at all.
func TestActivityClock_AGapInWatchingRestartsTheClock(t *testing.T) {
	tests := []struct {
		name      string
		idleAfter time.Duration
		idleFor   time.Duration
		gap       time.Duration
		inMemory  bool
		wantPause bool
	}{
		{name: "an outage longer than half a short idleAfter", idleAfter: 5 * time.Minute, idleFor: 6 * time.Minute,
			gap: 3 * time.Minute, inMemory: true},
		{name: "a delayed check within a long idleAfter", idleAfter: time.Hour, idleFor: 61 * time.Minute,
			gap: 3 * time.Minute, inMemory: true, wantPause: true},
		{name: "an outage beyond the cap on a long idleAfter", idleAfter: 2 * time.Hour, idleFor: 121 * time.Minute,
			gap: unobservedGap + time.Minute, inMemory: true},
		{name: "a restart with the written evaluation minutes old", idleAfter: time.Hour, idleFor: 61 * time.Minute,
			gap: 3 * time.Minute},
		{name: "a restart with the written evaluation moments old", idleAfter: time.Hour, idleFor: 61 * time.Minute,
			gap: 30 * time.Second, wantPause: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := clockTarget("app:v1", nil)
			workload := clockWorkload(fixedTime.Add(-tt.idleFor), target)
			workload.Spec.IdlePolicy.IdleAfter = &metav1.Duration{Duration: tt.idleAfter}
			evaluated := metav1.NewTime(fixedTime.Add(-tt.gap))
			workload.Status.Activity.LastEvaluatedTime = &evaluated
			metrics := idleCPU
			pauser := &stubPauser{pauseDone: true}
			r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})
			if tt.inMemory {
				r.activityMemo.put(workload.UID, *workload.Status.Activity.DeepCopy())
			}

			_, err := r.reconcileAutomation(context.Background(), workload, target)
			require.NoError(t, err)

			assert.Equal(t, tt.wantPause, pauser.pauseCalls == 1)
			if !tt.wantPause {
				assert.Equal(t, v1alpha1.ActivitySourceUnobserved, workload.Status.Activity.LastActivitySource)
			}
		})
	}
}

func TestActivityClock_ZeroReplicasIsIdleNotMissingData(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	// Scaled to zero, the Metrics API has no pods to report on at all.
	metrics := &zeroReplicaMetrics{stubMetrics{err: fmt.Errorf("%w for default/api", opmetrics.ErrNoPodMetrics)}}
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{pauser: pauser})
	r.metrics = metrics

	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.pauseCalls)
}

func TestActivityClock_ForecastVeto(t *testing.T) {
	tests := []struct {
		name      string
		engine    *stubForecaster
		wantPause bool
	}{
		{name: "confident forecast of demand defers the pause", engine: &stubForecaster{phase: forecast.DailyActive, predictValue: 30}},
		{name: "confident forecast of no demand", engine: &stubForecaster{phase: forecast.DailyActive, predictValue: 2}, wantPause: true},
		{name: "unconfident forecast is ignored", engine: &stubForecaster{phase: forecast.DailySuggesting, predictValue: 30}, wantPause: true},
		// autoResume would wake it straight back for the rest of this hour.
		{name: "demand forecast for the rest of this hour", engine: &stubForecaster{phase: forecast.DailyActive,
			predictByHour: map[int]float64{0: 30, 1: 2}}},
		{name: "demand forecast for the next hour", engine: &stubForecaster{phase: forecast.DailyActive,
			predictByHour: map[int]float64{0: 2, 1: 30}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := clockTarget("app:v1", nil)
			workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)

			pauser := runClock(t, workload, target, clockOpts{metrics: idleCPU, engine: tt.engine})

			assert.Equal(t, tt.wantPause, pauser.pauseCalls == 1)
		})
	}
}

func vetoEvents(recorder *events.FakeRecorder) int {
	n := 0
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, ReasonIdleVetoed) {
			n++
		}
	}
	return n
}

// A veto lasts as long as the forecast expects demand, and the clock checks
// every minute meanwhile. The condition says so throughout; the event marks
// only its beginning.
func TestActivityClock_ForecastVetoIsACondition(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	engine := &stubForecaster{phase: forecast.DailyActive, predictByHour: map[int]float64{0: 2, 1: 30}}
	metrics := idleCPU
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: &metrics, pauser: pauser})
	recorder := r.Recorder.(*events.FakeRecorder)

	for range 3 {
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
	}

	cond := meta.FindStatusCondition(workload.Status.Conditions, conditionIdleVetoed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "ForecastExpectsDemand", cond.Reason)
	assert.Contains(t, cond.Message, "30% of requests in the hour from 13:00 UTC", "says when demand is expected")
	assert.Equal(t, 1, vetoEvents(recorder), "one event for the veto, not one a minute")
	assert.Zero(t, pauser.pauseCalls)
	written := getWorkload(t, r, "api")
	assert.True(t, meta.IsStatusConditionTrue(written.Status.Conditions, conditionIdleVetoed), "and it's written")

	engine.predictByHour[1] = 2
	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)

	assert.True(t, meta.IsStatusConditionFalse(workload.Status.Conditions, conditionIdleVetoed), "the veto is over")
	assert.Equal(t, 1, pauser.pauseCalls)
}

// The event follows the write that records the veto, so a conflict doesn't
// announce one veto twice.
func TestActivityClock_ForecastVetoEventFollowsTheWrite(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	engine := &stubForecaster{phase: forecast.DailyActive, predictValue: 300}
	metrics := idleCPU
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: &metrics})
	recorder := r.Recorder.(*events.FakeRecorder)
	fail := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if fail {
				return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("managedworkloads").GroupResource(), obj.GetName(), errors.New("stale"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}})

	_, err := r.reconcileAutomation(context.Background(), workload.DeepCopy(), target)
	require.Error(t, err)
	assert.Zero(t, vetoEvents(recorder), "nothing is announced until it's recorded")

	fail = false
	_, err = r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	assert.Equal(t, 1, vetoEvents(recorder))
}

// A dry-run workload gone Idle is measuring a would-be pause, which a real
// pause would already be. The forecast doesn't hold back a pause it's too
// late for, so it doesn't say it does.
func TestActivityClock_ForecastDoesNotVetoAPauseUnderWay(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.DryRun = true
	workload.Status.Phase = v1alpha1.PhaseIdle
	engine := &stubForecaster{phase: forecast.DailyActive, predictValue: 300}
	metrics := idleCPU
	r := newAutomationReconciler(t, workload, engine, automationOpts{metrics: &metrics})

	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)

	assert.Equal(t, v1alpha1.PhaseIdle, workload.Status.Phase)
	assert.False(t, meta.IsStatusConditionTrue(workload.Status.Conditions, conditionIdleVetoed))
	assert.Zero(t, vetoEvents(r.Recorder.(*events.FakeRecorder)))
}

// meshMetrics is a workload with an injected sidecar: its pod requests
// twice what its own containers do.
type meshMetrics struct{ stubMetrics }

func (m *meshMetrics) PodRequestsPerReplica(_ context.Context, _ *v1alpha1.ManagedWorkload) (float64, float64, error) {
	return 2 * m.cpuPerReplica, m.memoryPerReplica, nil
}

// The veto and autoResume judge the forecast by one measure, so a forecast
// that holds an idle workload awake wakes it once it's paused. Judged
// against the whole pod's request, sidecars included, the same forecast was
// half the utilization once paused, and never woke it.
func TestActivityClock_VetoAndAutoResumeAgree(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.IdlePolicy.AutoResume = true
	engine := &stubForecaster{phase: forecast.DailyActive, predictValue: 15}
	pauser := &stubPauser{pauseDone: true, resumeDone: true}
	r := newAutomationReconciler(t, workload, engine, automationOpts{pauser: pauser})
	r.metrics = &meshMetrics{stubMetrics{cpuMillis: 5, cpuPerReplica: 100, replicas: 1}}

	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	require.True(t, meta.IsStatusConditionTrue(workload.Status.Conditions, conditionIdleVetoed),
		"a forecast of 15%% holds it awake")

	engine.predictValue = 5
	_, err = r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	require.Equal(t, v1alpha1.PhasePaused, workload.Status.Phase)

	engine.predictValue = 15
	r.clock = func() time.Time { return fixedTime.Add(5 * time.Minute) }
	_, err = r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	assert.Equal(t, 1, pauser.resumeCalls, "the forecast that held it awake wakes it")
}

func TestHourStart(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	tests := []struct {
		name string
		loc  *time.Location
		at   time.Time
		want time.Time
	}{
		{name: "UTC", at: fixedTime.Add(10 * time.Minute), want: fixedTime},
		{name: "hours at half past in UTC", loc: kolkata, at: fixedTime.Add(10 * time.Minute),
			want: fixedTime.Add(-30 * time.Minute)},
		{name: "the first 1am of a clock change", loc: newYork, at: time.Date(2026, 11, 1, 5, 40, 0, 0, time.UTC),
			want: time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)},
		{name: "the repeated 1am", loc: newYork, at: time.Date(2026, 11, 1, 6, 40, 0, 0, time.UTC),
			want: time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{Timezone: tt.loc}
			assert.True(t, tt.want.Equal(r.hourStart(tt.at)), "got %s", r.hourStart(tt.at).UTC())
		})
	}
}

func TestActivityClock_DryRunReportsIdleOnceWithoutPausing(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.DryRun = true
	metrics := idleCPU
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})

	for range 2 {
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
	}

	assert.Equal(t, 0, pauser.pauseCalls)
	assert.Equal(t, v1alpha1.PhaseIdle, workload.Status.Phase)
	recorder, ok := r.Recorder.(*events.FakeRecorder)
	require.True(t, ok)
	assert.Len(t, recorder.Events, 1, "the would-pause event fires when the clock runs out, not every check")
}

func TestActivityClock_ActivityTakesIdleWorkloadBackToRunning(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.DryRun = true
	workload.Status.Phase = v1alpha1.PhaseIdle

	runClock(t, workload, target, clockOpts{metrics: stubMetrics{cpuMillis: 500, cpuPerReplica: 1000, replicas: 1}})

	assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
}

func TestActivityClock_AnnotationWakesPausedWorkload(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-2 * time.Hour))

	tests := []struct {
		name       string
		target     *appsv1.Deployment
		onWorkload map[string]string
		// beforePause says the annotations were already there when the
		// pause began, so its record holds them.
		beforePause bool
		wantWake    bool
		wantSource  v1alpha1.ActivitySource
	}{
		{
			name: "last-activity newer than the pause",
			target: clockTarget("app:v1", map[string]string{
				v1alpha1.AnnotationLastActivity: fixedTime.Add(-time.Minute).Format(time.RFC3339),
			}),
			wantWake:   true,
			wantSource: v1alpha1.ActivitySourceWoke,
		},
		{
			name:   "a request the doorman is holding",
			target: clockTarget("app:v1", nil),
			onWorkload: map[string]string{
				v1alpha1.AnnotationLastRequest: fixedTime.Add(-time.Second).Format(time.RFC3339),
			},
			wantWake:   true,
			wantSource: v1alpha1.ActivitySourceRequest,
		},
		{
			name:   "a request from before the pause",
			target: clockTarget("app:v1", nil),
			onWorkload: map[string]string{
				v1alpha1.AnnotationLastRequest: fixedTime.Add(-3 * time.Hour).Format(time.RFC3339),
			},
			beforePause: true,
		},
		{
			name: "active-until in the future",
			target: clockTarget("app:v1", map[string]string{
				v1alpha1.AnnotationActiveUntil: fixedTime.Add(time.Hour).Format(time.RFC3339),
			}),
			wantWake:   true,
			wantSource: v1alpha1.ActivitySourceWoke,
		},
		{
			name: "last-activity from before the pause",
			target: clockTarget("app:v1", map[string]string{
				v1alpha1.AnnotationLastActivity: fixedTime.Add(-3 * time.Hour).Format(time.RFC3339),
			}),
			beforePause: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := clockWorkload(fixedTime.Add(-3*time.Hour), tt.target)
			workload.Annotations = tt.onWorkload
			workload.Status.Phase = v1alpha1.PhasePaused
			recorded := &v1alpha1.WakeAnnotations{}
			if tt.beforePause {
				recorded.Workload = activityAnnotationValues(workload)
				recorded.Target = activityAnnotationValues(tt.target)
			}
			workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2, PausedAt: &pausedAt,
				WakeAnnotations: recorded}

			pauser := runClock(t, workload, tt.target, clockOpts{metrics: idleCPU})

			assert.Equal(t, tt.wantWake, pauser.resumeCalls == 1)
			if tt.wantWake {
				assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
				assert.Equal(t, tt.wantSource, workload.Status.Activity.LastActivitySource,
					"the clock records what woke it")
				assert.True(t, workload.Status.Activity.LastActivityTime.Time.Equal(fixedTime))
			}
		})
	}
}

func TestPodTemplateHash(t *testing.T) {
	base := clockTarget("app:v1", nil)

	scaled := clockTarget("app:v1", nil)
	scaled.Spec.Replicas = ptr.To(int32(0))
	scaled.Generation = 7
	assert.Equal(t, podTemplateHash(base), podTemplateHash(scaled), "pausing and resuming must not look like a deploy")

	assert.NotEqual(t, podTemplateHash(base), podTemplateHash(clockTarget("app:v2", nil)))
}

// The hash is worked out once for each version of the target, not on each
// of the checks that need it.
func TestTemplateHashes(t *testing.T) {
	var hashes templateHashes
	target := clockTarget("app:v1", nil)
	target.UID, target.ResourceVersion = "deploy-uid", "1"
	v1 := hashes.of("workload-uid", target)
	require.Equal(t, podTemplateHash(target), v1)

	target.Spec.Template.Spec.Containers[0].Image = "app:v2"
	assert.Equal(t, v1, hashes.of("workload-uid", target), "the same version isn't hashed again")

	target.ResourceVersion = "2"
	assert.Equal(t, podTemplateHash(target), hashes.of("workload-uid", target), "a new version is")
	assert.NotEqual(t, v1, hashes.of("workload-uid", target))

	recreated := clockTarget("app:v3", nil)
	recreated.UID, recreated.ResourceVersion = "recreated-uid", "2"
	assert.Equal(t, podTemplateHash(recreated), hashes.of("workload-uid", recreated), "and so is a new target")

	unsaved := clockTarget("app:v4", nil)
	assert.Equal(t, podTemplateHash(unsaved), hashes.of("workload-uid", unsaved))
}

func TestNextCheck(t *testing.T) {
	now := fixedTime
	assert.Equal(t, activityCheckInterval, nextCheck(now, now.Add(time.Hour), time.Time{}))
	assert.Equal(t, 20*time.Second, nextCheck(now, now.Add(20*time.Second), time.Time{}))
	assert.Equal(t, 10*time.Second, nextCheck(now, now.Add(time.Hour), now.Add(10*time.Second)))
	assert.Equal(t, activityCheckInterval, nextCheck(now, now.Add(-time.Minute), time.Time{}), "an overdue deadline falls back to the poll")
}

// A workload whose clock predates fingerprinting (or whose target couldn't be
// read before) has no stored hash. Seeing the template for the first time is
// not a deploy, so it must not hold the workload awake.
func TestActivityClock_FirstFingerprintIsNotARollout(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Status.Activity.TemplateHash = ""

	pauser := runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.Equal(t, 1, pauser.pauseCalls)
	assert.Equal(t, podTemplateHash(target), workload.Status.Activity.TemplateHash)
}

func prometheusServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func promValue(v string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1234567890,"` + v + `"]}]}}`
}

func TestActivityClock_PrometheusSource(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   func(t *testing.T) string
		wantPause  bool
		wantSource v1alpha1.ActivitySource
		wantReason string
	}{
		{
			name:       "requests flowing keeps it awake",
			endpoint:   func(t *testing.T) string { return prometheusServer(t, http.StatusOK, promValue("3.5")) },
			wantSource: v1alpha1.ActivitySourcePrometheus,
		},
		{
			name:      "zero requests lets it pause",
			endpoint:  func(t *testing.T) string { return prometheusServer(t, http.StatusOK, promValue("0")) },
			wantPause: true,
		},
		{
			name: "empty result lets it pause",
			endpoint: func(t *testing.T) string {
				return prometheusServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			},
			wantPause: true,
		},
		{
			name:       "no endpoint configured",
			endpoint:   func(*testing.T) string { return "" },
			wantReason: "EndpointNotConfigured",
		},
		{
			name:       "query fails",
			endpoint:   func(t *testing.T) string { return prometheusServer(t, http.StatusInternalServerError, "") },
			wantReason: "QueryFailed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := clockTarget("app:v1", nil)
			workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
			workload.Spec.IdlePolicy.Activity = &v1alpha1.ActivitySpec{
				Prometheus: []v1alpha1.PrometheusActivity{{PromQL: `sum(rate(nginx_ingress_controller_requests[5m]))`}},
			}
			metrics := idleCPU
			pauser := &stubPauser{pauseDone: true}
			r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})
			r.prometheus = signal.NewPrometheus(tt.endpoint(t))

			_, err := r.reconcileAutomation(context.Background(), workload, target)
			require.NoError(t, err)

			assert.Equal(t, tt.wantPause, pauser.pauseCalls == 1)
			if tt.wantSource != "" {
				assert.Equal(t, tt.wantSource, workload.Status.Activity.LastActivitySource)
			}
			cond := meta.FindStatusCondition(workload.Status.Conditions, conditionPrometheusAvailable)
			require.NotNil(t, cond)
			if tt.wantReason == "" {
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				return
			}
			assert.Equal(t, metav1.ConditionFalse, cond.Status, "a source the clock can't read must stop it from pausing")
			assert.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

func promQueries(n int) []v1alpha1.PrometheusActivity {
	queries := make([]v1alpha1.PrometheusActivity, n)
	for i := range queries {
		queries[i] = v1alpha1.PrometheusActivity{PromQL: fmt.Sprintf(`sum(rate(http_requests_total{route="%d"}[5m]))`, i)}
	}
	return queries
}

// Prometheus failing for one check isn't asked again by the next: each
// would wait out a timeout to learn of the outage, holding a reconcile
// worker that wakes queue behind.
func TestActivityClock_PrometheusOutageFailsFast(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.IdlePolicy.Activity = &v1alpha1.ActivitySpec{Prometheus: promQueries(3)}
	metrics := idleCPU
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})
	r.prometheus = signal.NewPrometheus(srv.URL)

	for range 3 {
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
	}

	assert.Equal(t, int32(1), calls.Load(), "asked once, then not again until it has had time to recover")
	assert.Zero(t, pauser.pauseCalls)
	cond := meta.FindStatusCondition(workload.Status.Conditions, conditionPrometheusAvailable)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

// A workload's queries share one time budget per check, so one with many
// slow queries holds a reconcile worker for seconds, not a timeout each.
func TestActivityClock_PrometheusQueriesShareABudget(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the budget")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1200 * time.Millisecond):
			_, _ = w.Write([]byte(promValue("0")))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), target)
	workload.Spec.IdlePolicy.Activity = &v1alpha1.ActivitySpec{Prometheus: promQueries(8)}
	metrics := idleCPU
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics, pauser: pauser})
	r.prometheus = signal.NewPrometheus(srv.URL)

	began := time.Now()
	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)

	assert.Less(t, time.Since(began), prometheusBudget+time.Second, "8 queries of 1.2s each stopped at the budget")
	assert.Zero(t, pauser.pauseCalls, "queries not evaluated leave the workload awake")
	cond := meta.FindStatusCondition(workload.Status.Conditions, conditionPrometheusAvailable)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
}

// A query that matches no series, such as one with a misspelt metric, looks
// like a quiet workload, and would never keep it awake. It's pointed out
// once, and the clock goes on as before.
func TestActivityClock_QueryThatNeverMatchesIsPointedOut(t *testing.T) {
	var matching atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if matching.Load() {
			_, _ = w.Write([]byte(promValue("0")))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-30*time.Minute), target)
	query := `sum(rate(nginx_ingress_controller_requets[5m]))`
	workload.Spec.IdlePolicy.Activity = &v1alpha1.ActivitySpec{Prometheus: []v1alpha1.PrometheusActivity{{PromQL: query}}}
	metrics := idleCPU
	r := newAutomationReconciler(t, workload, &stubForecaster{}, automationOpts{metrics: &metrics})
	r.prometheus = signal.NewPrometheus(srv.URL)
	check := func() *metav1.Condition {
		t.Helper()
		_, err := r.reconcileAutomation(context.Background(), workload, target)
		require.NoError(t, err)
		return meta.FindStatusCondition(workload.Status.Conditions, conditionPrometheusQueryEmpty)
	}

	cond := check()
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, query)
	assert.True(t, meta.IsStatusConditionTrue(workload.Status.Conditions, conditionPrometheusAvailable),
		"the query was evaluated: it isn't a failure")
	check()
	emitted := drainEvents(t, r)
	assert.Equal(t, 1, strings.Count(emitted, ReasonPrometheusQueryEmpty), "pointed out once: %s", emitted)

	matching.Store(true)
	cond = check()
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "cleared once the query matches a series")
	matching.Store(false)
	cond = check()
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "a query that has matched may go quiet")
	assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
}

func TestActivityClock_NoPrometheusConditionWithoutQueries(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-30*time.Minute), target)

	runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.Nil(t, meta.FindStatusCondition(workload.Status.Conditions, conditionPrometheusAvailable))
}

// A paused workload wakes for any activity annotation set since the pause
// began, whatever time it states: the doorman stamps to the second, so a
// request can share the pause's second, and kubectl hybernate wake stamps
// from a laptop clock that may be behind.
func TestWokenByActivity(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	old := fixedTime.Add(-2 * time.Hour).Format(time.RFC3339)
	skewed := fixedTime.Add(-90 * time.Minute).Format(time.RFC3339)
	tests := []struct {
		name        string
		pausedAt    metav1.Time
		recorded    *v1alpha1.WakeAnnotations
		annotations map[string]string
		want        bool
	}{
		{name: "a request in the second the pause completed", pausedAt: metav1.NewTime(fixedTime.Truncate(time.Second)),
			annotations: map[string]string{v1alpha1.AnnotationLastRequest: fixedTime.Format(time.RFC3339)}, want: true},
		{name: "a wake stamped by a clock behind the operator's", pausedAt: pausedAt,
			recorded:    &v1alpha1.WakeAnnotations{Workload: map[string]string{v1alpha1.AnnotationLastActivity: old}},
			annotations: map[string]string{v1alpha1.AnnotationLastActivity: skewed}, want: true},
		{name: "the activity it was paused after", pausedAt: pausedAt,
			recorded:    &v1alpha1.WakeAnnotations{Workload: map[string]string{v1alpha1.AnnotationLastActivity: old}},
			annotations: map[string]string{v1alpha1.AnnotationLastActivity: old}},
		{name: "no annotations", pausedAt: pausedAt, recorded: &v1alpha1.WakeAnnotations{}},
		{name: "a value that isn't a time, as an awake workload ignores it", pausedAt: pausedAt,
			recorded:    &v1alpha1.WakeAnnotations{},
			annotations: map[string]string{v1alpha1.AnnotationLastActivity: strconv.FormatInt(fixedTime.Unix(), 10)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := pausedWorkload(v1alpha1.PhasePaused)
			workload.Status.Pause.PausedAt = &tt.pausedAt
			workload.Status.Pause.WakeAnnotations = tt.recorded
			workload.Annotations = tt.annotations
			pauser := &stubPauser{resumeDone: true}
			r := newTestReconcilerWithReplicas(t, workload, pauser, 0)

			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)

			assert.Equal(t, tt.want, pauser.resumeCalls > 0)
		})
	}
}
