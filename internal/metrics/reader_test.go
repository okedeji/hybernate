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

package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

func container(name, cpu string) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}}
}

func usage(name, cpu, mem string) metricsv1beta1.ContainerMetrics {
	return metricsv1beta1.ContainerMetrics{Name: name, Usage: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
	}}
}

func TestWorkloadContainers(t *testing.T) {
	nativeSidecar := container("log-shipper", "50m")
	nativeSidecar.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{container("migrate", "500m"), nativeSidecar},
		Containers:     []corev1.Container{container("app", "100m")},
	}

	containers := WorkloadContainers(spec)
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, c.Name)
	}

	assert.Equal(t, []string{"app", "log-shipper"}, names,
		"native sidecars run alongside the app; one-off init containers don't")
}

func TestUsage_CountsOnlyTheTemplatesContainers(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{container("app", "10m")}}
	pods := []metricsv1beta1.PodMetrics{
		{Containers: []metricsv1beta1.ContainerMetrics{usage("app", "1m", "6Mi"), usage("istio-proxy", "3m", "28Mi")}},
		{Containers: []metricsv1beta1.ContainerMetrics{usage("app", "2m", "6Mi"), usage("istio-proxy", "3m", "28Mi")}},
	}

	cpu, mem := Usage(pods, spec)

	assert.Equal(t, int64(3), cpu, "an injected proxy's CPU isn't the app's activity")
	assert.Equal(t, int64(12*1024*1024), mem)
}

// The experiment that found this: Istio injects its proxy at pod creation,
// so the template requests 10m while the pod uses 4m, 3m of it the proxy's.
// Counted together that's 40%, and the workload never looks idle. For cost,
// though, the proxy is part of what the workload costs and what pausing frees.
func TestReader_InjectedSidecar(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, metricsv1beta1.AddToScheme(scheme))
	labels := map[string]string{"app": "web"}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "mesh"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container("web", "10m")}},
			},
		},
	}
	proxy := container("istio-proxy", "100m")
	proxy.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "mesh", Labels: labels},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{proxy},
			Containers:     []corev1.Container{container("web", "10m")},
		},
	}
	podMetrics := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "mesh", Labels: labels},
		Containers: []metricsv1beta1.ContainerMetrics{usage("web", "1m", "6Mi"), usage("istio-proxy", "3m", "29Mi")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, pod, podMetrics).Build()
	r := NewReader(c, c)
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "mesh"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "web"}},
	}
	ctx := context.Background()

	t.Run("activity counts only the app", func(t *testing.T) {
		used, err := r.WorkloadCPUMillis(ctx, workload)
		require.NoError(t, err)
		requested, err := r.CPURequestPerReplica(ctx, workload)
		require.NoError(t, err)
		assert.InDelta(t, 0.1, used/requested, 0.001, "10% of the app's own request, not 40%")
	})

	t.Run("cost counts the whole pod", func(t *testing.T) {
		cpu, err := r.TotalCPUMillis(ctx, workload)
		require.NoError(t, err)
		assert.InDelta(t, 4, cpu, 0.001)
		mem, err := r.TotalMemoryBytes(ctx, workload)
		require.NoError(t, err)
		assert.InDelta(t, 35*1024*1024, mem, 1)
	})

	t.Run("pausing frees the whole pod's requests", func(t *testing.T) {
		cpu, mem, err := r.PodRequestsPerReplica(ctx, workload)
		require.NoError(t, err)
		assert.InDelta(t, 110, cpu, 0.001, "the app's 10m and the proxy's 100m")
		assert.InDelta(t, 128*1024*1024, mem, 1)
	})
}

func TestPodRequests(t *testing.T) {
	template := corev1.PodSpec{Containers: []corev1.Container{container("web", "10m")}}
	running := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container("web", "10m"), container("linkerd-proxy", "100m")}}}
	deleting := running
	deleting.DeletionTimestamp = ptr.To(metav1.Now())
	deleting.Spec = corev1.PodSpec{Containers: []corev1.Container{container("web", "10m"), container("linkerd-proxy", "500m")}}

	tests := []struct {
		name    string
		pods    []corev1.Pod
		wantCPU int64
	}{
		{name: "a running pod, sidecar included", pods: []corev1.Pod{running}, wantCPU: 110},
		{name: "a pod being deleted is skipped", pods: []corev1.Pod{deleting, running}, wantCPU: 110},
		{name: "no pod running falls back to the template", pods: nil, wantCPU: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, _ := PodRequests(tt.pods, template)
			assert.Equal(t, tt.wantCPU, cpu)
		})
	}
}

func pricedNode(name, instanceType, region string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		"node.kubernetes.io/instance-type": instanceType,
		"topology.kubernetes.io/region":    region,
	}}}
}

func webPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{container("web", "100m")}},
	}
}

func deleting(pod *corev1.Pod) *corev1.Pod {
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pod.Finalizers = []string{"e2e.hybernate.io/hold"}
	return pod
}

// A workload is priced at the nodes its pods are on, averaged over them.
func TestReader_ListRates(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	labels := map[string]string{"app": "web"}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
		},
	}
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "web"}},
	}
	small, _ := cost.ListPriceOf(cost.NodeType{InstanceType: "m6i.large", Region: "us-east-1"})
	large, _ := cost.ListPriceOf(cost.NodeType{InstanceType: "r6i.4xlarge", Region: "us-east-1"})
	nodes := []client.Object{pricedNode("small", "m6i.large", "us-east-1"),
		pricedNode("large", "r6i.4xlarge", "us-east-1"), pricedNode("metal", "custom.metal", "us-east-1")}

	tests := []struct {
		name    string
		pods    []client.Object
		want    cost.Rates
		listed  bool
		wantErr error
	}{
		{name: "one node type", pods: []client.Object{webPod("a", "small"), webPod("b", "small")},
			want: small.Rates, listed: true},
		{name: "averaged over its pods", pods: []client.Object{webPod("a", "small"), webPod("b", "large")},
			want: cost.Mean([]cost.Rates{small.Rates, large.Rates}), listed: true},
		{name: "unlisted nodes are left out", pods: []client.Object{webPod("a", "small"), webPod("b", "metal")},
			want: small.Rates, listed: true},
		{name: "a pod being deleted is left out", pods: []client.Object{webPod("a", "small"), deleting(webPod("b", "large"))},
			want: small.Rates, listed: true},
		{name: "only unlisted nodes", pods: []client.Object{webPod("a", "metal")}},
		{name: "not scheduled yet", pods: []client.Object{webPod("a", "")}, wantErr: ErrNoScheduledPods},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(append([]client.Object{deploy}, nodes...), tt.pods...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

			got, listed, err := NewReader(c, c).ListRates(context.Background(), workload)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.listed, listed)
			assert.InDelta(t, tt.want.CPUPerHour, got.CPUPerHour, 1e-9)
			assert.InDelta(t, tt.want.MemoryPerHour, got.MemoryPerHour, 1e-9)
		})
	}
}
