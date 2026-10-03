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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

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

// A sidecar injected at pod creation isn't in the template, so its CPU isn't
// the app's activity, though its requests are part of what the pod costs.
func TestScanCluster_InjectedSidecars(t *testing.T) {
	pm := makePodMetrics("meshed-app", testNamespace, "1m", "6Mi")
	pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
		Name:  "istio-proxy",
		Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("30m"), corev1.ResourceMemory: resource.MustParse("30Mi")},
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "meshed-app-1", Namespace: testNamespace, Labels: map[string]string{"app": "meshed-app"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "main", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}}},
			{Name: "istio-proxy", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}}},
		}},
	}
	objs := deploymentWithRollout("meshed-app", testNamespace, 1, 30*24*time.Hour)
	objs = append(objs, pm, pod)

	got := byName(scanWorkloads(t, objs...))["meshed-app"]

	assert.Equal(t, StateIdle, got.State, "1m of the app's 100m is idle; the proxy's 30m is not the app's")
	assert.Equal(t, int64(200), got.PodCPURequestMillis, "the proxy's 100m is freed too")
	assert.Equal(t, int64(256<<20), got.PodMemoryRequestBytes)
}
