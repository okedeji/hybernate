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
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
		used, pods, err := r.WorkloadCPUMillis(ctx, workload)
		require.NoError(t, err)
		requested, err := r.CPURequestPerReplica(ctx, workload)
		require.NoError(t, err)
		assert.InDelta(t, 0.1, used/requested, 0.001, "10% of the app's own request, not 40%")
		assert.Equal(t, 1, pods)
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
	evicted := deleting
	evicted.DeletionTimestamp = nil
	evicted.Status.Phase = corev1.PodFailed

	tests := []struct {
		name    string
		pods    []corev1.Pod
		wantCPU int64
	}{
		{name: "a running pod, sidecar included", pods: []corev1.Pod{running}, wantCPU: 110},
		{name: "a pod being deleted is skipped", pods: []corev1.Pod{deleting, running}, wantCPU: 110},
		{name: "a failed pod is skipped", pods: []corev1.Pod{evicted, running}, wantCPU: 110},
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
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{container("web", "100m")}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
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
	pod := &corev1.Pod{ObjectMeta: meta, Spec: corev1.PodSpec{Containers: []corev1.Container{container("web", cpu)}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
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

	used, pods, err := r.WorkloadCPUMillis(ctx, workload)
	require.NoError(t, err)
	assert.InDelta(t, 100, used, 0.001, "only the stable pod's usage")
	assert.Equal(t, 1, pods, "measured over the stable pod alone")

	cpu, _, err := r.PodRequestsPerReplica(ctx, workload)
	require.NoError(t, err)
	assert.InDelta(t, 100, cpu, 0.001, "requests read from the stable pod")
}

// A Metrics API that fails isn't read again for a minute: every awake
// workload reads it each minute, and each would otherwise wait out a
// timeout to learn of the outage, holding a reconcile worker.
func TestReader_BacksOffAFailingMetricsAPI(t *testing.T) {
	web := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container("web", "100m")}},
			},
		},
	}
	pod, used := podOf("web-5d8f9-x2kqp", "5d8f9", "100m")
	var lists int
	down := true
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(web, pod, used).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*metricsv1beta1.PodMetricsList); ok {
					lists++
					if down {
						return errors.New("the server is currently unable to handle the request")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := NewReader(c, c)
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "web"}},
	}
	ctx := context.Background()
	read := func() error {
		_, _, err := r.WorkloadCPUMillis(ctx, workload)
		return err
	}

	require.Error(t, read())
	now = now.Add(metricsAPIBackoff - time.Second)
	err := read()
	require.ErrorIs(t, err, ErrMetricsAPIBackingOff)
	assert.Contains(t, err.Error(), "unable to handle the request", "says what failed")
	assert.Equal(t, 1, lists, "not read again within the backoff")

	now = now.Add(time.Second)
	require.Error(t, read())
	assert.Equal(t, 2, lists, "read again once it's up")

	down = false
	now = now.Add(metricsAPIBackoff)
	require.NoError(t, read())
	require.NoError(t, read())
	assert.Equal(t, 4, lists, "back, it's read every time")
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

func webDeployment(template corev1.Container) *appsv1.Deployment {
	labels := map[string]string{"app": "web"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: "web-uid", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{template}},
			},
		},
	}
}

func webWorkload() *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "web"}},
	}
}

// Activity is judged against what the running pods request: a LimitRange
// default, or a Vertical Pod Autoscaler's, is set on the pod at admission
// and isn't in the template. Judged against the template, a workload with
// LimitRange defaults could never be measured, and one under a VPA would
// be measured against requests its pods don't have.
func TestReader_CPURequestPerReplica(t *testing.T) {
	limitsOnly := corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}}}
	tests := []struct {
		name     string
		template corev1.Container
		podCPU   string
		want     float64
		wantErr  string
	}{
		{name: "a LimitRange default on the pod", template: corev1.Container{Name: "web"}, podCPU: "100m", want: 100},
		{name: "a VPA's request on the pod", template: container("web", "1"), podCPU: "50m", want: 50},
		{name: "no pod running reads the template", template: container("web", "300m"), want: 300},
		{name: "no pod running, a limit stands in", template: limitsOnly, want: 250},
		{name: "no pod running and no request", template: corev1.Container{Name: "web"},
			wantErr: "no pod of Deployment web is running, and its pod template requests no CPU"},
		{name: "running pods with no request", template: corev1.Container{Name: "web"}, podCPU: "0",
			wantErr: "the running pods of Deployment web request no CPU"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{webDeployment(tt.template)}
			if tt.podCPU != "" {
				pod, _ := podOf("web-5d8f9-x2kqp", "5d8f9", tt.podCPU)
				objs = append(objs, pod)
			}
			c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(objs...).Build()

			got, err := NewReader(c, c).CPURequestPerReplica(context.Background(), webWorkload())

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tt.want, got, 0.001)
		})
	}
}

// What a pod requests is read again after requestTTL, or at once when the
// target's spec changes, rather than listing pods on every check.
func TestReader_CPURequestPerReplicaIsKeptBriefly(t *testing.T) {
	deploy := webDeployment(container("web", "1"))
	pod, _ := podOf("web-5d8f9-x2kqp", "5d8f9", "100m")
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(deploy, pod).Build()
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	r := NewReader(c, c)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	read := func() float64 {
		t.Helper()
		got, err := r.CPURequestPerReplica(ctx, webWorkload())
		require.NoError(t, err)
		return got
	}
	resize := func(cpu string) {
		t.Helper()
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), pod))
		pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpu)
		require.NoError(t, c.Update(ctx, pod))
	}

	assert.InDelta(t, 100, read(), 0.001)
	resize("200m")
	now = now.Add(requestTTL - time.Second)
	assert.InDelta(t, 100, read(), 0.001, "kept within requestTTL")
	now = now.Add(time.Second)
	assert.InDelta(t, 200, read(), 0.001, "read again after it")

	resize("300m")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Generation++
	require.NoError(t, c.Update(ctx, deploy))
	assert.InDelta(t, 300, read(), 0.001, "read again once the target changes")
}

