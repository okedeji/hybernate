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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

var scanTime = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func scanOptions(namespaces ...string) ClusterOptions {
	return ClusterOptions{Namespaces: namespaces, CPUThreshold: 10, Rates: cost.DefaultRates, Now: func() time.Time { return scanTime }}
}

// deploymentWithRollout is a Deployment whose newest ReplicaSet was created
// age ago.
func deploymentWithRollout(name, namespace string, replicas int32, age time.Duration) []runtime.Object {
	d := makeDeployment(name, namespace, replicas, "100m", "128Mi", nil)
	d.UID = types.UID(namespace + "-" + name)
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-abc", Namespace: namespace,
		CreationTimestamp: metav1.NewTime(scanTime.Add(-age)),
		OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: d.UID, Controller: ptr.To(true)}},
	}}
	return []runtime.Object{d, rs}
}

func scanWorkloads(t *testing.T, objs ...runtime.Object) *ClusterReport {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()
	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace, "preview-918"))
	require.NoError(t, err)
	return report
}

func byName(report *ClusterReport) map[string]Workload {
	out := map[string]Workload{}
	for _, w := range report.Workloads {
		out[w.Name] = w
	}
	return out
}

func TestScanCluster_JudgesEachWorkload(t *testing.T) {
	objs := deploymentWithRollout("idle-api", testNamespace, 2, time.Hour)
	objs = append(objs, makePodMetrics("idle-api", testNamespace, "5m", "20Mi"))
	objs = append(objs, deploymentWithRollout("busy-api", testNamespace, 1, time.Hour)...)
	objs = append(objs, makePodMetrics("busy-api", testNamespace, "80m", "100Mi"))
	objs = append(objs, deploymentWithRollout("starting", testNamespace, 1, 2*time.Hour)...)

	got := byName(scanWorkloads(t, objs...))

	assert.Equal(t, StateIdle, got["idle-api"].State, "5m of 2×100m is 2.5%")
	assert.Equal(t, 2, *got["idle-api"].CPUPercent)
	assert.Equal(t, StateActive, got["busy-api"].State)
	assert.Equal(t, StateUnknown, got["starting"].State, "no metrics yet isn't the same as idle")
	assert.Equal(t, "no metrics yet", got["starting"].Unmeasured)
	assert.Nil(t, got["starting"].CPUPercent)
}

func TestScanCluster_PricesWhatWorkloadsReserve(t *testing.T) {
	objs := deploymentWithRollout("idle-api", testNamespace, 2, time.Hour)
	objs = append(objs, makePodMetrics("idle-api", testNamespace, "5m", "20Mi"))
	objs = append(objs, deploymentWithRollout("busy-api", testNamespace, 1, time.Hour)...)
	objs = append(objs, makePodMetrics("busy-api", testNamespace, "80m", "100Mi"))

	report := scanWorkloads(t, objs...)
	got := byName(report)

	perReplica := 0.1*cost.DefaultRates.CPUPerHour + 0.125*cost.DefaultRates.MemoryPerHour
	assert.InDelta(t, 2*perReplica, got["idle-api"].HourlyCost, 0.0001)
	assert.InDelta(t, 2*perReplica*hoursPerMonth, got["idle-api"].MonthlyCost, 0.001)
	assert.Equal(t, "idle-api", report.Workloads[0].Name, "idle workloads first")

	assert.Equal(t, 2, report.Totals.Workloads)
	assert.Equal(t, 1, report.Totals.Idle)
	assert.Equal(t, int64(200), report.Totals.IdleCPUMillis, "what idle workloads reserve, all replicas")
	assert.Equal(t, int64(2*128<<20), report.Totals.IdleMemoryBytes)
	assert.InDelta(t, got["idle-api"].HourlyCost, report.Totals.IdleHourlyCost, 0.0001, "only idle workloads")
	assert.InDelta(t, got["idle-api"].MonthlyCost+got["busy-api"].MonthlyCost, report.Totals.MonthlyCost, 0.001)
}

