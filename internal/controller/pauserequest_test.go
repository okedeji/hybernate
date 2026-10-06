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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
)

const pauseToken = "2026-03-14T11:59:30.123456789Z"

// requestedPause is a running workload, active five minutes ago, with an
// hour's idle clock, whose pause has been requested.
func requestedPause() *v1alpha1.ManagedWorkload {
	w := lifecycleWorkload("api", nil, v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: pauseToken}
	w.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	w.Status.LastTransitionTime = ptr.To(metav1.NewTime(fixedTime.Add(-3 * time.Hour)))
	w.Status.Activity = &v1alpha1.ActivityStatus{
		LastActivityTime:   metav1.NewTime(fixedTime.Add(-5 * time.Minute)),
		LastActivitySource: v1alpha1.ActivitySourceCPU,
		LastEvaluatedTime:  ptr.To(metav1.NewTime(fixedTime.Add(-30 * time.Second))),
	}
	return w
}

func pauseRequestCondition(t *testing.T, w *v1alpha1.ManagedWorkload) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(w.Status.Conditions, conditionPauseRequest)
	require.NotNil(t, c, "the PauseRequest condition says what came of the request")
	return c
}

func patchWorkload(t *testing.T, r *Reconciler, name string, mutate func(*v1alpha1.ManagedWorkload)) {
	t.Helper()
	w := getWorkload(t, r, name)
	patch := client.MergeFrom(w.DeepCopy())
	mutate(w)
	require.NoError(t, r.Patch(context.Background(), w, patch))
}

func patchTarget(t *testing.T, r *Reconciler, mutate func(*appsv1.Deployment)) {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "api"}, &d))
	patch := client.MergeFrom(d.DeepCopy())
	mutate(&d)
	require.NoError(t, r.Patch(context.Background(), &d, patch))
}

// A requested pause is an ordinary pause, made now rather than when the
// idle clock runs out, with or without an idle policy, and acted on once.
func TestPauseRequest_PausesNowAndOnce(t *testing.T) {
	for _, withPolicy := range []bool{true, false} {
		t.Run(map[bool]string{true: "with an idle policy", false: "without one"}[withPolicy], func(t *testing.T) {
			workload := requestedPause()
			if !withPolicy {
				workload.Spec.IdlePolicy = nil
			}
			r := lifecycleReconciler(t, workload, 3, interceptor.Funcs{})

			reconcileUntilSettled(t, r)

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase)
			assert.Equal(t, int32(0), targetReplicas(t, r))
			assert.Equal(t, int32(3), got.Status.Pause.PreviousReplicas)
			assert.Equal(t, pauseToken, got.Status.LastPauseRequest)
			c := pauseRequestCondition(t, got)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, reasonPausing, c.Reason)
			recorded := drainEvents(t, r)
			assert.Equal(t, 1, strings.Count(recorded, "Normal PauseRequested api: pause requested, pausing"), recorded)
			assert.Equal(t, 1, strings.Count(recorded, "Normal Paused api: paused"), recorded)
		})
	}
}

// The handled request is in status, so an operator that restarts, or a
// workload woken since, doesn't act on it again.
func TestPauseRequest_HandledRequestIsntActedOnAgain(t *testing.T) {
	workload := requestedPause()
	workload.Status.LastPauseRequest = pauseToken
	r := lifecycleReconciler(t, workload, 3, interceptor.Funcs{})

	reconcileUntilSettled(t, r)

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Equal(t, int32(3), targetReplicas(t, r))
	assert.NotContains(t, drainEvents(t, r), ReasonPauseRequested)
}

// A requested pause wakes like an idle one: its Services route to the
// doorman, and a request the doorman holds wakes it to every replica it
// had, after which the old request isn't acted on again.
func TestPauseRequest_WakesOnARequest(t *testing.T) {
	r := lifecycleReconciler(t, requestedPause(), 3, interceptor.Funcs{})
	reconcileUntilSettled(t, r)
	paused := getWorkload(t, r, "api")
	require.Equal(t, v1alpha1.PhasePaused, paused.Status.Phase)
	assert.True(t, doormanEligible(paused), "its Services route to the doorman")

	patchWorkload(t, r, "api", func(w *v1alpha1.ManagedWorkload) {
		w.Annotations[v1alpha1.AnnotationLastRequest] = fixedTime.Format(time.RFC3339)
	})
	reconcileUntilSettled(t, r)

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Equal(t, int32(3), targetReplicas(t, r))
	assert.Equal(t, v1alpha1.ActivitySourceRequest, got.Status.Activity.LastActivitySource)
	assert.Equal(t, pauseToken, got.Status.LastPauseRequest)
}