func TestAppCPURequest_CountsOnlyTheTemplatesContainers(t *testing.T) {
	template := corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	proxy := container("istio-proxy", "100m")
	proxy.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	pod := corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{proxy},
		Containers:     []corev1.Container{container("web", "100m"), container("linkerd-proxy", "100m")},
	}}

	cpu, fromPod := AppCPURequest([]corev1.Pod{pod}, template)

	assert.True(t, fromPod)
	assert.Equal(t, int64(100), cpu, "an injected sidecar's request isn't the workload's")
}

func runningPod(name, revision, cpu string, created time.Time) *corev1.Pod {
	pod, _ := podOf(name, revision, cpu)
	pod.CreationTimestamp = metav1.NewTime(created)
	return pod
}

// The request activity is judged against, and a pause is priced at, is a
// running pod's of the target's current revision. A failed pod, evicted or
// shut down with its node, stays listed with whatever its revision asked
// for: after a cut from 1 core to 100m, 50m of real use judged against it
// reads as 5%, and a busy workload is paused.
func TestReader_RequestsOfTheCurrentRevision(t *testing.T) {
	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	evicted := runningPod("web-1aaaa-old00", "1aaaa", "1", start)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	completed := runningPod("web-1aaaa-old01", "1aaaa", "1", start)
	completed.Status.Phase = corev1.PodSucceeded
	pending := runningPod("web-2bbbb-new01", "2bbbb", "2", start.Add(2*time.Hour))
	pending.Status.Phase = corev1.PodPending

	tests := []struct {
		name string
		pods []client.Object
		want float64
	}{
		{name: "an evicted pod of an old revision",
			pods: []client.Object{evicted, runningPod("web-9zzzz-new00", "9zzzz", "100m", start.Add(time.Hour))}, want: 100},
		{name: "a completed pod", pods: []client.Object{completed, runningPod("web-9zzzz-new00", "9zzzz", "100m", start)}, want: 100},
		{name: "a pod not running yet", pods: []client.Object{pending, runningPod("web-9zzzz-new00", "9zzzz", "100m", start)}, want: 100},
		{name: "mid-rollout, the newest revision's pod",
			pods: []client.Object{runningPod("web-1aaaa-old00", "1aaaa", "1", start),
				runningPod("web-9zzzz-new00", "9zzzz", "100m", start.Add(time.Hour)),
				runningPod("web-1aaaa-old01", "1aaaa", "1", start)}, want: 100},
		{name: "only failed pods, the template", pods: []client.Object{evicted}, want: 300},
		{name: "none running yet, as in a Recreate rollout, the newest starting pod",
			pods: []client.Object{evicted, pending}, want: 2000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{webDeployment(container("web", "300m"))}, tt.pods...)
			c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(objs...).Build()
			r := NewReader(c, c)

			activity, err := r.CPURequestPerReplica(context.Background(), webWorkload())
			require.NoError(t, err)
			pause, _, err := r.PodRequestsPerReplica(context.Background(), webWorkload())
			require.NoError(t, err)

			assert.InDelta(t, tt.want, activity, 0.001, "judged against")
			assert.InDelta(t, tt.want, pause, 0.001, "priced at")
		})
	}
}

// A StatefulSet's pods carry their revision, and it records which is
// current: its pods are replaced from the highest ordinal down, so the
// newest pod needn't be of it.
func TestReader_RequestsOfAStatefulSetsUpdateRevision(t *testing.T) {
	labels := map[string]string{"app": "db"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", UID: "db-uid", Generation: 2},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{container("db", "100m")}}},
		},
		Status: appsv1.StatefulSetStatus{CurrentRevision: "db-old", UpdateRevision: "db-new"},
	}
	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	pod := func(name, revision, cpu string, created time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", CreationTimestamp: metav1.NewTime(created),
				Labels: map[string]string{"app": "db", appsv1.ControllerRevisionHashLabelKey: revision}},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{container("db", cpu)}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(sts,
		pod("db-0", "db-old", "1", start.Add(time.Hour)), pod("db-1", "db-new", "100m", start)).Build()
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "db"}},
	}

	got, err := NewReader(c, c).CPURequestPerReplica(context.Background(), workload)

	require.NoError(t, err)
	assert.InDelta(t, 100, got, 0.001)
}

// Mid-rollout, what's read isn't kept: the first read after a change can
// find only the old revision's pods running, and keeping that would judge
// activity against the old request for requestTTL after the new pods run.
func TestReader_RequestReadMidRolloutIsntKept(t *testing.T) {
	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	old := runningPod("web-1aaaa-old00", "1aaaa", "1", start)
	current := runningPod("web-9zzzz-new00", "9zzzz", "100m", start.Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(readerScheme(t)).WithObjects(webDeployment(container("web", "100m")), old, current).Build()
	r := NewReader(c, c)
	ctx := context.Background()

	got, err := r.CPURequestPerReplica(ctx, webWorkload())
	require.NoError(t, err)
	assert.InDelta(t, 100, got, 0.001)
	_, cached := r.cachedRequest(webDeployment(container("web", "100m")))
	assert.False(t, cached, "two revisions running")

	require.NoError(t, c.Delete(ctx, old))
	_, err = r.CPURequestPerReplica(ctx, webWorkload())
	require.NoError(t, err)
	_, cached = r.cachedRequest(webDeployment(container("web", "100m")))
	assert.True(t, cached, "the rollout done")
}
