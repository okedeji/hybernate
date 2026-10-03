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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func optInNamespace(labels, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Labels: labels, Annotations: annotations}}
}

func optInDeployment(name string, labels, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "sandbox", UID: types.UID("uid-" + name), Labels: labels, Annotations: annotations,
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
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "sandbox", Name: name}})
	require.NoError(t, err)
}

func managedWorkload(t *testing.T, r *OptInReconciler, name string) (*v1alpha1.ManagedWorkload, bool) {
	t.Helper()
	var mw v1alpha1.ManagedWorkload
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "sandbox", Name: name}, &mw)
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
		ObjectMeta: metav1.ObjectMeta{Name: "api-by-hand", Namespace: "sandbox"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
	ours := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "sandbox", Labels: map[string]string{v1alpha1.LabelFromLabel: "true"}},
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
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "sandbox"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "legacy"}},
	}
	r, recorder := optInReconciler(t, optInNamespace(nil, nil), optInDeployment("api", managedLabel, nil), other)

	reconcileOptIn(t, r, "api")

	mw, _ := managedWorkload(t, r, "api")
	assert.Equal(t, "legacy", mw.Spec.Target.Name, "someone else's ManagedWorkload is never overwritten")
	assert.Len(t, recorded(recorder, "already exists for another workload"), 1)
}
