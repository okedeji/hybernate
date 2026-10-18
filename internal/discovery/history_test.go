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

package discovery

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// longDeployment is a name long enough that the API server cuts its pods' names short.
const longDeployment = "payments-reconciliation-worker-eu-west-primary-blue"

var weekStart = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC) // a Monday

// week is a replay of seven days at five-minute steps, of a workload with
// two pods each requesting 1 vCPU and costing $0.05 an hour, whose CPU at
// each step is cores(t).
func week(cores func(t time.Time) float64) replay {
	r := replay{
		start: weekStart, end: weekStart.Add(7*24*time.Hour - minStep), step: minStep,
		usedCores: map[int64]float64{}, pods: map[int64]int{},
		requestCores: 1, podHourly: 0.05, threshold: 10, idleAfter: time.Hour,
	}
	for t := r.start; !t.After(r.end); t = t.Add(r.step) {
		r.pods[t.Unix()] = 2
		r.usedCores[t.Unix()] = cores(t)
	}
	return r
}

func officeHours(t time.Time) float64 {
	if t.Weekday() >= time.Monday && t.Weekday() <= time.Friday && t.Hour() >= 9 && t.Hour() < 17 {
		return 1.2 // 60% of two pods' requests
	}
	return 0.02
}

func TestReplay(t *testing.T) {
	tests := []struct {
		name       string
		replay     replay
		wantSleep  float64
		wantQuiet  float64
		wantWakes  int
		wantFreed  float64
		wantRuning float64
	}{
		{
			name:       "busy all week never sleeps",
			replay:     week(func(time.Time) float64 { return 1 }),
			wantRuning: 168,
		},
		{
			name:   "idle all week sleeps after the first idleAfter",
			replay: week(func(time.Time) float64 { return 0.01 }),
			// Asleep from the step an hour after the start to the end.
			wantSleep: 168 - 1, wantQuiet: 168, wantRuning: 168,
			wantFreed: (168 - 1) * 2 * 0.05,
		},
		{
			name:   "busy in office hours sleeps nights and the weekend",
			replay: week(officeHours),
			// Five 8-hour days are active. The clock runs out an hour after
			// the last busy step, at 16:55, so 55 minutes after each day are
			// awake. The rest, less the first hour, is asleep.
			wantSleep: 168 - 5*8 - 5*55.0/60 - 1, wantQuiet: 168 - 5*8, wantRuning: 168, wantWakes: 5,
			wantFreed: (168 - 5*8 - 5*55.0/60 - 1) * 2 * 0.05,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.replay.run()

			assert.InDelta(t, 168, h.Hours, 0.01)
			assert.InDelta(t, tt.wantRuning, h.RunningHours, 0.01)
			assert.InDelta(t, tt.wantQuiet, h.QuietHours, 0.01)
			assert.InDelta(t, tt.wantSleep, h.SleepHours, 0.01)
			assert.Equal(t, tt.wantWakes, h.Wakes)
			assert.InDelta(t, tt.wantFreed, h.Freed, 0.01)
			assert.InDelta(t, tt.wantFreed/168*hoursPerMonth, h.MonthlyFreed, 0.01)
			assert.InDelta(t, tt.wantRuning*2*0.05, h.Cost, 0.01, "both pods while they ran")
			assert.InDelta(t, h.Cost/168*hoursPerMonth, h.MonthlyCost, 0.01)
		})
	}
}

func TestReplay_RolloutIsActivity(t *testing.T) {
	r := week(func(time.Time) float64 { return 0 })
	r.rollouts = []time.Time{weekStart.Add(72*time.Hour + 2*time.Minute)}

	h := r.run()

	assert.Equal(t, 1, h.Wakes, "a deploy wakes it like any activity")
	assert.InDelta(t, 168-1-1, h.SleepHours, 0.01, "and keeps it awake for idleAfter")
}

