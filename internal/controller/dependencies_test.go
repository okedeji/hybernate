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
	"path/filepath"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/forecast"
	"github.com/okedeji/hybernate/internal/lifecycle"
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
		Client: withWorkloadIndexes(fake.NewClientBuilder().WithScheme(scheme)).
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
			name: "dependent in another namespace, which postgres's doesn't allow",
			dependents: []*v1alpha1.ManagedWorkload{depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
				v1alpha1.DependencyRef{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"})},
			wantPause: true,
		},
		{
			name: "same name in another namespace is a different dependency",
			dependents: []*v1alpha1.ManagedWorkload{
				depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef()),
			},
			wantPause: true,
		},
		{
			name: "a duplicate, whose target another ManagedWorkload manages, doesn't hold it",
			dependents: []*v1alpha1.ManagedWorkload{withCondition(
				depWorkload("default", "api-copy", v1alpha1.TargetKindDeployment, "", postgresRef()),
				conditionDuplicateTarget, metav1.ConditionTrue, conditionDuplicateTarget)},
			wantPause: true,
		},
		{
			name: "a dependent whose target is gone doesn't hold it",
			dependents: []*v1alpha1.ManagedWorkload{withCondition(
				depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef()),
				conditionTargetAvailable, metav1.ConditionFalse, ReasonTargetNotFound)},
			wantPause: true,
		},
		{
			name: "a dependent being deleted doesn't hold it",
			dependents: []*v1alpha1.ManagedWorkload{func() *v1alpha1.ManagedWorkload {
				w := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, postgresRef())
				w.Finalizers = []string{finalizerName}
				w.DeletionTimestamp = &metav1.Time{Time: fixedTime}
				return w
			}()},
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

func withCondition(w *v1alpha1.ManagedWorkload, condType string, status metav1.ConditionStatus,
	reason string) *v1alpha1.ManagedWorkload {
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: condType, Status: status, Reason: reason})
	return w
}

// A condition's message over 32768 bytes fails the whole status write, so
// a dependency with many dependents names only some.
func TestDependencies_HoldersListIsBounded(t *testing.T) {
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
	objs := make([]client.Object, 0, 2001)
	objs = append(objs, postgres)
	for i := range 2000 {
		objs = append(objs, depWorkload("default", fmt.Sprintf("api-%04d", i), v1alpha1.TargetKindDeployment,
			v1alpha1.PhaseRunning, postgresRef()))
	}
	r := depReconciler(t, &stubPauser{pauseDone: true}, objs...)

	_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
	require.NoError(t, err)

	held := meta.FindStatusCondition(postgres.Status.Conditions, conditionHeldByDependents)
	require.NotNil(t, held)
	assert.Equal(t, "kept awake for default/api-0000, default/api-0001, default/api-0002, default/api-0003, "+
		"default/api-0004, default/api-0005, default/api-0006, default/api-0007, default/api-0008, default/api-0009 "+
		"and 1990 more", held.Message)
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

	_, err := r.handleResume(context.Background(), api, nil)
	require.NoError(t, err)

	assert.Equal(t, fixedTime.UTC().Format(time.RFC3339),
		fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity],
		"the dependency is woken through its activity annotation")
	assert.Empty(t, fetch(t, r, "redis").Annotations[v1alpha1.AnnotationLastActivity])
	assert.Equal(t, v1alpha1.PhaseRunning, api.Status.Phase, "without waitForReady the dependent doesn't wait")
}

func TestDependencies_WaitForReady(t *testing.T) {
	pausedPostgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
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
			managed: pausedPostgres, wantWait: true, wantRewoken: true},
		{name: "unmanaged dependency scaled to zero won't start", postgres: postgresTarget(0, 0)},
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

			result, err := r.handleResume(context.Background(), api, nil)
			require.NoError(t, err)

			if tt.managed != nil {
				stamped := fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity] != ""
				assert.Equal(t, tt.wantRewoken, stamped)
			}
			if tt.wantWait {
				assert.Equal(t, 0, pauser.resumeCalls, "the dependent must not scale up before its dependency is Ready")
				require.NotNil(t, result)
				assert.Equal(t, dependencyReadyCheckInterval, result.RequeueAfter)
				waiting := meta.FindStatusCondition(fetch(t, r, "api").Status.Conditions, conditionWaitingForDependencies)
				require.NotNil(t, waiting)
				assert.Equal(t, metav1.ConditionTrue, waiting.Status)
				assert.Equal(t, "waiting for default/statefulset/postgres (0 ready)", waiting.Message,
					"the kind tells a StatefulSet from a Deployment of the same name")
				return
			}
			assert.Equal(t, 1, pauser.resumeCalls, "a dependency that won't start never strands the dependent at zero")
			assert.Equal(t, v1alpha1.PhaseRunning, api.Status.Phase)
		})
	}
}

