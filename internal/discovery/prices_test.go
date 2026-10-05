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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

func node(name, instanceType string, labels ...string) *corev1.Node {
	l := map[string]string{
		"node.kubernetes.io/instance-type": instanceType,
		"topology.kubernetes.io/region":    "us-east-1",
	}
	for i := 0; i+1 < len(labels); i += 2 {
		l[labels[i]] = labels[i+1]
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

// podOn is a running pod of the workload named app, on a node.
func podOn(app, pod, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: testNamespace, Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "main",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi")}}}}},
	}
}

func listPrice(t *testing.T, instanceType string) cost.Rates {
	t.Helper()
	p, ok := cost.ListPriceOf(cost.NodeType{InstanceType: instanceType, Region: "us-east-1"})
	require.True(t, ok, instanceType)
	return p.Rates
}

// One pod at 1 vCPU and 4 GiB, an hour.
func podHour(r cost.Rates) float64 {
	return cost.ComputeHourly(1, 4, r)
}

// Each workload is priced at the nodes its own pods run on.
func TestScanCluster_PricesAtItsNodes(t *testing.T) {
	small, large := listPrice(t, "m6i.large"), listPrice(t, "r6i.4xlarge")
	objs := []runtime.Object{
		node("small-1", "m6i.large"), node("small-2", "m6i.large"), node("large-1", "r6i.4xlarge"),
		node("metal-1", "custom.metal"), node("spot-1", "m6i.large", "karpenter.sh/capacity-type", "spot"),
	}
	add := func(name string, nodes ...string) {
		objs = append(objs, makeDeployment(name, testNamespace, int32(len(nodes)), "1", "4Gi", nil))
		for i, n := range nodes {
			objs = append(objs, podOn(name, name+"-"+string(rune('a'+i)), n))
		}
	}
	add("on-small", "small-1")
	add("on-both", "small-2", "large-1")
	add("on-metal", "metal-1")
	add("on-spot", "spot-1")

	got := byName(scanWorkloads(t, objs...))

	assert.InDelta(t, podHour(small), got["on-small"].HourlyCost, 1e-9)
	assert.InDelta(t, 2*podHour(cost.Mean([]cost.Rates{small, large})), got["on-both"].HourlyCost, 1e-9,
		"averaged over its pods")
	assert.InDelta(t, podHour(cost.DefaultRates), got["on-metal"].HourlyCost, 1e-9,
		"a node type without a list price gets the scan's rates")
	assert.True(t, got["on-spot"].OnSpot)
	assert.False(t, got["on-small"].OnSpot)
	assert.InDelta(t, podHour(small), got["on-spot"].HourlyCost, 1e-9, "spot is priced at on-demand")
}

// A workload with no pod on a node is priced where Hybernate last saw it
// run, or at the cluster's most common node type.
func TestScanCluster_PricesPausedWorkloads(t *testing.T) {
	small := listPrice(t, "m6i.large")
	cpu, memory := resource.MustParse("0.05"), resource.MustParse("0.006")
	pausedStatus := func(rates *v1alpha1.CostRates) v1alpha1.ManagedWorkloadStatus {
		return v1alpha1.ManagedWorkloadStatus{Phase: v1alpha1.PhasePaused,
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &metav1.Time{Time: scanTime.Add(-time.Hour)},
				Resources: &v1alpha1.ResourceSnapshot{Replicas: 1, CPUMillis: 1000, MemoryBytes: 4 << 30}},
			Cost: &v1alpha1.CostStatus{ListRates: rates}}
	}
	recorded := managedFor("recorded", pausedStatus(&v1alpha1.CostRates{CPUPerHour: &cpu, MemoryPerHour: &memory}))
	unrecorded := managedFor("unrecorded", pausedStatus(nil))
	objs := []runtime.Object{node("small-1", "m6i.large"), node("small-2", "m6i.large"),
		node("large-1", "r6i.4xlarge"), recorded, unrecorded,
		makeDeployment("recorded", testNamespace, 0, "1", "4Gi", nil),
		makeDeployment("unrecorded", testNamespace, 0, "1", "4Gi", nil)}

	got := byName(scanWorkloads(t, objs...))

	assert.InDelta(t, podHour(cost.Rates{CPUPerHour: 0.05, MemoryPerHour: 0.006}), got["recorded"].HourlyCost, 1e-9,
		"where Hybernate last saw it run")
	assert.InDelta(t, podHour(small), got["unrecorded"].HourlyCost, 1e-9, "the most common node type")
}

