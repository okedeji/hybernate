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
	"encoding/json"
	"errors"
	"fmt"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// running is a Deployment with its ReplicaSet and pods, each pod with its
// metrics, using cpu.
func running(name, namespace string, selector *metav1.LabelSelector, podLabels map[string]string, pods int,
	cpu string) []runtime.Object {
	d := makeDeployment(name, namespace, int32(pods), "100m", "128Mi", nil)
	d.UID = types.UID(namespace + "-" + name)
	d.Spec.Selector = selector
	d.Spec.Template.Labels = podLabels
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-7d9f8c6b5", Namespace: namespace, UID: types.UID(namespace + "-" + name + "-rs"),
		CreationTimestamp: metav1.NewTime(scanTime.Add(-30 * 24 * time.Hour)),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: d.UID,
			Controller: ptr.To(true)}},
	}}
	objs := make([]runtime.Object, 0, 2+2*pods)
	objs = append(objs, d, rs)
	for i := range pods {
		podName := fmt.Sprintf("%s-7d9f8c6b5-pod%d", name, i)
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, Labels: podLabels,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name,
					UID: rs.UID, Controller: ptr.To(true)}}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi")}}}}},
		}, &metricsv1beta1.PodMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, Labels: podLabels},
			Containers: []metricsv1beta1.ContainerMetrics{{Name: "main", Usage: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("20Mi")}}},
		})
	}
	return objs
}

// calls counts the API calls a client makes, by verb and type.
type calls struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *calls) record(verb string, obj any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[fmt.Sprintf("%s %T", verb, obj)]++
}

func (c *calls) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.n {
		n += v
	}
	return n
}

func countingClient(objs []runtime.Object) (client.Client, *calls) {
	counted := &calls{n: map[string]int{}}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				counted.record("list", list)
				return c.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				counted.record("get", obj)
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return c, counted
}

// A namespace costs the same few calls however many workloads it has: its
// pods and their metrics are read once and matched in memory.
func TestScanCluster_APICallsDontGrowWithWorkloads(t *testing.T) {
	callsFor := func(workloads int) (int, *ClusterReport) {
		objs := make([]runtime.Object, 0, workloads*6)
		for i := range workloads {
			name := fmt.Sprintf("app-%d", i)
			labels := map[string]string{"app": name}
			objs = append(objs, running(name, testNamespace, &metav1.LabelSelector{MatchLabels: labels}, labels, 2, "5m")...)
		}
		c, counted := countingClient(objs)
		report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))
		require.NoError(t, err)
		return counted.total(), report
	}

	one, _ := callsFor(1)
	many, report := callsFor(30)

	assert.Equal(t, one, many, "calls for 1 workload and for 30")
	assert.LessOrEqual(t, one, 12, "a handful of calls a namespace")
	require.Len(t, report.Workloads, 30)
	for _, w := range report.Workloads {
		require.NotNil(t, w.CPUPercent, w.Name)
		assert.Equal(t, 5, *w.CPUPercent, "each matched to its own pods' metrics")
	}
}

// pagedReader serves a list two items a page, as an API server does with a
// limit.
type pagedReader struct {
	client.Reader
	pages []string
}

func (r *pagedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var o client.ListOptions
	o.ApplyOptions(opts)
	r.pages = append(r.pages, o.Continue)
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	pods := list.(*corev1.PodList)
	start := 0
	if o.Continue != "" {
		_, _ = fmt.Sscan(o.Continue, &start)
	}
	total := len(pods.Items)
	end := min(start+2, total)
	pods.Items = pods.Items[start:end]
	if end < total {
		pods.Continue = fmt.Sprint(end)
	}
	return nil
}