// Steps without pods, such as while something else had it scaled to zero,
// free nothing and aren't counted as running.
func TestReplay_NoPodsFreesNothing(t *testing.T) {
	r := week(func(time.Time) float64 { return 0 })
	for at := range r.pods {
		if time.Unix(at, 0).After(weekStart.Add(84 * time.Hour)) {
			delete(r.pods, at)
		}
	}

	h := r.run()

	assert.InDelta(t, 84, h.RunningHours, 0.1)
	assert.InDelta(t, 84-1, h.SleepHours, 0.1)
	assert.InDelta(t, (84-1)*2*0.05, h.Freed, 0.01)
}

// CPU is measured against requests for the pods running at each step, so a
// workload an autoscaler has scaled out isn't judged idle for spreading its
// load, and what pausing frees follows the pods it ran.
func TestReplay_FollowsReplicas(t *testing.T) {
	r := week(func(time.Time) float64 { return 0.5 })
	for at := range r.pods {
		r.pods[at] = 4
	}

	h := r.run()

	assert.Zero(t, h.SleepHours, "0.5 cores across 4 pods is 12.5%, above the threshold")

	r.threshold = 20
	h = r.run()
	assert.InDelta(t, (168-1)*4*0.05, h.Freed, 0.01, "priced on the 4 pods it ran")
}

func TestWorkloadSeries(t *testing.T) {
	history := []containerCPU{
		{pod: "api-7d9f8c6b5-abcde", container: "app", samples: map[int64]float64{0: 0.3, 300: 0.1}},
		{pod: "api-7d9f8c6b5-abcde", container: "istio-proxy", samples: map[int64]float64{0: 0.5, 300: 0.5}},
		{pod: "api-7d9f8c6b5-fghij", container: "app", samples: map[int64]float64{0: 0.2}},
		{pod: "api-gateway-5c6d7-klmno", container: "app", samples: map[int64]float64{0: 9}},
	}

	used, pods, found := workloadSeries(history, replicaSetPods([]string{"api-7d9f8c6b5"}), []string{"app"})

	assert.True(t, found)
	assert.InDelta(t, 0.5, used[0], 0.001, "its own containers in both pods; not the sidecar, not api-gateway")
	assert.InDelta(t, 0.1, used[300], 0.001)
	assert.Equal(t, map[int64]int{0: 2, 300: 1}, pods)
}

func TestReplicaSetPods(t *testing.T) {
	long := longDeployment
	longRS := long + "-7d9f8c6b5"
	tests := []struct {
		name        string
		replicaSets []string
		pod         string
		want        bool
	}{
		{"its ReplicaSet's pod", []string{"api-7d9f8c6b5"}, "api-7d9f8c6b5-abcde", true},
		{"an older ReplicaSet's pod", []string{"api-7d9f8c6b5", "api-65bd9"}, "api-65bd9-x2k4p", true},
		{"a longer-named Deployment's pod", []string{"api-7d9f8c6b5"}, "api-gateway-7d9f8c6b5-abcde", false},
		{"a Job's pod", []string{"api-7d9f8c6b5"}, "api-migrate-x7k2p", false},
		{"a CronJob's pod", []string{"api-7d9f8c6b5"}, "api-29312345-x7k2p", false},
		{"a DaemonSet's pod", []string{"api-7d9f8c6b5"}, "api-agent-x7k2p", false},
		{"a StatefulSet's pod", []string{"api-7d9f8c6b5"}, "api-0", false},
		{"a ReplicaSet it doesn't own", []string{"api-7d9f8c6b5"}, "api-5c6d7f8b9-abcde", false},
		{"a long name cut through its hash", []string{longRS}, (longRS + "-")[:58] + "x7k2p", true},
		{"no ReplicaSets", nil, "api-7d9f8c6b5-abcde", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replicaSetPods(tt.replicaSets)(tt.pod))
		})
	}
}

