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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

const testNamespace = "test-ns"

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = metricsv1beta1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func int32Ptr(i int32) *int32 { return &i }

func makeDeployment(name, namespace string, replicas int32, cpuReq, memReq string, lbls map[string]string) *appsv1.Deployment { //nolint:unparam
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "test:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(cpuReq),
								corev1.ResourceMemory: resource.MustParse(memReq),
							},
						},
					}},
				},
			},
		},
	}
}

func makePodMetrics(name, namespace string, cpuUsage, memUsage string) *metricsv1beta1.PodMetrics { //nolint:unparam
	return &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-pod-1",
			Namespace: namespace,
			Labels:    map[string]string{"app": name},
		},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "main",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpuUsage),
				corev1.ResourceMemory: resource.MustParse(memUsage),
			},
		}},
	}
}

func TestScanner_Scan_ClassifiesWorkloads(t *testing.T) {
	ns := testNamespace
	th := DefaultThresholds()

	objects := []runtime.Object{
		// Idle: 10m CPU usage vs 50m threshold
		makeDeployment("idle-app", ns, 1, "1000m", "1Gi", nil),
		makePodMetrics("idle-app", ns, "10m", "100Mi"),

		// Wasteful: 200m usage / 1000m request = 20% < 30%
		makeDeployment("wasteful-app", ns, 1, "1000m", "2Gi", nil),
		makePodMetrics("wasteful-app", ns, "200m", "512Mi"),

		// Active: 500m usage / 1000m request = 50% > 30%
		makeDeployment("active-app", ns, 2, "1000m", "1Gi", nil),
		makePodMetrics("active-app", ns, "500m", "800Mi"),
	}

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objects...).Build()
	scanner := NewScanner(c)

	result, err := scanner.Scan(context.Background(), ns, []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment}, th)
	require.NoError(t, err)

	assert.Equal(t, 3, result.Summary.Total)
	assert.Equal(t, 1, result.Summary.Active)
	assert.Equal(t, 1, result.Summary.Idle)
	assert.Equal(t, 1, result.Summary.Wasteful)

	byName := make(map[string]v1alpha1.DiscoveredWorkload)
	for _, d := range result.Discovered {
		byName[d.Name] = d
	}

	assert.Equal(t, v1alpha1.ClassificationIdle, byName["idle-app"].Classification)
	assert.Equal(t, v1alpha1.ClassificationWasteful, byName["wasteful-app"].Classification)
	assert.Equal(t, v1alpha1.ClassificationActive, byName["active-app"].Classification)
}

func TestScanner_Scan_SkipsIgnored(t *testing.T) {
	ns := testNamespace
	th := DefaultThresholds()

	objects := []runtime.Object{
		makeDeployment("ignored-app", ns, 1, "1000m", "1Gi", map[string]string{
			v1alpha1.LabelIgnore: "true",
		}),
		makePodMetrics("ignored-app", ns, "10m", "100Mi"),
		makeDeployment("normal-app", ns, 1, "1000m", "1Gi", nil),
		makePodMetrics("normal-app", ns, "10m", "100Mi"),
	}

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objects...).Build()
	scanner := NewScanner(c)

	result, err := scanner.Scan(context.Background(), ns, []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment}, th)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Summary.Total)
	assert.Equal(t, "normal-app", result.Discovered[0].Name)
}

func TestScanner_Scan_DetectsManaged(t *testing.T) {
	ns := testNamespace
	th := DefaultThresholds()

	objects := []runtime.Object{
		makeDeployment("managed-app", ns, 1, "1000m", "1Gi", nil),
		makePodMetrics("managed-app", ns, "10m", "100Mi"),
		&v1alpha1.ManagedWorkload{
			ObjectMeta: metav1.ObjectMeta{Name: "managed-app-mw", Namespace: ns},
			Spec: v1alpha1.ManagedWorkloadSpec{
				Target: v1alpha1.WorkloadRef{
					Kind: v1alpha1.TargetKindDeployment,
					Name: "managed-app",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objects...).Build()
	scanner := NewScanner(c)

	result, err := scanner.Scan(context.Background(), ns, []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment}, th)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Summary.Managed)
	assert.True(t, result.Discovered[0].Managed)
}

func TestScanner_Scan_EmptyNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	scanner := NewScanner(c)

	result, err := scanner.Scan(context.Background(), "empty-ns", []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment}, DefaultThresholds())
	require.NoError(t, err)

	assert.Equal(t, 0, result.Summary.Total)
	assert.Empty(t, result.Discovered)
	assert.Equal(t, "$0.00", result.Summary.EstimatedMonthlySavings)
}

func TestScanner_Scan_SortsBySavingsDescending(t *testing.T) {
	ns := testNamespace
	th := DefaultThresholds()

	objects := []runtime.Object{
		// Small idle workload — less savings
		makeDeployment("small-idle", ns, 1, "100m", "128Mi", nil),
		makePodMetrics("small-idle", ns, "5m", "10Mi"),

		// Large idle workload — more savings
		makeDeployment("large-idle", ns, 4, "2000m", "8Gi", nil),
		makePodMetrics("large-idle", ns, "10m", "50Mi"),
	}

	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objects...).Build()
	scanner := NewScanner(c)

	result, err := scanner.Scan(context.Background(), ns, []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment}, th)
	require.NoError(t, err)
	require.Len(t, result.Discovered, 2)

	assert.Equal(t, "large-idle", result.Discovered[0].Name)
	assert.Equal(t, "small-idle", result.Discovered[1].Name)
}