func TestScanCluster_NodePrices(t *testing.T) {
	report := scanWorkloads(t, node("small-1", "m6i.large"), node("small-2", "m6i.large"),
		node("spot-1", "m6i.large", "eks.amazonaws.com/capacityType", "SPOT"), node("metal-1", "custom.metal"))

	small := listPrice(t, "m6i.large")
	assert.Equal(t, []NodeTypePrice{
		{Provider: "aws", InstanceType: "m6i.large", Region: "us-east-1", Nodes: 3, SpotNodes: 1, Listed: true,
			CPUPerHour: small.CPUPerHour, MemoryPerHour: small.MemoryPerHour},
		{InstanceType: "custom.metal", Region: "us-east-1", Nodes: 1},
	}, report.NodePrices.NodeTypes)
	assert.Empty(t, report.NodePrices.NodesUnread)
}

// Prices the user gives win over node prices, part by part.
func TestScanCluster_OwnPricesOverrideNodes(t *testing.T) {
	small := listPrice(t, "m6i.large")
	objs := []runtime.Object{node("small-1", "m6i.large"),
		makeDeployment("api", testNamespace, 1, "1", "4Gi", nil), podOn("api", "api-a", "small-1")}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()
	opts := scanOptions(testNamespace)
	opts.Rates.CPUPerHour, opts.OwnCPUPrice = 0.1, true

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)
	require.NoError(t, err)

	want := cost.Rates{CPUPerHour: 0.1, MemoryPerHour: small.MemoryPerHour}
	assert.InDelta(t, podHour(want), byName(report)["api"].HourlyCost, 1e-9)
}

func TestScanCluster_NodesForbidden(t *testing.T) {
	objs := []runtime.Object{node("small-1", "m6i.large"),
		makeDeployment("api", testNamespace, 1, "1", "4Gi", nil), podOn("api", "api-a", "small-1")}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch,
			list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*metav1.PartialObjectMetadataList); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
			}
			return c.List(ctx, list, opts...)
		}}).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))
	require.NoError(t, err)

	assert.Equal(t, "your access doesn't allow listing nodes", report.NodePrices.NodesUnread)
	assert.InDelta(t, podHour(cost.DefaultRates), byName(report)["api"].HourlyCost, 1e-9)
}

// The replay prices what a workload would have freed at its nodes' prices,
// the same as its cost.
func TestScanCluster_ReplaysAtItsNodesPrices(t *testing.T) {
	quiet := func(time.Time) float64 { return 0.002 }
	f := &fakePrometheus{from: scanTime.Add(-7 * 24 * time.Hour), byNamespace: map[string][]series{testNamespace: {
		{pod: "priced-7d9f8c6b5-abcde", container: "main", cores: quiet},
	}}}
	objs := deploymentWithRollout("priced", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, node("gpu-1", "g5.xlarge"), podOn("priced", "priced-7d9f8c6b5-abcde", "gpu-1"),
		makePodMetrics("priced", testNamespace, "1m", "20Mi"))
	opts := scanOptions(testNamespace)
	opts.History, opts.Window, opts.IdleAfter = newFakePrometheus(t, f), 7*24*time.Hour, time.Hour
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), opts)

	require.NoError(t, err)
	w := byName(report)["priced"]
	require.NotNil(t, w.History)
	require.Greater(t, w.History.SleepHours, 100.0)
	assert.InDelta(t, podHour(listPrice(t, "g5.xlarge")), w.HourlyCost, 1e-9)
	assert.InDelta(t, w.HourlyCost*w.History.SleepHours, w.History.Freed, 1e-6)
}

// A node without a region label can't be looked up, since prices differ by
// region, so its pods are priced at the scan's rates rather than guessed.
func TestScanCluster_NodeWithoutARegion(t *testing.T) {
	unplaced := node("small-1", "m6i.large")
	delete(unplaced.Labels, "topology.kubernetes.io/region")
	objs := []runtime.Object{unplaced, makeDeployment("api", testNamespace, 1, "1", "4Gi", nil),
		podOn("api", "api-a", "small-1")}

	report := scanWorkloads(t, objs...)

	assert.Equal(t, []NodeTypePrice{{InstanceType: "m6i.large", Nodes: 1}}, report.NodePrices.NodeTypes)
	assert.InDelta(t, podHour(cost.DefaultRates), byName(report)["api"].HourlyCost, 1e-9)
}