// A waitForReady dependency whose own resume settled short, with a replica
// that never became Ready, serves with the rest, and its dependents must
// start once it does rather than wait at zero for the last one.
func TestDependencies_WaitForAPartlyReadyDependency(t *testing.T) {
	tests := []struct {
		name     string
		managed  v1alpha1.WorkloadPhase
		ready    int32
		resuming time.Duration
		wantWait bool
	}{
		{name: "managed, its resume settled short", managed: v1alpha1.PhaseRunning, ready: 2},
		{name: "managed and idle, its resume settled short", managed: v1alpha1.PhaseIdle, ready: 2},
		{name: "managed, still resuming", managed: v1alpha1.PhaseResuming, ready: 2, resuming: 5 * time.Minute,
			wantWait: true},
		{name: "managed and running, none Ready", managed: v1alpha1.PhaseRunning, wantWait: true},
		{name: "unmanaged, given as long as a resume gives", ready: 2, resuming: lifecycle.ServingWait},
		{name: "unmanaged, not yet given as long", ready: 2, resuming: time.Minute, wantWait: true},
		{name: "unmanaged, none Ready", resuming: 5 * time.Minute, wantWait: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := postgresRef()
			ref.WaitForReady = true
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, ref)
			began := metav1.NewTime(fixedTime.Add(-tt.resuming))
			api.Status.LastTransitionTime = &began
			api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
			objs := []client.Object{api, postgresTarget(3, tt.ready)}
			if tt.managed != "" {
				objs = append(objs, depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, tt.managed))
			}
			pauser := &stubPauser{resumeDone: true}
			r := depReconciler(t, pauser, objs...)

			_, err := r.handleResume(context.Background(), fetch(t, r, "api"), nil)
			require.NoError(t, err)

			if tt.wantWait {
				assert.Zero(t, pauser.resumeCalls, "it started before its dependency served")
				return
			}
			assert.Equal(t, 1, pauser.resumeCalls, "it waited on a dependency serving with %d of 3 Ready", tt.ready)
		})
	}
}

