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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reads records the reads a client makes, by verb, type and namespace,
// failing those fail says to.
type reads struct {
	mu   sync.Mutex
	made []string
}

func (r *reads) record(verb string, obj any, namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made = append(r.made, fmt.Sprintf("%s %T %s", verb, obj, namespace))
}

func (r *reads) of(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, made := range r.made {
		if strings.HasPrefix(made, prefix) {
			out = append(out, made)
		}
	}
	return out
}

func recordingClient(objs []runtime.Object, fail func(list client.ObjectList, namespace string) error) (
	client.Client, *reads) {
	r := &reads{}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var o client.ListOptions
				o.ApplyOptions(opts)
				r.record("list", list, o.Namespace)
				if fail != nil {
					if err := fail(list, o.Namespace); err != nil {
						return err
					}
				}
				return c.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				r.record("get", obj, key.Namespace)
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return c, r
}

// previews is a cluster of namespaces, each with a workload whose
// ConfigMap names its database's Service.
func previews(n int) ([]runtime.Object, []string) {
	objs := make([]runtime.Object, 0, 9*n)
	namespaces := make([]string, 0, n)
	for i := range n {
		ns := fmt.Sprintf("preview-%02d", i)
		namespaces = append(namespaces, ns)
		labels := map[string]string{"app": "api"}
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		objs = append(objs, running("api", ns, &metav1.LabelSelector{MatchLabels: labels}, labels, 1, "5m")...)
		db := database("postgres", ns)
		api := objs[len(objs)-4].(*appsv1.Deployment)
		api.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"}}}}
		objs = append(objs, db, depService(ns, "postgres", false, map[string]string{"app": "postgres"}),
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "api-config", Namespace: ns},
				Data: map[string]string{"DATABASE_URL": "postgres://postgres:5432/app"}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: ns}})
	}
	return objs, namespaces
}

// Scanning every namespace lists each kind across the cluster once, however
// many namespaces there are, and finds what scanning each namespace does.
func TestScanCluster_AllNamespacesAreListedAcrossTheCluster(t *testing.T) {
	objs, namespaces := previews(30)
	opts := scanOptions(namespaces...)
	perNamespace, _ := recordingClient(objs, nil)
	want, err := NewScanner(perNamespace, perNamespace).ScanCluster(context.Background(), opts)
	require.NoError(t, err)

	c, made := recordingClient(objs, nil)
	opts.AllNamespaces = true
	got, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Empty(t, made.of("get"), "no namespace or ConfigMap is read one at a time")
	for _, list := range made.of("list") {
		assert.True(t, strings.HasSuffix(list, " "), "listed across the cluster: %s", list)
	}
	assert.LessOrEqual(t, len(made.made), 12, "a few lists for the cluster, not for each namespace")
	require.Len(t, got.Workloads, 60)
	assert.Equal(t, want.Workloads, got.Workloads)
	assert.Equal(t, want.Notes, got.Notes)
	api := byName(got)["api"]
	require.NotNil(t, api.CPUPercent)
	require.Len(t, api.Dependencies, 1, "found through its ConfigMap")
}

// A kind the user may only list in each namespace is listed in each, and
// the others still across the cluster.
func TestScanCluster_AcrossTheClusterFallsBackToEachNamespace(t *testing.T) {
	objs, namespaces := previews(3)
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("cluster-wide"))
	c, made := recordingClient(objs, func(list client.ObjectList, namespace string) error {
		if _, ok := list.(*corev1.PodList); ok && namespace == "" {
			return denied
		}
		return nil
	})
	opts := scanOptions(namespaces...)
	opts.AllNamespaces = true

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Len(t, made.of("list *v1.PodList preview-"), 3, "pods listed in each namespace")
	assert.Len(t, made.of("list *v1.DeploymentList"), 1, "Deployments still listed once")
	assert.Empty(t, report.Notes)
	for _, w := range report.Workloads {
		if w.Name == "api" {
			require.NotNil(t, w.CPUPercent, w.Namespace)
			assert.Equal(t, 5, *w.CPUPercent)
		}
	}
}

// An API the cluster doesn't serve isn't asked for again in each namespace.
func TestScanCluster_AcrossTheClusterWithoutTheMetricsAPI(t *testing.T) {
	objs, namespaces := previews(3)
	unavailable := apierrors.NewServiceUnavailable("metrics-server is down")
	c, made := recordingClient(objs, func(list client.ObjectList, _ string) error {
		if _, ok := list.(*metricsv1beta1.PodMetricsList); ok {
			return unavailable
		}
		return nil
	})
	opts := scanOptions(namespaces...)
	opts.AllNamespaces = true

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	assert.Len(t, made.of("list *v1beta1.PodMetricsList"), 1)
	assert.Contains(t, report.Notes, "the Metrics API isn't available, so CPU couldn't be measured; is metrics-server installed?")
	assert.Empty(t, report.Incomplete)
}

// A scan that runs out of time reports the namespaces it scanned, and lists
// the rest, rather than nothing at all.
func TestScanCluster_OutOfTimeReportsWhatItScanned(t *testing.T) {
	objs, namespaces := previews(3)
	c, _ := recordingClient(objs, func(list client.ObjectList, namespace string) error {
		if _, ok := list.(*corev1.PodList); ok && namespace == "preview-02" {
			time.Sleep(500 * time.Millisecond)
			return context.DeadlineExceeded
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	report, err := NewScanner(c, c).ScanCluster(ctx, scanOptions(namespaces...))

	require.NoError(t, err)
	assert.Equal(t, 2, report.Namespaces, "the namespaces scanned")
	scanned := map[string]bool{}
	for _, w := range report.Workloads {
		scanned[w.Namespace] = true
	}
	assert.Equal(t, map[string]bool{"preview-00": true, "preview-01": true}, scanned)
	require.NotNil(t, report.Incomplete)
	assert.Equal(t, []string{"preview-02"}, report.Incomplete.Unscanned)
	assert.Contains(t, report.Notes, "the scan ran out of time before it had scanned 1 namespace, so their "+
		"workloads aren't in this report: preview-02")
}
