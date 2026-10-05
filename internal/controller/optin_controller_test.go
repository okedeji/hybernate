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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func optInNamespace(labels, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev", Labels: labels, Annotations: annotations}}
}

func optInDeployment(name string, labels, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "dev", UID: types.UID("uid-" + name), Labels: labels, Annotations: annotations,
	}}
}

func optInReconciler(t *testing.T, objs ...client.Object) (*OptInReconciler, *events.FakeRecorder) {
	t.Helper()
	recorder := events.NewFakeRecorder(20)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &OptInReconciler{Client: c, Scheme: testScheme(t), Recorder: recorder,
		Kind: v1alpha1.TargetKindDeployment, Defaults: DefaultOptInDefaults}, recorder
}

func reconcileOptIn(t *testing.T, r *OptInReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "dev", Name: name}})
	require.NoError(t, err)
}

func managedWorkload(t *testing.T, r *OptInReconciler, name string) (*v1alpha1.ManagedWorkload, bool) {
	t.Helper()
	var mw v1alpha1.ManagedWorkload
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "dev", Name: name}, &mw)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &mw, true
}

func recorded(recorder *events.FakeRecorder, reason string) []string {
	var out []string
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, reason) {
			out = append(out, e)
		}
	}
	return out
}

var managedLabel = map[string]string{v1alpha1.LabelManaged: "true"}

func TestOptIn_LabelledWorkloadIsManaged(t *testing.T) {
	labels := map[string]string{v1alpha1.LabelManaged: "true", "app.kubernetes.io/instance": "shop"}
	annotations := map[string]string{v1alpha1.AnnotationDryRun: "true", v1alpha1.AnnotationIdleAfter: "2h"}
	r, recorder := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", labels, annotations))

	reconcileOptIn(t, r, "api")

	mw, ok := managedWorkload(t, r, "api")
	require.True(t, ok)
	assert.Equal(t, v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}, mw.Spec.Target)
	assert.True(t, mw.Spec.DryRun)
	assert.Equal(t, 2*time.Hour, mw.Spec.IdlePolicy.IdleAfter.Duration)
	assert.Equal(t, map[string]string{v1alpha1.LabelFromLabel: "true"}, mw.Labels,
		"the workload's labels, including a GitOps tool's tracking label, aren't copied")
	require.Len(t, mw.OwnerReferences, 1)
	assert.Equal(t, types.UID("uid-api"), mw.OwnerReferences[0].UID, "deleted along with the workload")
	assert.Len(t, recorded(recorder, ReasonManaged), 1)
}

func TestOptIn_Namespace(t *testing.T) {
	ns := optInNamespace(managedLabel, map[string]string{v1alpha1.AnnotationIdleAfter: "3h"})
	r, _ := optInReconciler(t, ns,
		optInDeployment("web", nil, nil),
		optInDeployment("worker", nil, map[string]string{v1alpha1.AnnotationIdleAfter: "20m"}),
		optInDeployment("tools", map[string]string{v1alpha1.LabelIgnore: "true"}, nil))

	for _, name := range []string{"web", "worker", "tools"} {
		reconcileOptIn(t, r, name)
	}

	web, ok := managedWorkload(t, r, "web")
	require.True(t, ok, "a managed namespace covers every workload in it")
	assert.Equal(t, 3*time.Hour, web.Spec.IdlePolicy.IdleAfter.Duration, "with the namespace's settings")
	worker, ok := managedWorkload(t, r, "worker")
	require.True(t, ok)
	assert.Equal(t, 20*time.Minute, worker.Spec.IdlePolicy.IdleAfter.Duration, "a workload's own setting wins")
	_, ok = managedWorkload(t, r, "tools")
	assert.False(t, ok, "ignore opts a workload out")
}

func TestOptIn_NotOptedIn(t *testing.T) {
	r, recorder := optInReconciler(t, optInNamespace(nil, nil),
		optInDeployment("plain", nil, nil),
		optInDeployment("typo", map[string]string{v1alpha1.LabelManaged: "dry-run"}, nil))

	reconcileOptIn(t, r, "plain")
	reconcileOptIn(t, r, "typo")

	_, ok := managedWorkload(t, r, "plain")
	assert.False(t, ok)
	_, ok = managedWorkload(t, r, "typo")
	assert.False(t, ok)
	ignored := recorded(recorder, ReasonLabelIgnored)
	require.Len(t, ignored, 1, "a value that isn't true is pointed out")
	assert.Contains(t, ignored[0], "settings such as dry-run are annotations")
}