func TestStatefulSetPods(t *testing.T) {
	db := statefulSetPods("db")
	assert.True(t, db("db-0"))
	assert.True(t, db("db-12"))
	assert.False(t, db("db-replica-0"))
	assert.False(t, db("db-7d9f8c6b5-abcde"))
	assert.False(t, db("db-01"), "ordinals have no leading zero")
	assert.False(t, db("db-"))
}

func TestHistoryStep(t *testing.T) {
	assert.Equal(t, 5*time.Minute, historyStep(7*24*time.Hour))
	assert.Equal(t, 5*time.Minute, historyStep(30*24*time.Hour))
	assert.Equal(t, 13*time.Minute, historyStep(90*24*time.Hour), "coarser to stay under Prometheus's limit")
}

func TestRolledOutBetween(t *testing.T) {
	at := weekStart.Add(time.Hour)
	rollouts := []time.Time{weekStart, at}

	assert.True(t, rolledOutBetween(rollouts, at.Add(-minStep), at), "the end of the step is in it")
	assert.False(t, rolledOutBetween(rollouts, at, at.Add(minStep)), "the start isn't")
	assert.False(t, rolledOutBetween(nil, weekStart, at))
}

func TestScanCluster_ReplaysHistory(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.002 }
	busy := func(time.Time) float64 { return 0.08 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "idle-api-7d9f8c6b5-abcde", container: "main", cores: quiet},
		{pod: "idle-api-7d9f8c6b5-fghij", container: "main", cores: quiet},
		{pod: "busy-api-7d9f8c6b5-klmno", container: "main", cores: busy},
		{pod: "managed-api-7d9f8c6b5-pqrst", container: "main", cores: quiet},
	}}}
	objs := deploymentWithRollout("idle-api", testNamespace, 2, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("idle-api", testNamespace, "1m", "20Mi"))
	objs = append(objs, deploymentWithRollout("busy-api", testNamespace, 1, 30*24*time.Hour)...)
	objs = append(objs, makePodMetrics("busy-api", testNamespace, "80m", "20Mi"))
	objs = append(objs, deploymentWithRollout("new-api", testNamespace, 1, 30*24*time.Hour)...)
	objs = append(objs, makePodMetrics("new-api", testNamespace, "1m", "20Mi"))
	objs = append(objs, deploymentWithRollout("managed-api", testNamespace, 1, 30*24*time.Hour)...)
	objs = append(objs, makePodMetrics("managed-api", testNamespace, "1m", "20Mi"),
		managedFor("managed-api", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning}))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Equal(t, ModeHistory, report.Mode)
	require.NotNil(t, report.History)
	assert.InDelta(t, 168, report.History.Hours, 0.1)
	got := byName(report)

	idle := got["idle-api"].History
	require.NotNil(t, idle)
	assert.InDelta(t, 168-1, idle.SleepHours, 0.1)
	assert.Zero(t, idle.Wakes)
	assert.InDelta(t, got["idle-api"].HourlyCost*(168-1), idle.Freed, 0.01, "both pods, priced as the scan prices them")

	busyHistory := got["busy-api"].History
	require.NotNil(t, busyHistory)
	assert.Zero(t, busyHistory.SleepHours, "80m of 100m is above the threshold all week")

	assert.Nil(t, got["new-api"].History, "no history recorded for it")
	assert.Contains(t, strings.Join(report.Notes, "\n"), "1 workload has no CPU history in Prometheus")
	assert.Contains(t, strings.Join(report.Notes, "\n"), "sees CPU and rollouts only")

	assert.NotNil(t, got["managed-api"].History, "managed workloads are replayed too")
	assert.Equal(t, 2, report.Totals.Replayed.Workloads, "but what Hybernate already manages isn't counted again")
	assert.Equal(t, 1, report.Totals.Replayed.Sleepers)
	assert.InDelta(t, idle.Freed, report.Totals.Replayed.Freed, 0.001)
	assert.Equal(t, "idle-api", report.Workloads[0].Name, "most freed first")
}