func TestScanCluster_Clues(t *testing.T) {
	objs := deploymentWithRollout("old", testNamespace, 1, 23*24*time.Hour)
	objs = append(objs, deploymentWithRollout("fresh", testNamespace, 1, 24*time.Hour)...)
	objs = append(objs, deploymentWithRollout("web", "preview-918", 1, time.Hour)...)

	got := byName(scanWorkloads(t, objs...))

	assert.Equal(t, []string{"not deployed in 23 days"}, got["old"].Clues)
	assert.Empty(t, got["fresh"].Clues)
	assert.Empty(t, got["web"].Clues, "a namespace's name says nothing certain about its use")
	require.NotNil(t, got["old"].LastDeployed)
	assert.True(t, got["old"].LastDeployed.Equal(scanTime.Add(-23*24*time.Hour)))
}

func TestScanCluster_SaysWhyCPUWasntMeasured(t *testing.T) {
	noRequests := makeDeployment("sidecarless", testNamespace, 1, "0", "64Mi", nil)

	got := byName(scanWorkloads(t, noRequests, makePodMetrics("sidecarless", testNamespace, "5m", "20Mi")))

	assert.Equal(t, StateUnknown, got["sidecarless"].State)
	assert.Equal(t, "no CPU requests", got["sidecarless"].Unmeasured)
}

func TestScanCluster_SkipsWhatIsntInPlay(t *testing.T) {
	ignored := makeDeployment("ignored", testNamespace, 1, "100m", "128Mi", map[string]string{v1alpha1.LabelIgnore: "true"})
	report := scanWorkloads(t, makeDeployment("off", testNamespace, 0, "100m", "128Mi", nil), ignored)

	assert.Empty(t, report.Workloads)
	assert.Contains(t, report.Notes, "1 workload is already scaled to zero, not counted")
}

func TestScanCluster_MarksManagedWorkloads(t *testing.T) {
	mw := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: testNamespace},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}

	got := byName(scanWorkloads(t, makeDeployment("api", testNamespace, 1, "100m", "128Mi", nil), mw))

	assert.True(t, got["api"].Managed)
}

func TestScanCluster_WithoutTheMetricsAPI(t *testing.T) {
	noMetrics := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*metricsv1beta1.PodMetricsList); ok {
			return &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonNotFound, Code: 404}}
		}
		return c.List(ctx, list, opts...)
	}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).
		WithRuntimeObjects(makeDeployment("api", testNamespace, 1, "100m", "128Mi", nil), makePodMetrics("api", testNamespace, "5m", "20Mi")).
		WithInterceptorFuncs(noMetrics).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))

	require.NoError(t, err)
	assert.Equal(t, StateUnknown, report.Workloads[0].State)
	assert.Equal(t, "no Metrics API", report.Workloads[0].Unmeasured)
	assert.Contains(t, report.Notes[0], "metrics-server")
}

func TestNamespaces(t *testing.T) {
	ns := func(name string) runtime.Object { return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}} }
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(ns("sandbox-42"), ns("kube-system"), ns("api")).Build()

	got, err := Namespaces(context.Background(), c, nil, SystemNamespaces)
	require.NoError(t, err)
	assert.Equal(t, []string{"api", "sandbox-42"}, got)

	got, err = Namespaces(context.Background(), c, []string{"kube-system"}, SystemNamespaces)
	require.NoError(t, err)
	assert.Equal(t, []string{"kube-system"}, got, "named namespaces are scanned as asked")
}

func TestNamespaces_Forbidden(t *testing.T) {
	forbidden := interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("no"))
	}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(forbidden).Build()

	_, err := Namespaces(context.Background(), c, nil, nil)

	assert.ErrorIs(t, err, ErrCantListNamespaces)
}

// idleNow is a workload last rolled out a month ago, using almost no CPU.
func idleNow(name string) []runtime.Object {
	objs := deploymentWithRollout(name, testNamespace, 1, 30*24*time.Hour)
	return append(objs, makePodMetrics(name, testNamespace, "1m", "10Mi"))
}