// A waitForReady dependency that's up but never Ready, crash-looping or
// unable to pull its image, would hold the dependent at zero for good. It's
// waited for until its progress deadline, then the dependent starts anyway
// and says why.
func TestDependencies_WaitIsBounded(t *testing.T) {
	deployment := func(deadline *int32) client.Object {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "default"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), ProgressDeadlineSeconds: deadline},
		}
	}
	tests := []struct {
		name     string
		kind     v1alpha1.TargetKind
		target   client.Object
		managed  v1alpha1.WorkloadPhase
		resuming time.Duration
		wantWait bool
		wantWhy  string
	}{
		{name: "a StatefulSet within its deadline", kind: v1alpha1.TargetKindStatefulSet,
			target: postgresTarget(1, 0), resuming: 9 * time.Minute, wantWait: true},
		{name: "a StatefulSet past its deadline", kind: v1alpha1.TargetKindStatefulSet,
			target: postgresTarget(1, 0), resuming: 11 * time.Minute,
			wantWhy: "not waiting for default/statefulset/postgres (it isn't Ready after 10m)"},
		{name: "a managed dependency whose resume settled with none Ready", kind: v1alpha1.TargetKindStatefulSet,
			target: postgresTarget(1, 0), managed: v1alpha1.PhaseRunning, resuming: 11 * time.Minute,
			wantWhy: "not waiting for default/statefulset/postgres (it isn't Ready after 10m)"},
		{name: "a Deployment within its progress deadline", kind: v1alpha1.TargetKindDeployment,
			target: deployment(ptr.To[int32](120)), resuming: time.Minute, wantWait: true},
		{name: "a Deployment past its progress deadline", kind: v1alpha1.TargetKindDeployment,
			target: deployment(ptr.To[int32](120)), resuming: 3 * time.Minute,
			wantWhy: "not waiting for default/deployment/postgres (it isn't Ready after 2m)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := v1alpha1.DependencyRef{Kind: tt.kind, Name: "postgres", WaitForReady: true}
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, ref)
			began := metav1.NewTime(fixedTime.Add(-tt.resuming))
			api.Status.LastTransitionTime = &began
			api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
			objs := []client.Object{api, tt.target}
			if tt.managed != "" {
				objs = append(objs, depWorkload("default", "postgres", tt.kind, tt.managed))
			}
			pauser := &stubPauser{resumeDone: true}
			r := depReconciler(t, pauser, objs...)

			_, err := r.handleResume(context.Background(), fetch(t, r, "api"), nil)
			require.NoError(t, err)

			if tt.wantWait {
				assert.Zero(t, pauser.resumeCalls)
				return
			}
			assert.Equal(t, 1, pauser.resumeCalls, "the dependent starts without the dependency")
			c := meta.FindStatusCondition(fetch(t, r, "api").Status.Conditions, conditionWaitingForDependencies)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, reasonDependencyWontStart, c.Reason)
			assert.Equal(t, tt.wantWhy, c.Message)
			assert.Len(t, recorded(r.Recorder.(*events.FakeRecorder), reasonDependencyWontStart), 1,
				"a Warning names the dependency")
		})
	}
}

// Two workloads that each waitForReady on the other would each wait for
// the other to start first, forever.
func TestDependencies_MutualWaitForReady(t *testing.T) {
	a := depWorkload("default", "a", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming,
		v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "b", WaitForReady: true})
	a.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
	b := depWorkload("default", "b", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming,
		v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindDeployment, Name: "a", WaitForReady: true})
	b.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
	zero := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](0)}}
	}

	for _, name := range []string{"a", "b"} {
		pauser := &stubPauser{resumeDone: true}
		r := depReconciler(t, pauser, a.DeepCopy(), b.DeepCopy(), zero("a"), zero("b"))

		_, err := r.handleResume(context.Background(), fetch(t, r, name), nil)
		require.NoError(t, err)

		assert.Equal(t, 1, pauser.resumeCalls, "%s starts", name)
		c := meta.FindStatusCondition(fetch(t, r, name).Status.Conditions, conditionWaitingForDependencies)
		require.NotNil(t, c)
		assert.Equal(t, conditionDependencyCycle, c.Reason)
	}
}

// A workload listed as its own dependency, as a namespace's depends-on
// makes of the dependency it names, is no cycle and nothing to wait for.
func TestDependencies_SelfReferenceIsIgnored(t *testing.T) {
	self := postgresRef()
	self.WaitForReady = true

	t.Run("it still pauses", func(t *testing.T) {
		postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning, self)
		pauser := &stubPauser{pauseDone: true}
		r := depReconciler(t, pauser, postgres)

		_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
		require.NoError(t, err)

		assert.Equal(t, 1, pauser.pauseCalls)
		assert.False(t, meta.IsStatusConditionTrue(postgres.Status.Conditions, conditionDependencyCycle))
	})
	t.Run("it still wakes", func(t *testing.T) {
		postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseResuming, self)
		postgres.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
		pauser := &stubPauser{resumeDone: true}
		r := depReconciler(t, pauser, postgres, postgresTarget(0, 0))

		_, err := r.handleResume(context.Background(), postgres, nil)
		require.NoError(t, err)

		assert.Equal(t, 1, pauser.resumeCalls, "waiting for its own pods would leave it at zero")
	})
}

func TestDependencies_WaitForMissingDependencyDoesNotBlock(t *testing.T) {
	ref := postgresRef()
	ref.WaitForReady = true
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming, ref)
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
	pauser := &stubPauser{resumeDone: true}
	r := depReconciler(t, pauser, api)

	_, err := r.handleResume(context.Background(), api, nil)
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.resumeCalls)
}

