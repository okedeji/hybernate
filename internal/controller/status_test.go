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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// busyWorkloadReconciler reconciles a running workload with an idle policy
// whose CPU is above the activity threshold, on a clock the test controls.
func busyWorkloadReconciler(t *testing.T) (*Reconciler, *time.Time) {
	t.Helper()
	workload := lifecycleWorkload("busy-app", nil, v1alpha1.PhaseRunning)
	workload.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: time.Hour}}
	r := newTestReconciler(t, workload, &stubPauser{})
	r.metrics = &stubMetrics{cpuMillis: 500, cpuPerReplica: 1000, replicas: 1, memoryBytes: 1 << 30}
	now := fixedTime
	r.clock = func() time.Time { return now }
	return r, &now
}

func reconcileAndVersion(t *testing.T, r *Reconciler) string {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcileFor("busy-app"))
	require.NoError(t, err)
	return getWorkload(t, r, "busy-app").ResourceVersion
}

func TestReconcile_CostAccumulatesForRunningWorkloads(t *testing.T) {
	r, now := busyWorkloadReconciler(t)

	reconcileAndVersion(t, r)
	*now = now.Add(statusFlushInterval)
	reconcileAndVersion(t, r)

	cost := getWorkload(t, r, "busy-app").Status.Cost
	require.NotNil(t, cost, "a running workload with automation must still have its cost tracked")
	assert.Positive(t, cost.CurrentMonthCPUHours.AsApproximateFloat64())
}

func TestReconcile_StatusWritesAreBatched(t *testing.T) {
	r, now := busyWorkloadReconciler(t)

	first := reconcileAndVersion(t, r)

	*now = now.Add(time.Minute)
	assert.Equal(t, first, reconcileAndVersion(t, r),
		"a check that only moves the clock and cost must not write status")

	*now = now.Add(statusFlushInterval)
	assert.NotEqual(t, first, reconcileAndVersion(t, r), "volatile values are flushed every statusFlushInterval")
}

func TestReconcile_MeaningfulChangesAreWrittenImmediately(t *testing.T) {
	r, now := busyWorkloadReconciler(t)
	first := reconcileAndVersion(t, r)

	r.metrics = &stubMetrics{err: assert.AnError}
	*now = now.Add(time.Minute)

	assert.NotEqual(t, first, reconcileAndVersion(t, r), "a new condition must not wait for the flush interval")
}

// Activity seen on a check whose status wasn't written must still count:
// otherwise a workload that was busy a minute ago could pause early.
func TestReconcile_UnwrittenActivityIsRemembered(t *testing.T) {
	r, now := busyWorkloadReconciler(t)
	reconcileAndVersion(t, r)

	*now = now.Add(time.Minute)
	busySince := *now
	reconcileAndVersion(t, r)

	r.metrics = &stubMetrics{cpuMillis: 5, cpuPerReplica: 1000, replicas: 1}
	*now = now.Add(time.Minute)
	reconcileAndVersion(t, r)

	*now = now.Add(statusFlushInterval)
	reconcileAndVersion(t, r)
	activity := getWorkload(t, r, "busy-app").Status.Activity
	require.NotNil(t, activity)
	assert.True(t, activity.LastActivityTime.Equal(&metav1.Time{Time: busySince}),
		"last activity %s, want the unwritten busy check at %s", activity.LastActivityTime, busySince)
}

// Status is written every statusFlushInterval, so after a restart the last
// written evaluation is normally a few minutes old. That's not an outage.
func TestActivityClock_FlushLagIsNotAnOutage(t *testing.T) {
	target := clockTarget("app:v1", nil)
	workload := clockWorkload(fixedTime.Add(-59*time.Minute), target)
	evaluated := metav1.NewTime(fixedTime.Add(-statusFlushInterval))
	workload.Status.Activity.LastEvaluatedTime = &evaluated

	runClock(t, workload, target, clockOpts{metrics: idleCPU})

	assert.NotEqual(t, v1alpha1.ActivitySourceUnobserved, workload.Status.Activity.LastActivitySource)
}

func TestWithoutVolatile(t *testing.T) {
	evaluated := metav1.NewTime(fixedTime)
	status := &v1alpha1.ManagedWorkloadStatus{
		Phase: v1alpha1.PhaseRunning,
		Cost:  &v1alpha1.CostStatus{EstimatedMonthlyCost: "$1.00"},
		Activity: &v1alpha1.ActivityStatus{
			LastActivityTime:  metav1.NewTime(fixedTime),
			LastEvaluatedTime: &evaluated,
			TemplateHash:      "abc",
		},
	}

	stripped := withoutVolatile(status)

	assert.Nil(t, stripped.Cost)
	assert.Equal(t, "abc", stripped.Activity.TemplateHash, "a new template is a deploy and must be written promptly")
	assert.Nil(t, stripped.Activity.LastEvaluatedTime)
	assert.Equal(t, v1alpha1.PhaseRunning, stripped.Phase)
	assert.NotNil(t, status.Cost, "the original must not be modified")
}