// The activity clock counts more than CPU; so does the scan, as far as it
// can see without Hybernate.
func TestScanCluster_UnmanagedActivitySources(t *testing.T) {
	held := idleNow("held")
	held[0].(*appsv1.Deployment).Annotations = map[string]string{
		v1alpha1.AnnotationActiveUntil: scanTime.Add(2 * time.Hour).Format(time.RFC3339)}
	touched := idleNow("touched")
	touched[0].(*appsv1.Deployment).Annotations = map[string]string{
		v1alpha1.AnnotationLastActivity: scanTime.Add(-10 * time.Minute).Format(time.RFC3339)}
	expired := idleNow("expired")
	expired[0].(*appsv1.Deployment).Annotations = map[string]string{
		v1alpha1.AnnotationActiveUntil: scanTime.Add(-time.Hour).Format(time.RFC3339)}
	stale := idleNow("stale")
	stale[0].(*appsv1.Deployment).Annotations = map[string]string{
		v1alpha1.AnnotationLastActivity: scanTime.Add(-3 * time.Hour).Format(time.RFC3339)}
	deployed := deploymentWithRollout("deployed", testNamespace, 1, 20*time.Minute)
	deployed = append(deployed, makePodMetrics("deployed", testNamespace, "1m", "10Mi"))

	objs := make([]runtime.Object, 0, len(held)+len(touched)+len(expired)+len(stale)+len(deployed))
	for _, o := range [][]runtime.Object{held, touched, expired, stale, deployed} {
		objs = append(objs, o...)
	}
	got := byName(scanWorkloads(t, objs...))

	assert.Equal(t, StateActive, got["held"].State)
	assert.Equal(t, "kept awake until Oct 3 11:00 UTC", got["held"].Reason)
	assert.Equal(t, StateActive, got["touched"].State)
	assert.Equal(t, "activity annotation 10m ago", got["touched"].Reason)
	assert.Equal(t, StateIdle, got["expired"].State, "a hold that has ended doesn't keep it awake")
	assert.Equal(t, StateIdle, got["stale"].State, "activity older than idleAfter doesn't count")
	assert.Equal(t, "CPU 1%", got["stale"].Reason)
	assert.Equal(t, StateActive, got["deployed"].State)
	assert.Equal(t, "deployed 20m ago", got["deployed"].Reason)
}

// Busy CPU is the strongest evidence, so it's the reason given.
func TestScanCluster_BusyCPUExplainsActivity(t *testing.T) {
	objs := deploymentWithRollout("busy", testNamespace, 1, 5*time.Minute)
	objs = append(objs, makePodMetrics("busy", testNamespace, "900m", "64Mi"))

	got := byName(scanWorkloads(t, objs...))

	assert.Equal(t, "CPU 900%", got["busy"].Reason)
}

func managedFor(name string, status v1alpha1.ManagedWorkloadStatus) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name}},
		Status:     status,
	}
}

// Once Hybernate manages a workload, its activity clock already combines
// every source, so the scan reads it rather than guessing again.
func TestScanCluster_ReadsHybernatesClock(t *testing.T) {
	active := managedFor("active", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning,
		Activity: &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(scanTime.Add(-5 * time.Minute)),
			LastActivitySource: v1alpha1.ActivitySourcePrometheus}})
	quiet := managedFor("quiet", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning,
		Activity: &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(scanTime.Add(-3 * time.Hour)),
			LastActivitySource: v1alpha1.ActivitySourceCPU},
		Conditions: []metav1.Condition{{Type: "HeldByDependents", Status: metav1.ConditionTrue, Reason: "DependentsAwake"}}})
	measuring := managedFor("measuring", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning,
		Activity: &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(scanTime.Add(-2 * time.Hour)),
			LastActivitySource: v1alpha1.ActivitySourceCPU}})
	measuring.Spec.DryRun = true

	objs := make([]runtime.Object, 0, 9)
	objs = append(objs, active, quiet, measuring)
	for _, name := range []string{"active", "quiet", "measuring"} {
		objs = append(objs, idleNow(name)...)
	}
	got := byName(scanWorkloads(t, objs...))

	assert.Equal(t, StateActive, got["active"].State, "a Prometheus signal the scan can't see itself")
	assert.Equal(t, "active 5m ago (prometheus)", got["active"].Reason)
	assert.Equal(t, StateIdle, got["quiet"].State)
	assert.Equal(t, "no activity for 3h, held awake by dependents", got["quiet"].Reason)
	assert.True(t, got["measuring"].DryRun)
	assert.True(t, got["measuring"].Managed)
}

