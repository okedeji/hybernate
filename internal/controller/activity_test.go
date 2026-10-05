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
	"strings"
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

func TestActivityClock_DoesNotActWithoutCPUData(t *testing.T) {
	tests := []struct {
		name       string
		metrics    stubMetrics
		wantReason string
	}{
		{name: "metrics API down", metrics: stubMetrics{err: errors.New("metrics API unavailable")}, wantReason: "MetricsUnavailable"},
		{name: "no CPU requests", metrics: stubMetrics{cpuMillis: 50, replicas: 1}, wantReason: "NoCPURequests"},
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
		{name: "confident forecast of demand defers the pause", engine: &stubForecaster{phase: forecast.DailyActive, predictValue: 300}},
		{name: "confident forecast of no demand", engine: &stubForecaster{phase: forecast.DailyActive, predictValue: 20}, wantPause: true},
		{name: "unconfident forecast is ignored", engine: &stubForecaster{phase: forecast.DailySuggesting, predictValue: 300}, wantPause: true},
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
	engine := &stubForecaster{phase: forecast.DailyActive, predictValue: 300}
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

	engine.predictValue = 20
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

func TestForecastHourAhead(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	r := &Reconciler{clock: func() time.Time { return fixedTime.Add(10 * time.Minute) }}

	assert.Equal(t, fixedTime.Add(time.Hour), r.forecastHourAhead().UTC(), "13:00 UTC, the hour an hour from 12:10")

	r.Timezone = kolkata
	assert.Equal(t, fixedTime.Add(30*time.Minute), r.forecastHourAhead().UTC(),
		"18:00 in Kolkata: its hours start at half past in UTC")
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
		wantWake   bool
		wantSource v1alpha1.ActivitySource
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
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := clockWorkload(fixedTime.Add(-3*time.Hour), tt.target)
			workload.Annotations = tt.onWorkload
			workload.Status.Phase = v1alpha1.PhasePaused
			workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2, PausedAt: &pausedAt}

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
			r.prometheusURL = tt.endpoint(t)

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
