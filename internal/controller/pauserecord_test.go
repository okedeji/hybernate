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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

// apiScaler is the KEDA ScaledObject that scales default/api.
const apiScaler = "api-scaler"

// apiScaledObject is default/api's ScaledObject, its own paused-replicas
// annotation set to pausedReplicas.
func apiScaledObject(pausedReplicas string) *unstructured.Unstructured {
	so := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"scaleTargetRef": map[string]any{"name": "api"}, "minReplicaCount": int64(1)}}}
	so.SetGroupVersionKind(kedaScaledObject)
	so.SetNamespace("default")
	so.SetName(apiScaler)
	so.SetAnnotations(map[string]string{autoscaler.PausedReplicasAnnotation: pausedReplicas})
	return so
}

func targetDeploymentOf(t *testing.T, r *Reconciler) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "api"}, &d))
	return &d
}

// copyPauseRecord puts a copy of a pause record on default/api, as a pause
// leaves one.
func copyPauseRecord(t *testing.T, r *Reconciler, record string) {
	t.Helper()
	d := targetDeploymentOf(t, r)
	d.Annotations = map[string]string{v1alpha1.AnnotationPausedFrom: record}
	require.NoError(t, r.Update(context.Background(), d))
}

// A paused workload whose ManagedWorkload loses its status, as to a backup
// restored without it, or the ManagedWorkload being created again, is
// taken up from the copy its target carries: Paused again, then woken to
// the replicas it had, with KEDA given back its own paused-replicas value.
// Without it the target would be taken for one scaled to zero outside
// Hybernate and left off for good, held there by KEDA.
func TestPause_StatusLostWhilePausedIsTakenUpFromTheTarget(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	r := lifecycleReconciler(t, workload, 3, interceptor.Funcs{}, apiScaledObject("5"))
	reconcileUntilSettled(t, r)
	require.Equal(t, v1alpha1.PhasePaused, getWorkload(t, r, "api").Status.Phase)
	require.Equal(t, int32(0), targetReplicas(t, r))

	lost := getWorkload(t, r, "api")
	lost.Status = v1alpha1.ManagedWorkloadStatus{}
	require.NoError(t, r.Status().Update(context.Background(), lost))
	reconcileUntilSettled(t, r)

	recovered := getWorkload(t, r, "api")
	require.Equal(t, v1alpha1.PhasePaused, recovered.Status.Phase, "not left off as scaled to zero outside Hybernate")
	require.NotNil(t, recovered.Status.Pause)
	assert.Equal(t, int32(3), recovered.Status.Pause.PreviousReplicas)
	assert.Contains(t, drainEvents(t, r), ReasonPauseRecovered)

	require.NoError(t, r.Update(context.Background(), wokenByActivity(recovered)))
	reconcileUntilSettled(t, r)

	assert.Equal(t, v1alpha1.PhaseRunning, getWorkload(t, r, "api").Status.Phase)
	assert.Equal(t, int32(3), targetReplicas(t, r))
	v, _ := pausedScaledObject(t, r)
	assert.Equal(t, "5", v, "KEDA is given back its own value, not left holding the target at zero")
	assert.NotContains(t, targetDeploymentOf(t, r).Annotations, v1alpha1.AnnotationPausedFrom)
}

// The copy is only taken up while the target is at zero: one scaled up
// since is released, and a copy that can't be read is reported, not acted
// on.
func TestPause_RecordCopyOnARunningTarget(t *testing.T) {
	tests := []struct {
		name   string
		record string
		event  string
	}{
		{name: "scaled up since", record: `{"replicas":3}`},
		{name: "unreadable", record: "3", event: ReasonPauseRecordLost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := lifecycleReconciler(t, lifecycleWorkload("api", v1alpha1.PhaseRunning), 2, interceptor.Funcs{})
			copyPauseRecord(t, r, tt.record)

			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
			assert.Nil(t, got.Status.Pause)
			assert.Equal(t, int32(2), targetReplicas(t, r))
			if tt.event != "" {
				assert.Contains(t, drainEvents(t, r), tt.event)
				return
			}
			assert.NotContains(t, targetDeploymentOf(t, r).Annotations, v1alpha1.AnnotationPausedFrom)
		})
	}
}

// A pause is Paused once the target's pods have stopped. Until then it's
// Pausing, with when it was scaled to zero written so the wait survives a
// restart; pods that never stop are waited for only so long, and reported.
func TestPause_WaitsForThePodsToStop(t *testing.T) {
	workload := withPauseRequested(lifecycleWorkload("api", v1alpha1.PhaseRunning))
	pauser := &stubPauser{}
	r := newTestReconcilerWithReplicas(t, workload, pauser, 3)

	res, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	got := getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePausing, got.Status.Phase)
	require.NotNil(t, got.Status.Pause.PausedAt, "written as it's scaled to zero")
	assert.Equal(t, 5*time.Second, res.RequeueAfter)

	scaledDown := targetDeploymentOf(t, r)
	scaledDown.Spec.Replicas = ptr.To[int32](0)
	require.NoError(t, r.Update(context.Background(), scaledDown))
	pauser.pauseDone, pauser.podsRunning = true, 2
	_, err = r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	got = getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase, "never wedged in Pausing")
	c := meta.FindStatusCondition(got.Status.Conditions, conditionPauseIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Contains(t, c.Message, "2 pods still running")
	assert.Contains(t, drainEvents(t, r), ReasonPauseIncomplete)

	_, err = r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	got = getWorkload(t, r, "api")
	assert.Equal(t, v1alpha1.PhasePaused, got.Status.Phase)
	c = meta.FindStatusCondition(got.Status.Conditions, conditionPauseIncomplete)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "cleared once they've gone")
	assert.Equal(t, reasonPodsStopped, c.Reason)
}