func TestScanCluster_ShowsWhatHybernateHasPaused(t *testing.T) {
	mw := managedFor("asleep", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhasePaused,
		Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3, PausedAt: ptr.To(metav1.NewTime(scanTime.Add(-5 * time.Hour))),
			Resources: &v1alpha1.ResourceSnapshot{Replicas: 3, CPUMillis: 200, MemoryBytes: 256 << 20}}})

	report := scanWorkloads(t, makeDeployment("asleep", testNamespace, 0, "100m", "128Mi", nil), mw)

	require.Len(t, report.Workloads, 1, "a paused workload isn't dropped as scaled to zero")
	w := report.Workloads[0]
	assert.Equal(t, StatePaused, w.State)
	assert.Equal(t, "paused 5h ago", w.Reason)
	assert.Equal(t, int32(3), w.Replicas, "what it ran before the pause")
	assert.Equal(t, int64(200), w.PodCPURequestMillis, "priced on what the pause freed")
	assert.Equal(t, 1, report.Totals.Paused)
	assert.InDelta(t, w.HourlyCost, report.Totals.PausedHourlyCost, 0.0001)
	assert.Zero(t, report.Totals.MonthlyCost, "a paused workload isn't costing anything")
}

func TestScanCluster_WakingIsActive(t *testing.T) {
	mw := managedFor("waking", v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseResuming,
		Activity: &v1alpha1.ActivityStatus{LastActivityTime: metav1.NewTime(scanTime.Add(-5 * time.Hour))}})

	got := byName(scanWorkloads(t, append(idleNow("waking"), mw)...))

	assert.Equal(t, StateActive, got["waking"].State)
	assert.Equal(t, "waking", got["waking"].Reason)
}

func TestMeasured(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	since := metav1.NewTime(now.Add(-48 * time.Hour))
	idleSince := metav1.NewTime(now.Add(-2 * time.Hour))
	summary := func() *v1alpha1.DryRunStatus {
		return &v1alpha1.DryRunStatus{Since: since, Pauses: 3, Slept: metav1.Duration{Duration: 10 * time.Hour}}
	}
	tests := []struct {
		name      string
		mw        *v1alpha1.ManagedWorkload
		wantNil   bool
		wantHours float64
	}{
		{name: "unmanaged", wantNil: true},
		{name: "not in dry-run", wantNil: true, mw: &v1alpha1.ManagedWorkload{
			Status: v1alpha1.ManagedWorkloadStatus{DryRun: summary()}}},
		{name: "awake", wantHours: 10, mw: &v1alpha1.ManagedWorkload{
			Spec:   v1alpha1.ManagedWorkloadSpec{DryRun: true},
			Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseRunning, DryRun: summary()}}},
		{name: "a would-be pause under way", wantHours: 12, mw: func() *v1alpha1.ManagedWorkload {
			d := summary()
			d.Resources = &v1alpha1.ResourceSnapshot{Replicas: 1}
			return &v1alpha1.ManagedWorkload{
				Spec: v1alpha1.ManagedWorkloadSpec{DryRun: true},
				Status: v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhaseIdle, LastTransitionTime: &idleSince,
					DryRun: d}}
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := measured(tt.mw, 0.5, now)
			if tt.wantNil {
				assert.Nil(t, m)
				return
			}
			require.NotNil(t, m)
			assert.Equal(t, 3, m.Pauses)
			assert.True(t, m.Since.Equal(since.Time))
			assert.InDelta(t, tt.wantHours, m.SleptHours, 0.001)
			assert.InDelta(t, tt.wantHours*0.5, m.Freed, 0.001, "priced at the scan's hourly cost")
		})
	}
}