func TestListAll_ReadsEveryPage(t *testing.T) {
	objs := make([]runtime.Object, 0, 5)
	for i := range 5 {
		objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i), Namespace: testNamespace,
			Labels: map[string]string{fmt.Sprintf("only-%d", i): "x"}}})
	}
	r := &pagedReader{Reader: fake.NewClientBuilder().WithRuntimeObjects(objs...).Build()}
	var names []string
	var labelCounts []int

	err := listAll(context.Background(), r, func(list *corev1.PodList) {
		for _, p := range list.Items {
			names = append(names, p.Name)
			labelCounts = append(labelCounts, len(p.Labels))
		}
	}, client.InNamespace(testNamespace))

	require.NoError(t, err)
	assert.Equal(t, []string{"p0", "p1", "p2", "p3", "p4"}, names)
	assert.Equal(t, []string{"", "2", "4"}, r.pages, "each page asks for the next")
	assert.Equal(t, []int{1, 1, 1, 1, 1}, labelCounts, "a page's labels don't leak into the next's")
}

// A selector's expressions count: an expression-only one finds its pods,
// and a stable Deployment that excludes its canary's pods doesn't measure
// them, nor does the canary measure the stable's.
func TestScanCluster_SelectorsWithExpressions(t *testing.T) {
	stable := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "track", Operator: metav1.LabelSelectorOpNotIn,
			Values: []string{"canary"}}}}
	canary := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web", "track": "canary"}}
	worker := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app",
		Operator: metav1.LabelSelectorOpIn, Values: []string{"worker"}}}}
	objs := running("web", testNamespace, stable, map[string]string{"app": "web", "track": "stable"}, 2, "5m")
	objs = append(objs, running("web-canary", testNamespace, canary, map[string]string{"app": "web", "track": "canary"}, 1, "90m")...)
	objs = append(objs, running("worker", testNamespace, worker, map[string]string{"app": "worker"}, 1, "50m")...)

	got := byName(scanWorkloads(t, objs...))

	require.NotNil(t, got["web"].CPUPercent)
	assert.Equal(t, 5, *got["web"].CPUPercent, "its two pods' 10m over two pods' 200m; not the canary's 90m")
	assert.Equal(t, StateIdle, got["web"].State)
	require.NotNil(t, got["web-canary"].CPUPercent)
	assert.Equal(t, 90, *got["web-canary"].CPUPercent)
	require.NotNil(t, got["worker"].CPUPercent, "an expression-only selector finds its pods")
	assert.Equal(t, 50, *got["worker"].CPUPercent)
	assert.Equal(t, StateActive, got["worker"].State)
}

// Pods another controller owns are never a workload's, even when its
// selector matches them.
func TestScanCluster_PodsByOwner(t *testing.T) {
	labels := map[string]string{"app": "web"}
	objs := running("web", testNamespace, &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "1m")
	job := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-migrate-x7k2p", Namespace: testNamespace, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "web-migrate",
			UID: "job", Controller: ptr.To(true)}}}}
	jobMetrics := &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: testNamespace, Labels: labels},
		Containers: []metricsv1beta1.ContainerMetrics{{Name: "main", Usage: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("900m")}}}}

	got := byName(scanWorkloads(t, append(objs, job, jobMetrics)...))

	require.NotNil(t, got["web"].CPUPercent)
	assert.Equal(t, 1, *got["web"].CPUPercent, "the migration Job's 900m isn't web's")
}

// CPU is a share of what the pods measured request: replicas still starting
// have no metrics, and aren't counted as idle ones.
func TestScanCluster_CPUOverPodsMeasured(t *testing.T) {
	labels := map[string]string{"app": "api"}
	objs := running("api", testNamespace, &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "50m")
	objs[0].(*appsv1.Deployment).Spec.Replicas = ptr.To[int32](4)

	got := byName(scanWorkloads(t, objs...))

	require.NotNil(t, got["api"].CPUPercent)
	assert.Equal(t, 50, *got["api"].CPUPercent, "50m of the one measured pod's 100m, not of 4 replicas' 400m")
	assert.Equal(t, StateActive, got["api"].State)
}

func forbidden(what string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: what}, "", errors.New("denied"))
}

// failingIn fails each list of a type in the namespaces given with err.
func failingIn(objs []runtime.Object, failing func(list client.ObjectList, namespace string) error) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var o client.ListOptions
				o.ApplyOptions(opts)
				if err := failing(list, o.Namespace); err != nil {
					return err
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
}

