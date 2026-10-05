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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
)

// depWorkload is a ManagedWorkload for a target of the same name, idle for
// longer than its one-hour idleAfter.
func depWorkload(namespace, name string, kind v1alpha1.TargetKind, phase v1alpha1.WorkloadPhase, deps ...v1alpha1.DependencyRef) *v1alpha1.ManagedWorkload {
	evaluated := metav1.NewTime(fixedTime.Add(-30 * time.Second))
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "-" + name)},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: kind, Name: name},
			IdlePolicy: &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
			DependsOn:  deps,
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: phase,
			Activity: &v1alpha1.ActivityStatus{
				LastActivityTime:   metav1.NewTime(fixedTime.Add(-2 * time.Hour)),
				LastActivitySource: v1alpha1.ActivitySourceCPU,
				LastEvaluatedTime:  &evaluated,
			},
		},
	}
}

// postgresTarget is the default/postgres StatefulSet with the given replica counts.
func postgresTarget(replicas, ready int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "default"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: ready},
	}
}

func depReconciler(t *testing.T, pauser *stubPauser, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := testScheme(t)
	metrics := idleCPU
	return &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&v1alpha1.ManagedWorkload{}).WithObjects(objs...).Build(),
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(20),
		pauser:   pauser,
		metrics:  &metrics,
		engines:  newEngineRegistry(func() forecaster { return &stubForecaster{phase: forecast.Observing} }),
		clock:    func() time.Time { return fixedTime },
	}
}

func fetch(t *testing.T, r *Reconciler, name string) *v1alpha1.ManagedWorkload {
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &w))
	return &w
}

func postgresRef() v1alpha1.DependencyRef {
	return v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}
}

func TestDependencies_HoldWhileDependentsAwake(t *testing.T) {
	tests := []struct {
		name       string
		dependents []*v1alpha1.ManagedWorkload
		wantPause  bool
		wantHeldBy string
	}{
		{
			name:       "awake dependent holds it",
			dependents: []*v1alpha1.ManagedWorkload{depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef())},
			wantHeldBy: "kept awake for default/api",
		},
		{
			name:       "paused dependent releases it",
			dependents: []*v1alpha1.ManagedWorkload{depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef())},
			wantPause:  true,
		},
		{
			name: "any awake dependent is enough",
			dependents: []*v1alpha1.ManagedWorkload{
				depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef()),
				depWorkload("default", "worker", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, postgresRef()),
			},
			wantHeldBy: "kept awake for default/worker",
		},
		{
			name: "dependent in another namespace",
			dependents: []*v1alpha1.ManagedWorkload{depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
				v1alpha1.DependencyRef{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"})},
			wantHeldBy: "kept awake for preview-42/api",
		},
		{
			name: "same name in another namespace is a different dependency",
			dependents: []*v1alpha1.ManagedWorkload{
				depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef()),
			},
			wantPause: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
			objs := make([]client.Object, 0, 1+len(tt.dependents))
			objs = append(objs, postgres)
			for _, d := range tt.dependents {
				objs = append(objs, d)
			}
			pauser := &stubPauser{pauseDone: true}
			r := depReconciler(t, pauser, objs...)

			_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
			require.NoError(t, err)

			assert.Equal(t, tt.wantPause, pauser.pauseCalls == 1)
			held := meta.FindStatusCondition(postgres.Status.Conditions, conditionHeldByDependents)
			if tt.wantHeldBy == "" {
				assert.False(t, held != nil && held.Status == metav1.ConditionTrue)
				return
			}
			require.NotNil(t, held)
			assert.Equal(t, metav1.ConditionTrue, held.Status)
			assert.Equal(t, tt.wantHeldBy, held.Message)
		})
	}
}

func TestDependencies_HoldAppliesInDryRun(t *testing.T) {
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
	postgres.Spec.DryRun = true
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef())
	r := depReconciler(t, &stubPauser{pauseDone: true}, postgres, api)

	_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
	require.NoError(t, err)

	assert.Equal(t, v1alpha1.PhaseRunning, postgres.Status.Phase, "a held workload isn't reported as idle")
	assert.True(t, meta.IsStatusConditionTrue(postgres.Status.Conditions, conditionHeldByDependents))
}