// A workload that ran four pods all week, and runs one now, would have
// freed four pods' worth: more than it costs now. What it could save is a
// share of what it cost over the same history, never more than all of it.
func TestScanCluster_SavingsAreAShareOfTheirOwnCost(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.001 }
	suffixes := []string{"abcde", "fghij", "klmno", "pqrst"}
	history := make([]series, 0, len(suffixes))
	for _, suffix := range suffixes {
		history = append(history, series{pod: "idle-api-7d9f8c6b5-" + suffix, container: "main", cores: quiet})
	}
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: history}}
	objs := deploymentWithRollout("idle-api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("idle-api", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	w := byName(report)["idle-api"]
	require.NotNil(t, w.History)
	assert.InDelta(t, 4*w.HourlyCost*168, w.History.Cost, 0.01, "four pods all week, at today's prices")
	assert.Greater(t, w.History.MonthlyFreed, w.MonthlyCost, "four pods freed is more than one costs")
	totals := report.Totals
	assert.InDelta(t, w.History.MonthlyCost, totals.SavingsBasis, 0.001)
	share := totals.Replayed.MonthlyFreed / totals.SavingsBasis
	assert.InDelta(t, (168-1)/168.0, share, 0.001, "asleep all but its first hour, of the pods it ran")
}

func TestScanCluster_SaysWhenPrometheusKeepsLessHistory(t *testing.T) {
	f := &fakePrometheus{from: scanTime.Add(-50 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "idle-api-7d9f8c6b5-abcde", container: "main", cores: func(time.Time) float64 { return 0 }},
	}}}
	objs := deploymentWithRollout("idle-api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("idle-api", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.InDelta(t, 50, report.History.Hours, 0.1)
	assert.Contains(t, strings.Join(report.Notes, "\n"), "Prometheus keeps 2 days of history, so the replay covers that, not the 7 days asked for")
	h := byName(report)["idle-api"].History
	require.NotNil(t, h)
	assert.InDelta(t, h.Freed/h.Hours*hoursPerMonth, h.MonthlyFreed, 0.001, "a month at the rate it covered")
}

// Workloads Hybernate manages are replayed with their own idleAfter;
// others with the scan's.
func TestScanCluster_ReplaysEachWithItsIdleAfter(t *testing.T) {
	quiet := func(time.Time) float64 { return 0 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "plain-7d9f8c6b5-abcde", container: "main", cores: quiet},
		{pod: "managed-7d9f8c6b5-abcde", container: "main", cores: quiet},
	}}}
	objs := deploymentWithRollout("plain", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("plain", testNamespace, "1m", "20Mi"))
	objs = append(objs, deploymentWithRollout("managed", testNamespace, 1, 30*24*time.Hour)...)
	mw := managedFor("managed", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning})
	mw.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{IdleAfter: &metav1.Duration{Duration: 3 * time.Hour}}
	objs = append(objs, makePodMetrics("managed", testNamespace, "1m", "20Mi"), mw)
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, 2*time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	got := byName(report)
	require.NotNil(t, got["plain"].History)
	require.NotNil(t, got["managed"].History)
	assert.InDelta(t, 168-2, got["plain"].History.SleepHours, 0.1, "the scan's --idle-after")
	assert.InDelta(t, 168-3, got["managed"].History.SleepHours, 0.1, "its own idle-after")
}