// Activity stamped before the request is what the request overrides, so it
// doesn't wake the workload the moment it's paused: not an earlier
// last-activity or last-request, and not a last-activity from a clock ahead
// of the operator's. Only a change made since the pause does.
func TestPauseRequest_EarlierActivityDoesntWake(t *testing.T) {
	// The real clock, which the lifecycle stamps pausedAt with.
	start := time.Now()
	workload := requestedPause()
	workload.Annotations[v1alpha1.AnnotationLastActivity] = start.Add(-10 * time.Minute).Format(time.RFC3339)
	workload.Annotations[v1alpha1.AnnotationLastRequest] = start.Add(-5 * time.Minute).Format(time.RFC3339)
	r := lifecycleReconciler(t, workload, 3, interceptor.Funcs{})
	r.clock = time.Now
	patchTarget(t, r, func(d *appsv1.Deployment) {
		d.Annotations = map[string]string{v1alpha1.AnnotationLastActivity: start.Add(2 * time.Minute).Format(time.RFC3339)}
	})

	reconcileUntilSettled(t, r)

	assert.Equal(t, v1alpha1.PhasePaused, getWorkload(t, r, "api").Status.Phase)
	assert.Equal(t, int32(0), targetReplicas(t, r))

	patchTarget(t, r, func(d *appsv1.Deployment) {
		d.Annotations[v1alpha1.AnnotationLastActivity] = start.Add(3 * time.Minute).Format(time.RFC3339)
	})
	reconcileUntilSettled(t, r)

	assert.Equal(t, v1alpha1.PhaseRunning, getWorkload(t, r, "api").Status.Phase)
}

// A request Hybernate won't act on is answered once, with the reason, so
// it isn't tried again on every reconcile, and the workload is left up.
func TestPauseRequest_Refused(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*v1alpha1.ManagedWorkload)
		replicas   int32
		objs       []client.Object
		setup      func(*testing.T, *Reconciler)
		wantReason string
		wantEvent  string
	}{
		{
			name: "protected namespace", replicas: 3, objs: []client.Object{defaultNamespace(protectedLabel)},
			wantReason: ReasonProtected,
			wantEvent:  "Warning Protected api: pause requested, but namespace default is protected",
		},
		{
			name: "scaled to zero outside Hybernate", replicas: 0, wantReason: ReasonScaledToZero,
			wantEvent: "Normal ScaledToZero api: pause requested, but it's scaled to zero outside Hybernate",
		},
		{
			name: "target missing", replicas: 3, wantReason: ReasonTargetNotFound,
			setup: func(t *testing.T, r *Reconciler) {
				require.NoError(t, r.Delete(context.Background(), targetDeployment("api", "default")))
			},
			wantEvent: "Warning TargetNotFound api: pause requested, but Deployment api not found",
		},
		{
			name: "target ignored", replicas: 3, wantReason: "TargetIgnored",
			setup: func(t *testing.T, r *Reconciler) {
				patchTarget(t, r, func(d *appsv1.Deployment) {
					d.Labels = map[string]string{v1alpha1.LabelIgnore: v1alpha1.True}
				})
			},
			wantEvent: "Warning TargetIgnored api: pause requested, but Deployment api has the hybernate.io/ignore label",
		},
		{
			name: "another ManagedWorkload manages the target", replicas: 3,
			mutate: func(w *v1alpha1.ManagedWorkload) { w.UID = "bbb" },
			objs: []client.Object{func() client.Object {
				owner := lifecycleWorkload("api-first", nil, v1alpha1.PhaseRunning)
				owner.UID = "aaa"
				owner.Spec.Target.Name = "api"
				return owner
			}()},
			wantReason: conditionDuplicateTarget,
			wantEvent:  "Warning DuplicateTarget api: pause requested, but Deployment/api is already managed by api-first",
		},
		{
			name: "awake dependents", replicas: 3,
			objs: []client.Object{depWorkload("default", "web", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
				v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"})},
			wantReason: conditionHeldByDependents,
			wantEvent:  "Normal HeldByDependents api: pause requested, but kept awake for default/web",
		},
		{
			name: "a dependsOn cycle", replicas: 3,
			mutate: func(w *v1alpha1.ManagedWorkload) {
				w.Spec.DependsOn = []v1alpha1.DependencyRef{{Kind: v1alpha1.TargetKindDeployment, Name: "web"}}
			},
			objs: []client.Object{depWorkload("default", "web", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused,
				v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"})},
			wantReason: conditionDependencyCycle,
			wantEvent:  "Warning DependencyCycle api: pause requested, but dependsOn forms a cycle through this workload",
		},
		{
			name: "an active-until hold", replicas: 3,
			mutate: func(w *v1alpha1.ManagedWorkload) {
				w.Annotations[v1alpha1.AnnotationActiveUntil] = fixedTime.Add(time.Hour).Format(time.RFC3339)
			},
			wantReason: reasonActiveUntil,
			wantEvent: "Warning ActiveUntil api: pause requested, but the hybernate.io/active-until annotation on the " +
				"ManagedWorkload keeps it awake until 2026-03-14T13:00:00Z; remove it to pause now",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := requestedPause()
			if tt.mutate != nil {
				tt.mutate(workload)
			}
			r := lifecycleReconciler(t, workload, tt.replicas, interceptor.Funcs{}, tt.objs...)
			if tt.setup != nil {
				tt.setup(t, r)
			}

			reconcileUntilSettled(t, r)

			got := getWorkload(t, r, "api")
			assert.NotEqual(t, v1alpha1.PhasePaused, got.Status.Phase)
			assert.Nil(t, got.Status.Pause)
			assert.Equal(t, pauseToken, got.Status.LastPauseRequest)
			c := pauseRequestCondition(t, got)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, tt.wantReason, c.Reason)
			assert.True(t, strings.HasPrefix(c.Message, "not paused: "), c.Message)
			recorded := drainEvents(t, r)
			assert.Contains(t, recorded, tt.wantEvent)
			assert.Equal(t, 1, strings.Count(recorded, "pause requested, but"), "answered once: %s", recorded)
		})
	}
}

// Under dry-run a request is counted as a would-be pause, and measured as
// one the idle clock began: the workload is Idle and stays up until
// activity after the request, not the activity the request overrode.
func TestPauseRequest_DryRun(t *testing.T) {
	workload := requestedPause()
	workload.Spec.DryRun = true
	workload.Annotations[v1alpha1.AnnotationLastActivity] = fixedTime.Add(-10 * time.Minute).Format(time.RFC3339)
	r := lifecycleReconciler(t, workload, 3, interceptor.Funcs{})
	now := fixedTime
	r.clock = func() time.Time { return now }

	reconcileUntilSettled(t, r)

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseIdle, got.Status.Phase, "earlier activity doesn't end the would-be pause")
	assert.Equal(t, int32(3), targetReplicas(t, r), "dry-run never scales")
	assert.Equal(t, pauseToken, got.Status.LastPauseRequest)
	c := pauseRequestCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reasonDryRun, c.Reason)
	require.NotNil(t, got.Status.DryRun)
	assert.Equal(t, int32(1), got.Status.DryRun.Pauses)
	recorded := drainEvents(t, r)
	assert.Equal(t, 1, strings.Count(recorded, "Normal PauseRequested [dry-run] api: pause requested; would pause"),
		recorded)

	now = fixedTime.Add(30 * time.Minute)
	patchWorkload(t, r, "api", func(w *v1alpha1.ManagedWorkload) {
		w.Annotations[v1alpha1.AnnotationLastActivity] = now.Format(time.RFC3339)
	})
	reconcileUntilSettled(t, r)

	got = getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
	assert.Equal(t, 30*time.Minute, got.Status.DryRun.Slept.Duration)
	assert.Equal(t, int32(1), got.Status.DryRun.Pauses)
}