func TestDependencies_CycleBlocksBothWithCondition(t *testing.T) {
	a := depWorkload("default", "a", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
		v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "b"})
	b := depWorkload("default", "b", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused,
		v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "a"})
	pauser := &stubPauser{pauseDone: true}
	r := depReconciler(t, pauser, a, b)

	_, err := r.reconcileAutomation(context.Background(), a, clockTarget("app:v1", nil))
	require.NoError(t, err)

	assert.Equal(t, 0, pauser.pauseCalls, "a cycle must stop the pause even when the other side is asleep")
	assert.True(t, meta.IsStatusConditionTrue(a.Status.Conditions, conditionDependencyCycle))
}

func learned(w *v1alpha1.ManagedWorkload, kind v1alpha1.TargetKind, name string) {
	w.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
		{Namespace: w.Namespace, Kind: kind, Name: name, Source: v1alpha1.LearnedFromWake}}}
}

// Two services that call each other learn that each depends on the other.
// That mustn't keep both awake forever, as a declared cycle would.
func TestDependencies_LearnedCycleDoesNotHold(t *testing.T) {
	tests := []struct {
		name        string
		bDeclaresA  bool
		wantAPauses bool
		wantBPauses bool
	}{
		{name: "learned both ways", wantAPauses: true, wantBPauses: true},
		{name: "declared one way, learned the other", bDeclaresA: true, wantAPauses: false, wantBPauses: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := depWorkload("default", "a", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			learned(a, v1alpha1.TargetKindDeployment, "b")
			b := depWorkload("default", "b", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			if tt.bDeclaresA {
				b.Spec.DependsOn = []v1alpha1.DependencyRef{{Kind: v1alpha1.TargetKindDeployment, Name: "a"}}
			} else {
				learned(b, v1alpha1.TargetKindDeployment, "a")
			}

			for _, c := range []struct {
				w     *v1alpha1.ManagedWorkload
				pause bool
			}{{a, tt.wantAPauses}, {b, tt.wantBPauses}} {
				pauser := &stubPauser{pauseDone: true}
				r := depReconciler(t, pauser, a.DeepCopy(), b.DeepCopy())
				w := fetch(t, r, c.w.Name)

				_, err := r.reconcileAutomation(context.Background(), w, clockTarget("app:v1", nil))
				require.NoError(t, err)

				assert.Equal(t, c.pause, pauser.pauseCalls == 1, "%s pauses", w.Name)
				assert.False(t, meta.IsStatusConditionTrue(w.Status.Conditions, conditionDependencyCycle),
					"a learned link never makes a cycle")
			}
		})
	}
}

func TestDependencies_WakingWakesDependencies(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef())
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
	unrelated := depWorkload("default", "redis", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
	r := depReconciler(t, &stubPauser{resumeDone: true}, api, postgres, unrelated, postgresTarget(1, 1))

	_, err := r.handleResume(context.Background(), api)
	require.NoError(t, err)

	assert.Equal(t, fixedTime.UTC().Format(time.RFC3339),
		fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity],
		"the dependency is woken through its activity annotation")
	assert.Empty(t, fetch(t, r, "redis").Annotations[v1alpha1.AnnotationLastActivity])
	assert.Equal(t, v1alpha1.PhaseRunning, api.Status.Phase, "without waitForReady the dependent doesn't wait")
}