// A managed workload's own CPU threshold decides whether its CPU is
// activity, now and in the replay; others use the scan's.
func TestScanCluster_EachWithItsCPUThreshold(t *testing.T) {
	busyish := func(time.Time) float64 { return 0.02 } // 20% of 100m
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "plain-7d9f8c6b5-abcde", container: "main", cores: busyish},
		{pod: "managed-7d9f8c6b5-abcde", container: "main", cores: busyish},
	}}}
	objs := deploymentWithRollout("plain", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("plain", testNamespace, "20m", "20Mi"))
	objs = append(objs, deploymentWithRollout("managed", testNamespace, 1, 30*24*time.Hour)...)
	mw := managedFor("managed", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning,
		Activity: &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(scanTime.Add(-5 * time.Hour)),
			LastActivitySource: v1alpha1.ActivitySourceCPU}})
	mw.Spec.IdlePolicy = &v1alpha1.IdlePolicySpec{Activity: &v1alpha1.ActivitySpec{CPUThreshold: 50}}
	objs = append(objs, makePodMetrics("managed", testNamespace, "20m", "20Mi"), mw)
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	got := byName(report)
	assert.Equal(t, StateActive, got["plain"].State, "20% is above the scan's 10%")
	assert.Equal(t, StateIdle, got["managed"].State, "but under its own 50%")
	require.NotNil(t, got["plain"].History)
	require.NotNil(t, got["managed"].History)
	assert.Zero(t, got["plain"].History.SleepHours)
	assert.InDelta(t, 168-1, got["managed"].History.SleepHours, 0.1)
}

// A Job or another Deployment whose pods start with the workload's name
// isn't the workload, and isn't replayed as its activity.
func TestScanCluster_HistoryIsTheWorkloadsOwn(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.001 }
	busy := func(time.Time) float64 { return 2 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "web-7d9f8c6b5-abcde", container: "main", cores: quiet},
		{pod: "web-migrate-x7k2p", container: "main", cores: busy},
		{pod: "web-29312345-x7k2p", container: "main", cores: busy},
		{pod: "web-api-7d9f8c6b5-fghij", container: "main", cores: busy},
	}}}
	objs := deploymentWithRollout("web", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("web", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	h := byName(report)["web"].History
	require.NotNil(t, h)
	assert.InDelta(t, 167, h.SleepHours, 1, "web was quiet all week; the migration Job and web-api aren't web")
}

// Without metrics-server, CPU right now can't be measured, but history can
// still judge a workload, so the table lists the workloads the headline
// counts.
func TestScanCluster_JudgesFromHistoryWithoutMetrics(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.001 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "idle-api-7d9f8c6b5-x7k2p", container: "main", cores: quiet},
		{pod: "busy-api-7d9f8c6b5-x7k2p", container: "main", cores: func(time.Time) float64 { return 2 }},
	}}}
	objs := deploymentWithRollout("idle-api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, deploymentWithRollout("busy-api", testNamespace, 1, 30*24*time.Hour)...)
	objs = append(objs, deploymentWithRollout("no-history", testNamespace, 1, 30*24*time.Hour)...)
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := failingIn(objs, func(list client.ObjectList, _ string) error {
		if _, ok := list.(*metricsv1beta1.PodMetricsList); ok {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "metrics.k8s.io", Kind: "PodMetrics"}}
		}
		return nil
	})

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	got := byName(report)
	assert.Equal(t, StateIdle, got["idle-api"].State)
	assert.Equal(t, "no activity in its CPU history", got["idle-api"].Reason)
	assert.Equal(t, StateActive, got["busy-api"].State)
	assert.Equal(t, StateUnknown, got["no-history"].State, "nothing to judge it by")
	assert.Equal(t, 2, report.Totals.Replayed.Workloads, "only the workloads the table lists")
	assert.InDelta(t, got["idle-api"].History.MonthlyCost+got["busy-api"].History.MonthlyCost,
		report.Totals.SavingsBasis, 0.001, "the one that couldn't be judged isn't in the share's basis")
	assert.Zero(t, CouldSave(Workload{State: StateUnknown, History: &History{MonthlyFreed: 5}}))
}

