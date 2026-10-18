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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

// optInEnv is a faultEnv namespace whose KEDA-scaled Deployment is opted in
// with the managed label, and an opt-in reconciler for it.
func optInEnv(t *testing.T, env *faultEnv, ns string) (*OptInReconciler, ctrl.Request) {
	t.Helper()
	require.NoError(t, env.admin.Create(env.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	so := envtestScaledObject(ns, "api")
	so.SetAnnotations(map[string]string{autoscaler.PausedReplicasAnnotation: "7"})
	require.NoError(t, env.admin.Create(env.ctx, so))
	d := envtestDeployment(ns, 3)
	d.Labels = managedLabel
	require.NoError(t, env.admin.Create(env.ctx, d))
	r := &OptInReconciler{Client: env.admin, Scheme: testScheme(t), Recorder: events.NewFakeRecorder(100),
		Kind: v1alpha1.TargetKindDeployment, Defaults: DefaultOptInDefaults}
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: "api"}}
	_, err := r.Reconcile(env.ctx, req)
	require.NoError(t, err)
	return r, req
}

// Removing the managed label from a paused workload hands it back as it
// was: its replicas, and its ScaledObject's own paused-replicas.
func TestOptIn_RemovingTheLabelFromAPausedWorkloadRestoresIt(t *testing.T) {
	env := startFaultEnv(t)
	optIn, req := optInEnv(t, env, "shop")
	r := env.reconciler(t, env.admin)

	w := env.workload(t, req.NamespacedName)
	withPauseRequested(w)
	require.NoError(t, env.admin.Update(env.ctx, w))
	env.reconcileUntil(t, r, req.NamespacedName, v1alpha1.PhasePaused, 10)
	require.Equal(t, int32(0), *env.deployment(t, req.NamespacedName).Spec.Replicas)
	v, _ := env.pausedReplicasAnnotation(t, req.NamespacedName)
	require.Equal(t, "0", v, "KEDA held at zero")

	d := env.deployment(t, req.NamespacedName)
	d.Labels = nil
	require.NoError(t, env.admin.Update(env.ctx, d))
	_, err := optIn.Reconcile(env.ctx, req)
	require.NoError(t, err)
	env.reconcileUntil(t, r, req.NamespacedName, "gone", 10)

	assert.Equal(t, int32(3), *env.deployment(t, req.NamespacedName).Spec.Replicas, "replicas restored")
	v, ok := env.pausedReplicasAnnotation(t, req.NamespacedName)
	assert.True(t, ok)
	assert.Equal(t, "7", v, "the ScaledObject's own paused-replicas is put back")
}

// A workload deleted and created again under the same name, as kubectl
// replace --force or a Helm reinstall does, finds its label-created
// ManagedWorkload still there before garbage collection gets to it. It
// takes it over, with what it has learned and recorded, rather than leave
// it owned by the workload that's gone, for the collector to delete.
func TestOptIn_RecreatedWorkloadAdoptsItsManagedWorkload(t *testing.T) {
	env := startFaultEnv(t)
	optIn, req := optInEnv(t, env, "shop")
	before := env.workload(t, req.NamespacedName)
	require.Len(t, before.OwnerReferences, 1)

	old := env.deployment(t, req.NamespacedName)
	require.NoError(t, env.admin.Delete(env.ctx, old))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(env.admin.Get(env.ctx, req.NamespacedName, &appsv1.Deployment{}))
	}, 10*time.Second, 50*time.Millisecond)
	recreated := envtestDeployment("shop", 3)
	recreated.Labels = managedLabel
	require.NoError(t, env.admin.Create(env.ctx, recreated))
	_, err := optIn.Reconcile(env.ctx, req)
	require.NoError(t, err)

	after := env.workload(t, req.NamespacedName)
	assert.Equal(t, before.UID, after.UID, "the same ManagedWorkload")
	require.Len(t, after.OwnerReferences, 1)
	assert.Equal(t, recreated.UID, after.OwnerReferences[0].UID, "owned by the new workload, not the deleted one")
}
