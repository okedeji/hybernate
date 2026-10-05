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
	"strings"
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
	podLabels := map[string]string{"app": "web", appsv1.DefaultDeploymentUniqueLabelKey: "5d8f9"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-5d8f9-x2kqp", Namespace: "mesh", Labels: podLabels},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{proxy},
			Containers:     []corev1.Container{container("web", "10m")},
		},
	}
	podMetrics := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "web-5d8f9-x2kqp", Namespace: "mesh", Labels: podLabels},
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
		ObjectMeta: metav1.ObjectMeta{Name: "web-5d8f9-" + name, Namespace: "shop",
			Labels: map[string]string{"app": "web", appsv1.DefaultDeploymentUniqueLabelKey: "5d8f9"}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{container("web", "100m")}},
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

func readerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, metricsv1beta1.AddToScheme(scheme))
	return scheme
}

func podOf(name, hash, cpu string) (*corev1.Pod, *metricsv1beta1.PodMetrics) {
	meta := metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"app": "web"}}
	if hash != "" {
		meta.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = hash
	}
	pod := &corev1.Pod{ObjectMeta: meta, Spec: corev1.PodSpec{Containers: []corev1.Container{container("web", cpu)}}}
	used := &metricsv1beta1.PodMetrics{ObjectMeta: *meta.DeepCopy(),
		Containers: []metricsv1beta1.ContainerMetrics{usage("web", cpu, "10Mi")}}
	return pod, used
}

// A canary whose pods carry the stable Deployment's labels shares its
// selector, but isn't part of it: its usage and requests aren't counted.
// The selector here is set-based, which a matchLabels-only reader rejected.
func TestReader_CountsOnlyTheTargetsPods(t *testing.T) {
	stable := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}},
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container("web", "100m")}},
			},
		},
	}
	stablePod, stableUsage := podOf("web-5d8f9-x2kqp", "5d8f9", "100m")
	canaryPod, canaryUsage := podOf("web-canary-7c4b2-p9w8z", "7c4b2", "900m")
	barePod, bareUsage := podOf("web-5d8f9-zzzzz", "", "900m")
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(stable,
		canaryPod, canaryUsage, barePod, bareUsage, stablePod, stableUsage).Build()
	r := NewReader(c, c)
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "web"}},
	}
	ctx := context.Background()

	used, err := r.WorkloadCPUMillis(ctx, workload)
	require.NoError(t, err)
	assert.InDelta(t, 100, used, 0.001, "only the stable pod's usage")

	cpu, _, err := r.PodRequestsPerReplica(ctx, workload)
	require.NoError(t, err)
	assert.InDelta(t, 100, cpu, 0.001, "requests read from the stable pod")
}

func TestRunBy(t *testing.T) {
	deployment := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db"}}
	pod := func(name, hash string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if hash != "" {
			p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = hash
		}
		return p
	}
	long := strings.Repeat("a", 60)

	tests := []struct {
		name   string
		target client.Object
		pod    *corev1.Pod
		want   bool
	}{
		{name: "a Deployment's pod", target: deployment("web"), pod: pod("web-5d8f9-x2kqp", "5d8f9"), want: true},
		{name: "a canary's pod", target: deployment("web"), pod: pod("web-canary-7c4b2-p9w8z", "7c4b2")},
		{name: "a pod no ReplicaSet runs", target: deployment("web"), pod: pod("web-5d8f9-x2kqp", "")},
		{name: "a long-named Deployment's pod, its name cut short", target: deployment(long),
			pod: pod((long + "-5d8f9-")[:58]+"x2kqp", "5d8f9"), want: true},
		{name: "a StatefulSet's pod", target: statefulSet, pod: pod("db-0", ""), want: true},
		{name: "another StatefulSet's pod", target: statefulSet, pod: pod("db-canary-0", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, runBy(tt.target, tt.pod))
		})
	}
}

// Storage is the claims the workload's pods mount, which are rarely
// labelled like its pods.
func TestReader_TotalPVCBytes(t *testing.T) {
	claim := func(name, size string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
			Status: corev1.PersistentVolumeClaimStatus{
				Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		}
	}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	web := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{Selector: selector, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "uploads", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "uploads"}}}},
		}}},
	}
	db := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{Selector: selector,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}}},
	}
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(web, db,
		claim("uploads", "5Gi"), claim("data-db-0", "10Gi"), claim("data-db-1", "10Gi"),
		claim("data-db-canary-0", "100Gi"), claim("unrelated", "100Gi")).Build()
	r := NewReader(c, c)

	tests := []struct {
		kind v1alpha1.TargetKind
		name string
		want float64
	}{
		{kind: v1alpha1.TargetKindDeployment, name: "web", want: 5 << 30},
		{kind: v1alpha1.TargetKindStatefulSet, name: "db", want: 20 << 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := &v1alpha1.ManagedWorkload{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: "shop"},
				Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: tt.kind, Name: tt.name}},
			}

			got, err := r.TotalPVCBytes(context.Background(), workload)

			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 1)
		})
	}
}