// A Prometheus too slow to answer in the scan's time costs the replay of
// the namespaces it didn't answer for, not the scan: they're judged from CPU
// right now, the notes say Prometheus was slow, and there's time left for
// the rest.
func TestScanCluster_SlowPrometheus(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	prom, err := NewPrometheusURL(slow.URL, slow.Client(), nil)
	require.NoError(t, err)
	objs := deploymentWithRollout("api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("api", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = prom, 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	report, err := NewScanner(c, c).ScanCluster(ctx, opts)

	require.NoError(t, err)
	require.NoError(t, ctx.Err(), "the scan finished within its own deadline")
	w := byName(report)["api"]
	assert.Equal(t, StateIdle, w.State, "judged from CPU right now")
	assert.Nil(t, w.History)
	assert.Contains(t, strings.Join(report.Notes, "\n"), "the scan ran out of the time it gives CPU history, "+
		"shared by reading the API server and querying Prometheus, before it had read the history of 1 namespace, "+
		"so workloads there are judged from CPU right now: "+testNamespace)
	assert.Contains(t, strings.Join(report.Notes, "\n"), "--window 0")
	assert.Nil(t, report.Incomplete, "everything the cluster holds was read")
}

// When reading the API server spends the time the scan gives history,
// Prometheus isn't queried at all, so it's neither waited on nor blamed.
func TestScanCluster_SlowAPIServerSpendsTheHistoryBudget(t *testing.T) {
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour)}
	objs := deploymentWithRollout("api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("api", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ServiceList); ok {
					deadline, _ := ctx.Deadline()
					time.Sleep(time.Until(deadline) - 300*time.Millisecond)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	report, err := NewScanner(c, c).ScanCluster(ctx, opts)

	require.NoError(t, err)
	assert.Empty(t, f.queries, "history wasn't queried after its time was spent")
	assert.Nil(t, byName(report)["api"].History)
	notes := strings.Join(report.Notes, "\n")
	assert.Contains(t, notes, "the scan ran out of the time it gives CPU history")
	assert.NotContains(t, notes, "Prometheus didn't")
}

