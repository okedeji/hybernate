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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/forecast"
)

const gibibyte = 1 << 30

// Two replicas of 1 vCPU and 1 GiB: $0.07 an hour at the default rates.
var twoReplicas = stubMetrics{cpuPerReplica: 1000, memoryPerReplica: gibibyte, replicas: 2}

func measuring(since time.Time) *v1alpha1.DryRunStatus {
	return &v1alpha1.DryRunStatus{Since: metav1.NewTime(since), EstimatedSavings: "$0.00"}
}

func runDryRunClock(t *testing.T, workload *v1alpha1.ManagedWorkload, metrics stubMetrics) []string {
	t.Helper()
	target := clockTarget("app:v1", nil)
	workload.Status.Activity.TemplateHash = podTemplateHash(target)
	pauser := &stubPauser{pauseDone: true}
	r := newAutomationReconciler(t, workload, &stubForecaster{phase: forecast.Observing},
		automationOpts{metrics: &metrics, pauser: pauser})
	_, err := r.reconcileAutomation(context.Background(), workload, target)
	require.NoError(t, err)
	require.Zero(t, pauser.pauseCalls, "dry-run never pauses")
	return recorded(r.Recorder.(*events.FakeRecorder), ReasonActivityResumed)
}

func TestDryRun_CountsAWouldBePause(t *testing.T) {
	workload := clockWorkload(fixedTime.Add(-61*time.Minute), clockTarget("app:v1", nil))
	workload.Spec.DryRun = true
	workload.Status.DryRun = measuring(fixedTime.Add(-24 * time.Hour))
	metrics := twoReplicas
	metrics.cpuMillis = 5

	runDryRunClock(t, workload, metrics)

	assert.Equal(t, v1alpha1.PhaseIdle, workload.Status.Phase)
	require.NotNil(t, workload.Status.DryRun)
	assert.Equal(t, int32(1), workload.Status.DryRun.Pauses)
	require.NotNil(t, workload.Status.DryRun.Resources, "what the pause would free is recorded as it begins")
	assert.Equal(t, int32(2), workload.Status.DryRun.Resources.Replicas)
}

func TestDryRun_ActivityEndsAWouldBePause(t *testing.T) {
	workload := clockWorkload(fixedTime, clockTarget("app:v1", nil))
	workload.Spec.DryRun = true
	workload.Status.Phase = v1alpha1.PhaseIdle
	idleSince := metav1.NewTime(fixedTime.Add(-3 * time.Hour))
	workload.Status.LastTransitionTime = &idleSince
	workload.Status.DryRun = &v1alpha1.DryRunStatus{
		Since:            metav1.NewTime(time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)),
		Pauses:           2,
		Slept:            metav1.Duration{Duration: time.Hour},
		FreedCPUHours:    resource.MustParse("2"),
		FreedMemoryHours: resource.MustParse("2"),
		EstimatedSavings: "$0.07",
		Resources:        &v1alpha1.ResourceSnapshot{Replicas: 2, CPUMillis: 1000, MemoryBytes: gibibyte},
	}
	metrics := twoReplicas
	metrics.cpuMillis = 1500

	resumed := runDryRunClock(t, workload, metrics)

	assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
	d := workload.Status.DryRun
	assert.Equal(t, int32(2), d.Pauses)
	assert.Equal(t, 4*time.Hour, d.Slept.Duration, "asleep from going Idle until the activity that would have woken it")
	assert.Equal(t, "$0.28", d.EstimatedSavings, "3h of 2 vCPU and 2 GiB added to $0.07")
	assert.Nil(t, d.Resources)
	require.Len(t, resumed, 1)
	assert.Contains(t, resumed[0], "[dry-run]")
	assert.Contains(t, resumed[0], "would have slept 3h, freeing $0.21")
	assert.Contains(t, resumed[0], "since Mar 10: would have paused 2 times, slept 4h, freeing $0.28")
}

// A workload already Idle when measuring started, such as across an operator
// upgrade, didn't have its would-be pause counted as it began, so its end
// isn't counted either.
func TestDryRun_APauseThatBeganBeforeMeasuringIsNotCounted(t *testing.T) {
	workload := clockWorkload(fixedTime, clockTarget("app:v1", nil))
	workload.Spec.DryRun = true
	workload.Status.Phase = v1alpha1.PhaseIdle
	idleSince := metav1.NewTime(fixedTime.Add(-3 * time.Hour))
	workload.Status.LastTransitionTime = &idleSince
	workload.Status.DryRun = measuring(fixedTime.Add(-time.Minute))
	metrics := twoReplicas
	metrics.cpuMillis = 1500

	resumed := runDryRunClock(t, workload, metrics)

	assert.Equal(t, v1alpha1.PhaseRunning, workload.Status.Phase)
	assert.Zero(t, workload.Status.DryRun.Slept.Duration)
	require.Len(t, resumed, 1)
	assert.NotContains(t, resumed[0], "would have slept")
}

// Would-be pauses add up to what they would have freed however short they
// are and however small the workload: each one used to be rounded to the
// cent as it was added, so a small workload never seemed worth pausing.
func TestDryRun_ShortPausesOfSmallWorkloadsAddUp(t *testing.T) {
	r := &Reconciler{clock: func() time.Time { return fixedTime }}
	workload := automationWorkload(v1alpha1.PhaseIdle)
	workload.Spec.DryRun = true
	workload.Status.DryRun = measuring(fixedTime.Add(-24 * time.Hour))
	idleSince := metav1.NewTime(fixedTime)
	workload.Status.LastTransitionTime = &idleSince
	workload.Status.Activity = &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(fixedTime.Add(3 * time.Minute))}

	for range 480 {
		workload.Status.DryRun.Pauses++
		workload.Status.DryRun.Resources = &v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: 10, MemoryBytes: 64 << 20}
		_, _, measured := r.endWouldBePause(workload)
		require.True(t, measured)
	}

	d := workload.Status.DryRun
	assert.Equal(t, 24*time.Hour, d.Slept.Duration)
	assert.InEpsilon(t, 0.01*24, d.FreedCPUHours.AsApproximateFloat64(), 0.001)
	assert.InEpsilon(t, 0.0625*24, d.FreedMemoryHours.AsApproximateFloat64(), 0.001)
	assert.Equal(t, cost.FormatDollars(cost.ComputeHourly(0.01, 0.0625, cost.DefaultRates)*24), d.EstimatedSavings)
}

func TestTrackDryRun(t *testing.T) {
	r := &Reconciler{clock: func() time.Time { return fixedTime }}
	workload := automationWorkload(v1alpha1.PhaseRunning)

	workload.Spec.DryRun = true
	r.trackDryRun(workload)
	require.NotNil(t, workload.Status.DryRun, "measuring starts with dry-run")
	assert.True(t, workload.Status.DryRun.Since.Time.Equal(fixedTime))

	workload.Status.DryRun.Pauses = 3
	r.trackDryRun(workload)
	assert.Equal(t, int32(3), workload.Status.DryRun.Pauses, "and carries on while it lasts")

	workload.Spec.DryRun = false
	r.trackDryRun(workload)
	assert.Nil(t, workload.Status.DryRun, "and is dropped when dry-run ends")
}

func TestRoundedDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Second, "under a minute"},
		{30 * time.Second, "under a minute"},
		{90 * time.Second, "2m"},
		{45 * time.Minute, "45m"},
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{3*time.Hour + 10*time.Minute, "3h10m"},
		{96 * time.Hour, "96h"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, roundedDuration(tt.in), tt.in.String())
	}
}