// The forecast expecting demand, and the hour Hybernate waits after a
// GitOps tool undid its last pause, both hold back the idle clock. Someone
// asking for the pause overrides both.
func TestPauseRequest_OverridesTheForecastAndTheGitOpsHold(t *testing.T) {
	expectsDemand := func(r *Reconciler) {
		r.engines = newEngineRegistry(func() forecaster {
			return &stubForecaster{phase: forecast.DailyActive, predictValue: 400}
		})
	}
	undoneByArgo := func(w *v1alpha1.ManagedWorkload) {
		w.Status.LastScaledUp = &v1alpha1.ScaledUp{At: metav1.NewTime(fixedTime.Add(-10 * time.Minute)),
			By: "argocd-controller", GitOps: "Argo CD", Replicas: 3}
		meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: conditionGitOpsConflict,
			Status: metav1.ConditionTrue, Reason: "PauseUndone"})
	}
	tests := []struct {
		name   string
		mutate func(*v1alpha1.ManagedWorkload)
		engine func(*Reconciler)
	}{
		{name: "the forecast expects demand", mutate: func(*v1alpha1.ManagedWorkload) {}, engine: expectsDemand},
		{name: "a GitOps tool undid the last pause", mutate: undoneByArgo, engine: func(*Reconciler) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idle := requestedPause()
			delete(idle.Annotations, v1alpha1.AnnotationPauseRequested)
			idle.Status.Activity.LastActivityTime = metav1.NewTime(fixedTime.Add(-2 * time.Hour))
			tt.mutate(idle)
			r := lifecycleReconciler(t, idle, 3, interceptor.Funcs{})
			tt.engine(r)

			reconcileUntilSettled(t, r)

			require.NotEqual(t, v1alpha1.PhasePaused, getWorkload(t, r, "api").Status.Phase,
				"the idle clock is held back")

			requested := requestedPause()
			tt.mutate(requested)
			r = lifecycleReconciler(t, requested, 3, interceptor.Funcs{})
			tt.engine(r)

			reconcileUntilSettled(t, r)

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase, "the request isn't")
			assert.Equal(t, int32(0), targetReplicas(t, r))
			assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, conditionIdleVetoed))
		})
	}
}