// A history query gets the time the scan gives history, not a fixed limit,
// so a Prometheus slower than that limit still answers within a longer
// --timeout.
func TestPrometheus_HistoryQueryGetsTheTimeHistoryHas(t *testing.T) {
	var deadline time.Time
	p := &Prometheus{Source: "thanos", get: func(ctx context.Context, _ string, _ url.Values) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	want, _ := ctx.Deadline()

	_, err := p.namespaceCPU(ctx, testNamespace, scanTime.Add(-time.Hour), scanTime, minStep)

	require.NoError(t, err)
	assert.Equal(t, want, deadline)
}

// When Prometheus, or the API server's proxy in front of it, gives up on a
// query on its own with time left, it's the query that was too big: the
// notes say so, and don't blame --timeout, which more of wouldn't help.
func TestScanCluster_PrometheusGivesUpOnItsOwn(t *testing.T) {
	prom := &Prometheus{Source: "monitoring/prometheus", get: func(context.Context, string, url.Values) ([]byte, error) {
		return nil, apierrors.NewTimeoutError("the proxy timed out", 0)
	}}
	objs := deploymentWithRollout("api", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics("api", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = prom, 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	report, err := NewScanner(c, c).ScanCluster(ctx, opts)

	require.NoError(t, err)
	assert.Equal(t, StateIdle, byName(report)["api"].State, "judged from CPU right now")
	notes := strings.Join(report.Notes, "\n")
	assert.Contains(t, notes, "gave up on the CPU history of 1 namespace after up to 0s, with time left, so "+
		"workloads there are judged from CPU right now: "+testNamespace+". A shorter --window asks it for less")
	assert.NotContains(t, notes, "--timeout")
	assert.Nil(t, report.Incomplete)
}

func TestJudgeFromHistory(t *testing.T) {
	tests := []struct {
		name    string
		history History
		want    State
		wantWhy string
	}{
		{"idle since its last activity", History{asleepAtEnd: true, lastActive: scanTime.Add(-50 * time.Hour)},
			StateIdle, "last active in its CPU history 2 days ago"},
		{"never active", History{asleepAtEnd: true}, StateIdle, "no activity in its CPU history"},
		{"active lately", History{lastActive: scanTime.Add(-20 * time.Minute)},
			StateActive, "active in its CPU history 20m ago"},
		{"too new to be idle", History{}, StateActive, "started too recently in its CPU history to be idle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := Workload{State: StateUnknown, History: &tt.history}
			judgeFromHistory(&w, scanTime)
			assert.Equal(t, tt.want, w.State)
			assert.Equal(t, tt.wantWhy, w.Reason)
		})
	}
}

// A Deployment's pods from a ReplicaSet since deleted, past its revision
// history limit, are still its history; a bare ReplicaSet's that still
// exists aren't.
func TestScanCluster_HistoryOfDeletedReplicaSets(t *testing.T) {
	busy := func(time.Time) float64 { return 2 }
	quiet := func(time.Time) float64 { return 0.001 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "web-7d9f8c6b5-x7k2p", container: "main", cores: quiet},
		{pod: "web-5c6d7f8b9-q2w4r", container: "main", cores: func(t time.Time) float64 {
			if t.Before(scanTime.Add(-3 * 24 * time.Hour)) {
				return 2
			}
			return 0
		}},
		{pod: "web-6b8c9d4f5-z9x8c", container: "main", cores: busy},
	}}}
	bare := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-6b8c9d4f5", Namespace: testNamespace}}
	objs := deploymentWithRollout("web", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, bare, makePodMetrics("web", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	h := byName(report)["web"].History
	require.NotNil(t, h)
	assert.InDelta(t, 72-1, h.SleepHours, 1, "busy for its first four days, on a ReplicaSet since deleted")
}

func TestDeploymentReplicaSet(t *testing.T) {
	long := longDeployment
	tests := []struct {
		name, deployment, pod, want string
	}{
		{"a Deployment's pod", "api", "api-7d9f8c6b5-x7k2p", "api-7d9f8c6b5"},
		{"a short hash", "api", "api-65bd9-x2k4p", "api-65bd9"},
		{"a hash of only digits", "api", "api-5967487594-x2k4p", "api-5967487594"},
		{"a Job's pod in letters a hash has", "api", "api-bckp-x7k2p", ""},
		{"a hash Kubernetes doesn't write", "api", "api-7d9f8c6z5-x7k2p", ""},
		{"a longer-named Deployment's pod", "api", "api-gateway-7d9f8c6b5-x7k2p", ""},
		{"a Job's pod", "api", "api-migrate-x7k2p", ""},
		{"a CronJob's pod", "api", "api-29312345-x7k2p", ""},
		{"a timestamp in generated digits", "api", "api-29245678-x7k2p", ""},
		{"a DaemonSet's pod", "api", "api-x7k2p", ""},
		{"a StatefulSet's pod", "api", "api-0", ""},
		{"a suffix Kubernetes doesn't generate", "api", "api-7d9f8c6b5-abcde", ""},
		{"a hash too long", "api", "api-7d9f8c6b5bcd-x7k2p", ""},
		{"another Deployment's pod", "api", "web-7d9f8c6b5-x7k2p", ""},
		{"a name cut short", long, (long + "-7d9f8c6b5-")[:58] + "x7k2p", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs, ok := deploymentReplicaSet(tt.deployment, tt.pod)
			assert.Equal(t, tt.want, rs)
			assert.Equal(t, tt.want != "", ok)
		})
	}
}