// A dependency in another namespace is only held, woken and waited for
// when its namespace allows dependents from the dependent's. Otherwise a
// user of one namespace could keep another's workloads awake, or tell
// from the conditions what's in it.
func TestDependencies_CrossNamespaceNeedsTheNamespacesConsent(t *testing.T) {
	shared := v1alpha1.DependencyRef{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
		WaitForReady: true}
	tests := []struct {
		name           string
		dependentsFrom *string
		learn          bool
		wantAllowed    bool
	}{
		{name: "no annotation"},
		{name: "another namespace listed", dependentsFrom: ptr.To("preview-1, preview-2")},
		{name: "listed", dependentsFrom: ptr.To("preview-1, preview-42"), wantAllowed: true},
		{name: "any namespace", dependentsFrom: ptr.To("*"), wantAllowed: true},
		{name: "learned, no annotation", learn: true},
		{name: "learned, listed", dependentsFrom: ptr.To("preview-42"), learn: true, wantAllowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
			if tt.dependentsFrom != nil {
				ns.Annotations = map[string]string{v1alpha1.AnnotationDependentsFrom: *tt.dependentsFrom}
			}
			dependent := func(phase v1alpha1.WorkloadPhase) *v1alpha1.ManagedWorkload {
				w := depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, phase)
				if tt.learn {
					w.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
						{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
							Source: v1alpha1.LearnedFromEnvironment, Via: "PGHOST"}}}
				} else {
					w.Spec.DependsOn = []v1alpha1.DependencyRef{shared}
				}
				return w
			}

			t.Run("held", func(t *testing.T) {
				postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
				pauser := &stubPauser{pauseDone: true}
				r := depReconciler(t, pauser, ns.DeepCopy(), postgres, dependent(v1alpha1.PhaseRunning))

				_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
				require.NoError(t, err)

				assert.Equal(t, tt.wantAllowed, pauser.pauseCalls == 0)
			})
			t.Run("woken and waited for", func(t *testing.T) {
				pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
				api := dependent(v1alpha1.PhasePaused)
				api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
				postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
				target := postgresTarget(0, 0)
				target.Namespace = "default"
				pauser := &stubPauser{resumeDone: true}
				r := depReconciler(t, pauser, ns.DeepCopy(), api, postgres, target)

				_, err := r.handleResume(context.Background(), api, nil)
				require.NoError(t, err)

				woken := fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity] != ""
				assert.Equal(t, tt.wantAllowed, woken)
				if !tt.learn {
					assert.Equal(t, !tt.wantAllowed, pauser.resumeCalls == 1, "only an allowed dependency is waited for")
				}
			})
			t.Run("reported", func(t *testing.T) {
				api := dependent(v1alpha1.PhaseRunning)
				r := depReconciler(t, &stubPauser{}, ns.DeepCopy(), api)

				require.NoError(t, r.checkDependenciesExist(context.Background(), api))

				c := meta.FindStatusCondition(api.Status.Conditions, conditionDependencyNotAllowed)
				if tt.wantAllowed {
					assert.Nil(t, c)
					assert.True(t, meta.IsStatusConditionTrue(api.Status.Conditions, conditionDependencyNotFound) == !tt.learn,
						"an allowed one is looked for")
					return
				}
				require.NotNil(t, c)
				assert.Equal(t, "not in a namespace that allows dependents from preview-42 with hybernate.io/dependents-from, "+
					"so not held or woken: default/statefulset/postgres", c.Message)
				assert.Nil(t, meta.FindStatusCondition(api.Status.Conditions, conditionDependencyNotFound),
					"one not allowed isn't looked for, so whether it exists isn't told")
			})
		})
	}
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
			if tt.wantMissing {
				assert.Equal(t, "not found: default/statefulset/postgres",
					meta.FindStatusCondition(api.Status.Conditions, conditionDependencyNotFound).Message)
			}
		})
	}
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

