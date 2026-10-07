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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func defaultNamespace(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: labels}}
}

var protectedLabel = map[string]string{v1alpha1.LabelProtected: v1alpha1.True}

// Hybernate pauses nothing in a protected namespace, and wakes what it had
// paused there, so protecting one never leaves a workload off.
func TestReconcile_ProtectedNamespace(t *testing.T) {
	tests := []struct {
		name       string
		phase      v1alpha1.WorkloadPhase
		ns         *corev1.Namespace
		patterns   []string
		wantPhase  v1alpha1.WorkloadPhase
		wantResume bool
	}{
		{name: "idle, by label", phase: v1alpha1.PhaseIdle, ns: defaultNamespace(protectedLabel),
			wantPhase: v1alpha1.PhaseRunning},
		{name: "paused, by pattern", phase: v1alpha1.PhasePaused, ns: defaultNamespace(nil),
			patterns: []string{"def*"}, wantPhase: v1alpha1.PhaseRunning, wantResume: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := depWorkload("default", "api", v1alpha1.TargetKindDeployment, tt.phase)
			workload.Status.Activity.LastActivityTime = metav1.NewTime(fixedTime.Add(-5 * time.Hour))
			pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
			if tt.phase == v1alpha1.PhasePaused {
				workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2, PausedAt: &pausedAt}
			}
			pauser := &stubPauser{pauseDone: true, resumeDone: true}
			r := depReconciler(t, pauser, workload, tt.ns, targetDeploymentWithReplicas("api", "default", 0))
			r.ProtectedNamespaces = tt.patterns

			_, err := r.Reconcile(context.Background(), reconcileFor("api"))
			require.NoError(t, err)

			got := fetch(t, r, "api")
			assert.Equal(t, tt.wantPhase, got.Status.Phase)
			assert.Zero(t, pauser.pauseCalls, "never paused")
			assert.Equal(t, tt.wantResume, pauser.restoreCalls > 0)
			assert.Nil(t, got.Status.Pause)
			c := meta.FindStatusCondition(got.Status.Conditions, conditionProtected)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Contains(t, c.Message, "label it hybernate.io/allow-protected=true to allow it")
		})
	}
}

// Protecting a namespace part-way through a pause, or a resume, hands the
// workload back at the replicas it had rather than leaving it at zero.
func TestReconcile_ProtectedNamespaceFinishesAnInterruptedTransition(t *testing.T) {
	for _, phase := range []v1alpha1.WorkloadPhase{v1alpha1.PhasePausing, v1alpha1.PhasePaused, v1alpha1.PhaseResuming} {
		t.Run(string(phase), func(t *testing.T) {
			workload := pausedWorkload(phase)
			r := realPauserReconciler(t, workload, defaultNamespace(protectedLabel))

			for range 2 {
				_, err := r.Reconcile(context.Background(), reconcileFor("api"))
				require.NoError(t, err)
			}

			got := getWorkload(t, r, "api")
			assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
			assert.Nil(t, got.Status.Pause)
			assert.Equal(t, int32(3), targetReplicas(t, r))
		})
	}
}

func TestReconcile_UnprotectedClearsTheCondition(t *testing.T) {
	workload := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	meta.SetStatusCondition(&workload.Status.Conditions, metav1.Condition{Type: conditionProtected,
		Status: metav1.ConditionTrue, Reason: "ProtectedNamespace"})
	allowed := defaultNamespace(map[string]string{v1alpha1.LabelProtected: v1alpha1.True,
		v1alpha1.LabelAllowProtected: v1alpha1.True})
	r := depReconciler(t, &stubPauser{}, []client.Object{workload, allowed, clockTarget("api:v1", nil)}...)

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	assert.False(t, meta.IsStatusConditionTrue(fetch(t, r, "api").Status.Conditions, conditionProtected))
}