func TestDependencies_WaitForReady(t *testing.T) {
	pausedPostgres := func(desired *v1alpha1.DesiredState) *v1alpha1.ManagedWorkload {
		w := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
		w.Spec.DesiredState = desired
		return w
	}
	tests := []struct {
		name        string
		postgres    client.Object
		managed     *v1alpha1.ManagedWorkload
		wantWait    bool
		wantRewoken bool
	}{
		{name: "dependency pods not ready", postgres: postgresTarget(1, 0), wantWait: true},
		{name: "dependency ready", postgres: postgresTarget(1, 1)},
		{name: "paused dependency is woken again and waited for", postgres: postgresTarget(0, 0),
			managed: pausedPostgres(nil), wantWait: true, wantRewoken: true},
		{name: "unmanaged dependency scaled to zero won't start", postgres: postgresTarget(0, 0)},
		{name: "dependency held paused by desiredState won't start", postgres: postgresTarget(0, 0),
			managed: pausedPostgres(ptr.To(v1alpha1.DesiredStatePaused))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := postgresRef()
			ref.WaitForReady = true
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, ref)
			api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
			pauser := &stubPauser{resumeDone: true}
			objs := []client.Object{api, tt.postgres}
			if tt.managed != nil {
				objs = append(objs, tt.managed)
			}
			r := depReconciler(t, pauser, objs...)

			result, err := r.handleResume(context.Background(), api)
			require.NoError(t, err)

			if tt.managed != nil {
				stamped := fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity] != ""
				assert.Equal(t, tt.wantRewoken, stamped)
			}
			if tt.wantWait {
				assert.Equal(t, 0, pauser.resumeCalls, "the dependent must not scale up before its dependency is Ready")
				require.NotNil(t, result)
				assert.Equal(t, dependencyReadyCheckInterval, result.RequeueAfter)
				assert.True(t, meta.IsStatusConditionTrue(fetch(t, r, "api").Status.Conditions, conditionWaitingForDependencies))
				return
			}
			assert.Equal(t, 1, pauser.resumeCalls, "a dependency that won't start never strands the dependent at zero")
			assert.Equal(t, v1alpha1.PhaseRunning, api.Status.Phase)
		})
	}
}

func TestDependencies_WaitForMissingDependencyDoesNotBlock(t *testing.T) {
	ref := postgresRef()
	ref.WaitForReady = true
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, ref)
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
	pauser := &stubPauser{resumeDone: true}
	r := depReconciler(t, pauser, api)

	_, err := r.handleResume(context.Background(), api)
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.resumeCalls)
}

func TestDependencies_NotFoundCondition(t *testing.T) {
	tests := []struct {
		name        string
		objs        []client.Object
		wantMissing bool
	}{
		{name: "unmanaged and missing", wantMissing: true},
		{name: "unmanaged but exists", objs: []client.Object{postgresTarget(1, 1)}},
		{name: "managed", objs: []client.Object{depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef())
			api.Status.Activity.LastActivityTime = metav1.NewTime(fixedTime)
			r := depReconciler(t, &stubPauser{}, append(tt.objs, api)...)

			_, err := r.reconcileAutomation(context.Background(), api, clockTarget("app:v1", nil))
			require.NoError(t, err)

			assert.Equal(t, tt.wantMissing, meta.IsStatusConditionTrue(api.Status.Conditions, conditionDependencyNotFound))
		})
	}
}

func TestDependencies_ManualPauseWarnsWhenDependentsAwake(t *testing.T) {
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
	postgres.Spec.DesiredState = ptr.To(v1alpha1.DesiredStatePaused)
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef())
	pauser := &stubPauser{pauseDone: true}
	r := depReconciler(t, pauser, postgres, api, postgresTarget(1, 1))

	_, err := r.reconcileDesiredState(context.Background(), postgres)
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.pauseCalls, "a manual pause still wins")
	recorder, ok := r.Recorder.(*events.FakeRecorder)
	require.True(t, ok)
	var warned bool
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "DependentsAwake") {
			warned = true
		}
	}
	assert.True(t, warned, "the override is reported")
}

func TestFindRelatedWorkloads_Dependencies(t *testing.T) {
	api := depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
		v1alpha1.DependencyRef{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"})
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
	unrelated := depWorkload("default", "redis", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
	r := depReconciler(t, &stubPauser{}, api, postgres, unrelated)

	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "default", Name: "postgres"}}},
		r.findRelatedWorkloads(context.Background(), api), "a change to a dependent re-checks its dependencies")
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "preview-42", Name: "api"}}},
		r.findRelatedWorkloads(context.Background(), postgres), "a change to a dependency re-checks its dependents")

	assert.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "default", Name: "postgres"}},
		{NamespacedName: types.NamespacedName{Namespace: "preview-42", Name: "api"}},
	}, r.findWorkloadsForTarget(context.Background(), postgresTarget(1, 1)),
		"a dependency's readiness changing re-checks dependents waiting for it")
}