func TestOptIn_InvalidSettingIsReported(t *testing.T) {
	r, recorder := optInReconciler(t, optInNamespace(nil, nil),
		optInDeployment("api", managedLabel, map[string]string{v1alpha1.AnnotationIdleAfter: "soon"}))

	reconcileOptIn(t, r, "api")

	mw, ok := managedWorkload(t, r, "api")
	require.True(t, ok, "a bad setting doesn't stop it being managed")
	assert.Equal(t, time.Hour, mw.Spec.IdlePolicy.IdleAfter.Duration)
	warnings := recorded(recorder, ReasonInvalidSetting)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `hybernate.io/idle-after="soon"`)
}

func TestOptIn_AnnotationChangesFollow(t *testing.T) {
	d := optInDeployment("api", managedLabel, nil)
	r, _ := optInReconciler(t, optInNamespace(nil, nil), d)
	reconcileOptIn(t, r, "api")

	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(d), d))
	d.Annotations = map[string]string{v1alpha1.AnnotationDryRun: "true"}
	require.NoError(t, r.Update(context.Background(), d))
	reconcileOptIn(t, r, "api")

	mw, _ := managedWorkload(t, r, "api")
	assert.True(t, mw.Spec.DryRun)
}

// Running it again with nothing changed writes nothing, so it can't fight
// itself or the API server's defaults.
func TestOptIn_Idempotent(t *testing.T) {
	r, _ := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil))
	reconcileOptIn(t, r, "api")
	first, _ := managedWorkload(t, r, "api")

	reconcileOptIn(t, r, "api")

	second, _ := managedWorkload(t, r, "api")
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion)
}

func TestOptIn_RemovingTheLabelReleasesIt(t *testing.T) {
	d := optInDeployment("api", managedLabel, nil)
	r, recorder := optInReconciler(t, optInNamespace(nil, nil), d)
	reconcileOptIn(t, r, "api")

	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(d), d))
	d.Labels = nil
	require.NoError(t, r.Update(context.Background(), d))
	reconcileOptIn(t, r, "api")

	_, ok := managedWorkload(t, r, "api")
	assert.False(t, ok)
	assert.Len(t, recorded(recorder, ReasonNoLongerManaged), 1)
}

// A ManagedWorkload someone writes always wins over the label.
func TestOptIn_AWrittenManagedWorkloadWins(t *testing.T) {
	written := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api-by-hand", Namespace: "dev"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
	ours := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev", Labels: map[string]string{v1alpha1.LabelFromLabel: "true"}},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
	r, _ := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil), written, ours)

	reconcileOptIn(t, r, "api")

	_, ok := managedWorkload(t, r, "api")
	assert.False(t, ok, "the label-created one gives way")
	_, ok = managedWorkload(t, r, "api-by-hand")
	assert.True(t, ok, "the written one is left alone")
}

func TestOptIn_NameTakenByAnotherWorkloadsManagedWorkload(t *testing.T) {
	other := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "legacy"}},
	}
	r, _ := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil), other)

	reconcileOptIn(t, r, "api")

	mw, _ := managedWorkload(t, r, "api")
	assert.Equal(t, "legacy", mw.Spec.Target.Name, "someone else's ManagedWorkload is never overwritten")
	ours, ok := managedWorkload(t, r, "api-deployment")
	require.True(t, ok, "the workload's own is named after its kind too")
	assert.Equal(t, "api", ours.Spec.Target.Name)
}

func TestOptIn_BothNamesTaken(t *testing.T) {
	taken := func(name, target string) *v1alpha1.ManagedWorkload {
		return &v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dev"},
			Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: target}}}
	}
	r, recorder := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil),
		taken("api", "legacy"), taken("api-deployment", "older"))

	reconcileOptIn(t, r, "api")

	var list v1alpha1.ManagedWorkloadList
	require.NoError(t, r.List(context.Background(), &list))
	assert.Len(t, list.Items, 2)
	assert.Len(t, recorded(recorder, "already exist for other workloads"), 1)
}