// Pod metrics are read in every namespace, so one where they're denied
// doesn't stop the others being measured.
func TestScanCluster_MetricsPerNamespace(t *testing.T) {
	labels := map[string]string{"app": "api"}
	objs := running("api", "open", &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "1m")
	objs = append(objs, running("api", "locked", &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "1m")...)
	c := failingIn(objs, func(list client.ObjectList, namespace string) error {
		if _, ok := list.(*metricsv1beta1.PodMetricsList); ok && namespace == "locked" {
			return forbidden("pods")
		}
		return nil
	})

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions("locked", "open"))

	require.NoError(t, err)
	byNamespace := map[string]Workload{}
	for _, w := range report.Workloads {
		byNamespace[w.Namespace] = w
	}
	assert.NotNil(t, byNamespace["open"].CPUPercent, "measured where metrics can be read")
	assert.Equal(t, "pod metrics not allowed", byNamespace["locked"].Unmeasured)
	assert.Contains(t, report.Notes, "your access doesn't allow reading pod metrics in 1 namespace, so CPU there "+
		"couldn't be measured: locked")
	assert.Empty(t, report.Incomplete, "a denial is a limit of access, not a failed scan")
}

// Hundreds of namespaces the user can't read are one note, not hundreds.
func TestScanCluster_DeniedNamespacesAreOneNote(t *testing.T) {
	namespaces := make([]string, 0, 200)
	for i := range 200 {
		namespaces = append(namespaces, fmt.Sprintf("team-%03d", i))
	}
	c := failingIn(nil, func(client.ObjectList, string) error { return forbidden("deployments") })

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(namespaces...))

	require.NoError(t, err)
	assert.Equal(t, []string{"your access doesn't allow reading workloads in 200 namespaces, so they weren't " +
		"scanned: team-000, team-001, team-002 and 197 more"}, report.Notes)
	assert.Empty(t, report.Incomplete)
}

// Namespaces the user named are what they asked to scan, so being denied
// one leaves the scan incomplete.
func TestScanCluster_DeniedNamedNamespaceIsIncomplete(t *testing.T) {
	payments := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}}
	c := failingIn([]runtime.Object{payments}, func(client.ObjectList, string) error { return forbidden("deployments") })
	opts := scanOptions("payments")
	opts.NamedNamespaces = true

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Equal(t, &Incomplete{Denied: []string{"payments"}}, report.Incomplete)
}

// A namespace the user named that doesn't exist leaves the scan incomplete,
// for that reason.
func TestScanCluster_MissingNamedNamespaceIsIncomplete(t *testing.T) {
	c := failingIn(nil, func(client.ObjectList, string) error { return nil })
	opts := scanOptions("nope")
	opts.NamedNamespaces = true

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Equal(t, &Incomplete{Missing: []string{"nope"}}, report.Incomplete)
}

// A read that fails for any reason but access, such as client-go's rate
// limiter giving up before the deadline, which isn't a deadline error,
// leaves the scan incomplete, and says where.
func TestScanCluster_FailedReadIsIncomplete(t *testing.T) {
	labels := map[string]string{"app": "api"}
	objs := running("api", "fine", &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "1m")
	objs = append(objs, running("api", "slow", &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "1m")...)
	throttled := errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
	c := failingIn(objs, func(list client.ObjectList, namespace string) error {
		if _, ok := list.(*corev1.PodList); ok && namespace == "slow" {
			return throttled
		}
		return nil
	})

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions("fine", "slow"))

	require.NoError(t, err)
	assert.Equal(t, &Incomplete{Failed: []string{"slow"}}, report.Incomplete)
	assert.Contains(t, strings.Join(report.Notes, "\n"),
		"the scan is incomplete: it couldn't read pods in slow (client rate limiter Wait returned an error")
}

// An empty cluster reports no workloads as an empty list, not null.
func TestScanCluster_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))

	require.NoError(t, err)
	assert.Equal(t, 1, report.Namespaces)
	out, err := json.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"workloads":[]`)
	assert.Empty(t, report.Notes)
}