// A workload woken by a scale-up wakes its dependencies, as any wake does.
func TestDependencies_ScaleUpWakesDependencies(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef())
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
	r := depReconciler(t, &stubPauser{}, api, postgres)

	require.NoError(t, r.wakeOnScaleUp(context.Background(), api, scaledBy("kubectl-scale", 1)))

	assert.Equal(t, fixedTime.UTC().Format(time.RFC3339),
		fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity])
}

// startEnvtest runs an API server with Hybernate's CRDs for the test.
func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	return cfg
}

// startCache runs a cache of the namespaces given, as the operator's is
// under watchNamespaces, until the test ends, and returns a client that
// reads from it.
func startCache(t *testing.T, cfg *rest.Config, namespaces ...string) client.Client {
	t.Helper()
	scheme := testScheme(t)
	byNamespace := map[string]cache.Config{}
	for _, ns := range namespaces {
		byNamespace[ns] = cache.Config{}
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, DefaultNamespaces: byNamespace})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { assert.NoError(t, c.Start(ctx)) })
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	require.True(t, c.WaitForCacheSync(ctx))
	cl, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: c}})
	require.NoError(t, err)
	return cl
}

func postgresStatefulSet(namespace string) *appsv1.StatefulSet {
	labels := map[string]string{"app": "postgres"}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres", Image: "postgres:17"}}},
			},
		},
	}
}

// Under watchNamespaces, the operator's cache can't read a dependency in
// another namespace at all: it fails with "unknown namespace for the
// cache", not NotFound. That mustn't stop the dependent pausing or waking.
func TestDependencies_UnwatchedNamespaceDoesNotBlock(t *testing.T) {
	cfg := startEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scheme := testScheme(t)
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	require.NoError(t, direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared"}}))
	require.NoError(t, direct.Create(ctx, postgresStatefulSet("shared")))
	api := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
			DependsOn: []v1alpha1.DependencyRef{
				{Namespace: "shared", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", WaitForReady: true}},
		},
	}
	require.NoError(t, direct.Create(ctx, api))
	api.Status.Phase = v1alpha1.PhasePaused
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2}
	require.NoError(t, direct.Status().Update(ctx, api))

	cached := startCache(t, cfg, "default")
	err = cached.Get(ctx, types.NamespacedName{Namespace: "shared", Name: "postgres"}, &appsv1.StatefulSet{})
	require.Error(t, err, "the cache can't read outside its namespaces")
	require.False(t, apierrors.IsNotFound(err))

	pauser := &stubPauser{resumeDone: true}
	metrics := idleCPU
	r := &Reconciler{Client: cached, Scheme: scheme, Recorder: events.NewFakeRecorder(20),
		WatchNamespaces: []string{"default"}, pauser: pauser, metrics: &metrics,
		clock:   func() time.Time { return fixedTime },
		engines: newEngineRegistry(func() forecaster { return &stubForecaster{phase: forecast.Observing} })}

	var w v1alpha1.ManagedWorkload
	require.Eventually(t, func() bool {
		return cached.Get(ctx, client.ObjectKeyFromObject(api), &w) == nil && w.Status.Phase == v1alpha1.PhasePaused
	}, 10*time.Second, 50*time.Millisecond)

	require.NoError(t, r.checkDependenciesExist(ctx, &w))
	cond := meta.FindStatusCondition(w.Status.Conditions, conditionDependencyNotFound)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "doesn't watch namespace shared")

	_, err = r.handleResume(ctx, &w)
	require.NoError(t, err, "waking isn't blocked by a dependency it can't see")
	assert.Equal(t, 1, pauser.resumeCalls)
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
}