// A protected namespace's workloads aren't opted in, whatever their
// labels, and one opted in before it was protected is released.
func TestOptIn_ProtectedNamespace(t *testing.T) {
	ns := optInNamespace(map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelProtected: "true"}, nil)
	r, recorder := optInReconciler(t, ns, optInDeployment("api", managedLabel, nil),
		&v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev",
			Labels: map[string]string{v1alpha1.LabelFromLabel: "true"}},
			Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}}})

	reconcileOptIn(t, r, "api")

	_, ok := managedWorkload(t, r, "api")
	assert.False(t, ok)
	assert.Len(t, recorded(recorder, ReasonProtected), 1)
}

func statefulSetReconciler(deployments *OptInReconciler) *OptInReconciler {
	return &OptInReconciler{Client: deployments.Client, Scheme: deployments.Scheme,
		Recorder: events.NewFakeRecorder(20), Kind: v1alpha1.TargetKindStatefulSet, Defaults: DefaultOptInDefaults}
}

// A Deployment and a StatefulSet can share a name. Each gets its own
// ManagedWorkload, whichever is opted in first, and neither is ever
// retargeted at the other.
func TestOptIn_DeploymentAndStatefulSetWithTheSameName(t *testing.T) {
	deploymentRef := v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}
	statefulSetRef := v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "api"}
	tests := []struct {
		name       string
		first      v1alpha1.TargetKind
		pauseFirst bool
		wantByName map[string]v1alpha1.WorkloadRef
	}{
		{name: "deployment first", first: v1alpha1.TargetKindDeployment,
			wantByName: map[string]v1alpha1.WorkloadRef{"api": deploymentRef, "api-statefulset": statefulSetRef}},
		{name: "statefulset first", first: v1alpha1.TargetKindStatefulSet,
			wantByName: map[string]v1alpha1.WorkloadRef{"api": statefulSetRef, "api-deployment": deploymentRef}},
		{name: "deployment first and paused", first: v1alpha1.TargetKindDeployment, pauseFirst: true,
			wantByName: map[string]v1alpha1.WorkloadRef{"api": deploymentRef, "api-statefulset": statefulSetRef}},
		{name: "statefulset first and paused", first: v1alpha1.TargetKindStatefulSet, pauseFirst: true,
			wantByName: map[string]v1alpha1.WorkloadRef{"api": statefulSetRef, "api-deployment": deploymentRef}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev", UID: "uid-sts-api"}}
			deployments, _ := optInReconciler(t, optInNamespace(managedLabel, nil), optInDeployment("api", nil, nil), sts)
			statefulSets := statefulSetReconciler(deployments)
			first, second := deployments, statefulSets
			if tt.first == v1alpha1.TargetKindStatefulSet {
				first, second = statefulSets, deployments
			}

			reconcileOptIn(t, first, "api")
			if tt.pauseFirst {
				mw, _ := managedWorkload(t, first, "api")
				mw.Status.Phase = v1alpha1.PhasePaused
				mw.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3}
				require.NoError(t, first.Update(context.Background(), mw))
			}
			for range 3 {
				reconcileOptIn(t, second, "api")
				reconcileOptIn(t, first, "api")
			}

			var list v1alpha1.ManagedWorkloadList
			require.NoError(t, deployments.List(context.Background(), &list, client.InNamespace("dev")))
			got := map[string]v1alpha1.WorkloadRef{}
			for _, mw := range list.Items {
				got[mw.Name] = mw.Spec.Target
				assert.Len(t, mw.OwnerReferences, 1, "%s is owned by its own workload only", mw.Name)
			}
			assert.Equal(t, tt.wantByName, got)
			if tt.pauseFirst {
				mw, _ := managedWorkload(t, first, "api")
				assert.Equal(t, v1alpha1.PhasePaused, mw.Status.Phase)
				require.NotNil(t, mw.Status.Pause)
				assert.Equal(t, int32(3), mw.Status.Pause.PreviousReplicas, "the paused workload keeps its replicas")
			}
		})
	}
}