// A Deployment's name long enough that the API server cuts its pods' names
// short still has its history found.
func TestScanCluster_HistoryOfALongName(t *testing.T) {
	name := longDeployment
	pod := (name + "-7d9f8c6b5-")[:58] + "x7k2p"
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: pod, container: "main", cores: func(time.Time) float64 { return 0.001 }},
	}}}
	objs := deploymentWithRollout(name, testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, makePodMetrics(name, testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	require.NotNil(t, byName(report)[name].History, pod)
	assert.NotContains(t, strings.Join(report.Notes, "\n"), "no CPU history")
}

// A workload already at zero by hand frees nothing more when paused, so it
// isn't replayed or counted toward what pausing could save.
func TestScanCluster_ScaledByHandCouldSaveNothing(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.001 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "old-api-7d9f8c6b5-abcde", container: "main", cores: quiet},
	}}}
	objs := deploymentWithRollout("old-api", testNamespace, 0, 30*24*time.Hour)
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	w := byName(report)["old-api"]
	assert.True(t, w.ScaledByHand)
	assert.Nil(t, w.History, "not replayed")
	assert.Zero(t, CouldSave(w))
	assert.Zero(t, report.Totals.Replayed.Workloads)
	assert.Zero(t, report.Totals.Replayed.MonthlyFreed)
	assert.Equal(t, 1, report.Totals.ScaledToZero)
	assert.NotContains(t, strings.Join(report.Notes, "\n"), "no CPU history")
	assert.Zero(t, CouldSave(Workload{ScaledByHand: true, History: &History{MonthlyFreed: 50}}))
}

// A workload created partway through the window is replayed from when it
// was created, so its monthly rate isn't diluted by hours before it existed.
// A workload deleted and created again with the same pod template, as a
// reinstall does, runs pods with the same names, so the week its earlier
// self ran is its history too.
func TestScanCluster_ReplaysAReinstalledWorkloadsWholeHistory(t *testing.T) {
	windowStart := scanTime.Add(-7 * 24 * time.Hour)
	f := &fakePrometheus{from: windowStart, byNamespace: map[string][]series{testNamespace: {
		{pod: "app-7d9f8c6b5-abcde", container: "main", cores: func(time.Time) float64 { return 0 }},
	}}}
	objs := deploymentWithRollout("app", testNamespace, 1, 6*24*time.Hour)
	objs[0].(*appsv1.Deployment).CreationTimestamp = metav1.NewTime(scanTime.Add(-time.Hour))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	h := byName(report)["app"].History
	require.NotNil(t, h)
	assert.InDelta(t, 7*24, h.Hours, 0.2, "replayed over the whole week, not the hour since it was created")
}

func TestScanCluster_ReplaysAYoungWorkloadFromItsFirstSample(t *testing.T) {
	windowStart := scanTime.Add(-7 * 24 * time.Hour)
	created := windowStart.Add(24 * time.Hour)
	f := &fakePrometheus{from: windowStart, byNamespace: map[string][]series{testNamespace: {
		{pod: "old-7d9f8c6b5-abcde", container: "main", cores: func(time.Time) float64 { return 0.5 }},
		{pod: "young-7d9f8c6b5-abcde", container: "main", cores: func(t time.Time) float64 {
			if t.Before(created) {
				return math.NaN()
			}
			return 0
		}},
	}}}
	objs := deploymentWithRollout("old", testNamespace, 1, 30*24*time.Hour)
	young := deploymentWithRollout("young", testNamespace, 1, 6*24*time.Hour)
	young[0].(*appsv1.Deployment).CreationTimestamp = metav1.NewTime(created)
	objs = append(objs, young...)
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	h := byName(report)["young"].History
	require.NotNil(t, h)
	assert.InDelta(t, 6*24, h.Hours, 0.2, "replayed from when it first ran")
	assert.InDelta(t, 6*24-1, h.SleepHours, 0.2, "asleep an hour after it started")
	hourly := byName(report)["young"].HourlyCost
	assert.InDelta(t, hourly*(6*24-1)/(6*24)*hoursPerMonth, h.MonthlyFreed, 0.01,
		"nearly all of a month: it slept all but its first hour")
}