// A request that finds the workload pausing, or waking, is answered once
// that's done: one pausing is then paused, and one waking is paused again.
func TestPauseRequest_DuringATransition(t *testing.T) {
	tests := []struct {
		phase      v1alpha1.WorkloadPhase
		replicas   int32
		wantReason string
	}{
		{phase: v1alpha1.PhasePausing, replicas: 3, wantReason: reasonAlreadyPaused},
		{phase: v1alpha1.PhaseResuming, replicas: 3, wantReason: reasonPausing},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			workload := requestedPause()
			workload.Status.Phase = tt.phase
			workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3}
			r := lifecycleReconciler(t, workload, tt.replicas, interceptor.Funcs{})

			reconcileUntilSettled(t, r)

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase)
			assert.Equal(t, pauseToken, got.Status.LastPauseRequest)
			assert.Equal(t, tt.wantReason, pauseRequestCondition(t, got).Reason)
		})
	}
}

// Against an API server, a request pauses the workload once: a request
// through the doorman wakes it to every replica it had, and an operator
// started afresh, as after a restart or a new leader, finds the request
// handled in status and leaves the workload running. A new request pauses
// it again.
func TestPauseRequest_AcrossARestart(t *testing.T) {
	cfg := startEnvtest(t)
	opScheme := testScheme(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := client.New(cfg, client.Options{Scheme: opScheme})
	require.NoError(t, err)

	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview"}}))
	require.NoError(t, c.Create(ctx, envtestDeployment("preview", 3)))
	require.NoError(t, c.Create(ctx, &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "preview",
			Annotations: map[string]string{v1alpha1.AnnotationPauseRequested: pauseToken}},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			IdlePolicy: &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}))

	key := client.ObjectKey{Namespace: "preview", Name: "api"}
	get := func() *v1alpha1.ManagedWorkload {
		var w v1alpha1.ManagedWorkload
		require.NoError(t, c.Get(ctx, key, &w))
		return &w
	}
	replicas := func() int32 {
		var d appsv1.Deployment
		require.NoError(t, c.Get(ctx, key, &d))
		return *d.Spec.Replicas
	}
	annotate := func(name, value string) {
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, name, value)
		require.NoError(t, c.Patch(ctx, get(), client.RawPatch(types.MergePatchType, []byte(patch))))
	}

	stop := startOperator(t, cfg, opScheme)
	require.Eventually(t, func() bool {
		w := get()
		return w.Status.Phase == v1alpha1.PhasePaused && replicas() == 0 && w.Status.LastPauseRequest == pauseToken
	}, 30*time.Second, 100*time.Millisecond, "the request pauses it")

	annotate(v1alpha1.AnnotationLastRequest, time.Now().UTC().Format(time.RFC3339))
	require.Eventually(t, func() bool { return get().Status.Phase == v1alpha1.PhaseResuming && replicas() == 3 },
		30*time.Second, 100*time.Millisecond, "a request through the doorman wakes it to the replicas it had")
	var d appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &d))
	d.Status.Replicas, d.Status.ReadyReplicas = 3, 3
	require.NoError(t, c.Status().Update(ctx, &d))
	require.Eventually(t, func() bool { return get().Status.Phase == v1alpha1.PhaseRunning },
		30*time.Second, 100*time.Millisecond, "and it's Running once its pods are Ready")

	stop()
	startOperator(t, cfg, opScheme)
	annotate("poke", "after-restart")
	assert.Never(t, func() bool { return get().Status.Phase != v1alpha1.PhaseRunning },
		3*time.Second, 100*time.Millisecond, "a restarted operator doesn't act on the handled request again")
	assert.Equal(t, int32(3), replicas())

	annotate(v1alpha1.AnnotationPauseRequested, "a-new-request")
	assert.Eventually(t, func() bool {
		w := get()
		return w.Status.Phase == v1alpha1.PhasePaused && replicas() == 0 && w.Status.LastPauseRequest == "a-new-request"
	}, 30*time.Second, 100*time.Millisecond, "a new request pauses it again")
}

// startOperator runs the ManagedWorkload controller against cfg until the
// returned stop is called, or the test ends.
func startOperator(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) func() {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	require.NoError(t, err)
	r := &Reconciler{Client: mgr.GetClient(), Scheme: scheme, Recorder: events.NewFakeRecorder(1000),
		PodReader: mgr.GetAPIReader()}
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { assert.NoError(t, mgr.Start(ctx)) })
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			running.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}