// An earlier version gave a ManagedWorkload an owner reference to each of
// a Deployment and a StatefulSet sharing a name. It keeps only its own
// target's, so deleting that target deletes it, and deleting the other
// doesn't.
func TestOptIn_DropsAnotherWorkloadsOwnerReference(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev", UID: "uid-sts-api"}}
	deployment := optInDeployment("api", nil, nil)
	r, _ := optInReconciler(t, optInNamespace(managedLabel, nil), deployment, sts)
	reconcileOptIn(t, r, "api")
	mw, ok := managedWorkload(t, r, "api")
	require.True(t, ok)
	mw.OwnerReferences = append(mw.OwnerReferences, metav1.OwnerReference{APIVersion: "apps/v1",
		Kind: "StatefulSet", Name: "api", UID: sts.UID})
	require.NoError(t, r.Update(context.Background(), mw))

	reconcileOptIn(t, r, "api")

	mw, ok = managedWorkload(t, r, "api")
	require.True(t, ok)
	require.Len(t, mw.OwnerReferences, 1)
	assert.Equal(t, deployment.UID, mw.OwnerReferences[0].UID)
}

// Rerunning either reconciler once both exist writes nothing.
func TestOptIn_SameNameIsStable(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev", UID: "uid-sts-api"}}
	deployments, _ := optInReconciler(t, optInNamespace(managedLabel, nil), optInDeployment("api", nil, nil), sts)
	statefulSets := statefulSetReconciler(deployments)
	reconcileOptIn(t, deployments, "api")
	reconcileOptIn(t, statefulSets, "api")
	before, _ := managedWorkload(t, deployments, "api")
	beforeSTS, _ := managedWorkload(t, deployments, "api-statefulset")

	reconcileOptIn(t, deployments, "api")
	reconcileOptIn(t, statefulSets, "api")

	after, _ := managedWorkload(t, deployments, "api")
	afterSTS, _ := managedWorkload(t, deployments, "api-statefulset")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Equal(t, beforeSTS.ResourceVersion, afterSTS.ResourceVersion)
}

func TestKindQualifiedName(t *testing.T) {
	assert.Equal(t, "api-statefulset", kindQualifiedName("api", v1alpha1.TargetKindStatefulSet))

	long := strings.Repeat("a", 240) + ".b"
	other := strings.Repeat("a", 240) + ".c"
	got := kindQualifiedName(long, v1alpha1.TargetKindDeployment)
	assert.Len(t, got, 253)
	assert.True(t, strings.HasSuffix(got, "-deployment"))
	assert.NotEqual(t, got, kindQualifiedName(other, v1alpha1.TargetKindDeployment), "long names sharing a prefix differ")
	assert.Empty(t, validation.IsDNS1123Subdomain(got))

	dotted := strings.Repeat("a", 231) + "." + strings.Repeat("b", 20)
	assert.Empty(t, validation.IsDNS1123Subdomain(kindQualifiedName(dotted, v1alpha1.TargetKindStatefulSet)),
		"a cut that ends on a dot is still a valid name")
}