// rejectingPatches makes every patch of the ManagedWorkload named fail, as
// a policy engine rejecting Hybernate's writes there would.
func rejectingPatches(r *Reconciler, name string) {
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if _, ok := obj.(*v1alpha1.ManagedWorkload); ok && obj.GetName() == name {
				return apierrors.NewForbidden(v1alpha1.GroupVersion.WithResource("managedworkloads").GroupResource(),
					name, errors.New("denied by policy"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

// A dependency that can't be woken, such as one in a namespace whose
// policies reject Hybernate's writes, never keeps its dependent from
// waking or being handed back.
func TestDependencies_WakeIsBestEffort(t *testing.T) {
	paused := func() *v1alpha1.ManagedWorkload {
		pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
		api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef())
		api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
		return api
	}
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)

	t.Run("waking", func(t *testing.T) {
		pauser := &stubPauser{resumeDone: true}
		r := depReconciler(t, pauser, paused(), postgres.DeepCopy(), postgresTarget(1, 1))
		rejectingPatches(r, "postgres")
		api := fetch(t, r, "api")

		_, err := r.handleResume(context.Background(), api, nil)
		require.NoError(t, err)

		assert.Equal(t, v1alpha1.PhaseRunning, api.Status.Phase)
		assert.Equal(t, 1, pauser.resumeCalls)
		assert.Len(t, recorded(r.Recorder.(*events.FakeRecorder), ReasonDependencyWakeFailed), 1)
	})
	t.Run("handed back", func(t *testing.T) {
		pauser := &stubPauser{}
		r := depReconciler(t, pauser, paused(), postgres.DeepCopy())
		rejectingPatches(r, "postgres")

		released, replicas, err := r.releaseTarget(context.Background(), fetch(t, r, "api"))
		require.NoError(t, err)

		assert.True(t, released)
		assert.Equal(t, int32(1), replicas)
		assert.Len(t, recorded(r.Recorder.(*events.FakeRecorder), ReasonDependencyWakeFailed), 1)
	})
}

// A ManagedWorkload in a namespace being deleted lets go of a target it
// can't hand back, once it has tried for a while, so it can't hold the
// namespace's deletion forever.
func TestReconcileDelete_GivesUpInATerminatingNamespace(t *testing.T) {
	tests := []struct {
		name        string
		terminating bool
		deletedAgo  time.Duration
		wantGone    bool
	}{
		{name: "terminating, tried for long enough", terminating: true, deletedAgo: 6 * time.Minute, wantGone: true},
		{name: "terminating, only just deleted", terminating: true, deletedAgo: time.Minute},
		{name: "namespace staying", deletedAgo: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
			if tt.terminating {
				ns.Status.Phase = corev1.NamespaceTerminating
			}
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused)
			api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1}
			api.Finalizers = []string{finalizerName}
			api.DeletionTimestamp = &metav1.Time{Time: fixedTime.Add(-tt.deletedAgo)}
			r := depReconciler(t, &stubPauser{restoreErr: errors.New("admission webhook keda is unavailable")}, ns, api)

			_, err := r.reconcileDelete(context.Background(), fetch(t, r, "api"))

			var w v1alpha1.ManagedWorkload
			gone := apierrors.IsNotFound(r.Get(context.Background(), client.ObjectKeyFromObject(api), &w))
			assert.Equal(t, tt.wantGone, gone)
			abandoned := recorded(r.Recorder.(*events.FakeRecorder), ReasonHandBackAbandoned)
			if tt.wantGone {
				require.NoError(t, err)
				assert.Len(t, abandoned, 1)
				return
			}
			require.Error(t, err, "the hand-back is retried")
			assert.Empty(t, abandoned)
		})
	}
}

// A workload woken by a scale-up wakes its dependencies, as any wake does.
func TestDependencies_ScaleUpWakesDependencies(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, postgresRef())
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
	postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
	r := depReconciler(t, &stubPauser{}, api, postgres, targetDeploymentWithReplicas("api", "default", 1))

	require.NoError(t, r.wakeOnScaleUp(context.Background(), api, scaledBy("kubectl-edit", 1)))

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

	require.NoError(t, direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shared",
		Annotations: map[string]string{v1alpha1.AnnotationDependentsFrom: "default"}}}))
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

	_, err = r.handleResume(ctx, &w, nil)
	require.NoError(t, err, "waking isn't blocked by a dependency it can't see")
	assert.Equal(t, 1, pauser.resumeCalls)
	assert.Equal(t, v1alpha1.PhaseRunning, w.Status.Phase)
}