// Settings no annotation controls, such as a desiredState patched in as
// the pause guide shows, survive the annotations being applied again.
func TestOptIn_UserSettingsSurvive(t *testing.T) {
	d := optInDeployment("api", managedLabel, map[string]string{v1alpha1.AnnotationDependsOn: "statefulset/postgres"})
	r, _ := optInReconciler(t, optInNamespace(nil, nil), d)
	reconcileOptIn(t, r, "api")
	mw, _ := managedWorkload(t, r, "api")
	cpu := resource.MustParse("0.05")
	mw.Spec.DesiredState = ptr.To(v1alpha1.DesiredStatePaused)
	mw.Spec.Prediction.Confidence = 70
	mw.Spec.CostTracking = &v1alpha1.CostTrackingSpec{Rates: &v1alpha1.CostRates{CPUPerHour: &cpu}}
	mw.Spec.IdlePolicy.Activity.Prometheus = []v1alpha1.PrometheusActivity{{PromQL: "sum(up)"}}
	mw.Spec.DependsOn[0].WaitForReady = true
	mw.Spec.DryRun = true
	require.NoError(t, r.Update(context.Background(), mw))

	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(d), d))
	d.Annotations[v1alpha1.AnnotationIdleAfter] = "3h"
	require.NoError(t, r.Update(context.Background(), d))
	reconcileOptIn(t, r, "api")

	mw, _ = managedWorkload(t, r, "api")
	assert.Equal(t, ptr.To(v1alpha1.DesiredStatePaused), mw.Spec.DesiredState)
	assert.Equal(t, 70, mw.Spec.Prediction.Confidence)
	assert.NotNil(t, mw.Spec.CostTracking)
	assert.Equal(t, []v1alpha1.PrometheusActivity{{PromQL: "sum(up)"}}, mw.Spec.IdlePolicy.Activity.Prometheus)
	assert.True(t, mw.Spec.DependsOn[0].WaitForReady)
	assert.Equal(t, 3*time.Hour, mw.Spec.IdlePolicy.IdleAfter.Duration, "the annotations still apply")
	assert.False(t, mw.Spec.DryRun, "dry-run is the annotations' to set")
}

// Opting in again while the old ManagedWorkload is still restoring the
// workload waits for it to go, rather than making a second one.
func TestOptIn_WaitsForTheOldManagedWorkloadToGo(t *testing.T) {
	leaving := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "dev", Finalizers: []string{finalizerName},
			Labels: map[string]string{v1alpha1.LabelFromLabel: "true"}},
		Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
	r, _ := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil), leaving)
	require.NoError(t, r.Delete(context.Background(), leaving))

	reconcileOptIn(t, r, "api")

	var list v1alpha1.ManagedWorkloadList
	require.NoError(t, r.List(context.Background(), &list))
	assert.Len(t, list.Items, 1)
}

// A bad value on the namespace is pointed out on the workload it applies
// to, saying where it is.
func TestOptIn_InvalidNamespaceSettingIsReported(t *testing.T) {
	r, recorder := optInReconciler(t, optInNamespace(managedLabel, map[string]string{v1alpha1.AnnotationIdleAfter: "soon"}),
		optInDeployment("api", nil, nil))

	reconcileOptIn(t, r, "api")

	warnings := recorded(recorder, ReasonInvalidSetting)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `hybernate.io/idle-after="soon" on the namespace`)
}

// Labelling a namespace, or removing its label, reaches its workloads
// through the namespace watch alone: nothing about the workloads changes.
func TestOptIn_NamespaceLabelThroughTheWatch(t *testing.T) {
	cfg := startEnvtest(t)
	scheme := testScheme(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	require.NoError(t, err)
	r := &OptInReconciler{Client: mgr.GetClient(), Scheme: scheme, Recorder: events.NewFakeRecorder(100),
		Kind: v1alpha1.TargetKindDeployment, Defaults: DefaultOptInDefaults}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, stop := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { assert.NoError(t, mgr.Start(mgrCtx)) })
	t.Cleanup(func() {
		stop()
		running.Wait()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	require.NoError(t, c.Create(ctx, ns))
	labels := map[string]string{"app": "web"}
	require.NoError(t, c.Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:v1"}}}}},
	}))
	key := types.NamespacedName{Namespace: "shop", Name: "web"}
	exists := func() bool { return c.Get(ctx, key, &v1alpha1.ManagedWorkload{}) == nil }
	gone := func() bool { return apierrors.IsNotFound(c.Get(ctx, key, &v1alpha1.ManagedWorkload{})) }

	ns.Labels = managedLabel
	require.NoError(t, c.Update(ctx, ns))
	require.Eventually(t, exists, 10*time.Second, 50*time.Millisecond, "labelling the namespace opts its workloads in")

	var mw v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(ctx, key, &mw))
	mw.Status.Phase = v1alpha1.PhasePaused
	mw.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2}
	require.NoError(t, c.Status().Update(ctx, &mw))

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Labels = nil
	require.NoError(t, c.Update(ctx, ns))
	require.Eventually(t, gone, 10*time.Second, 50*time.Millisecond,
		"removing the namespace's label releases a paused workload, whose ManagedWorkload's deletion restores it")
}
