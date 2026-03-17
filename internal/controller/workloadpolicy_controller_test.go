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

package controller

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
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func wpScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = metricsv1beta1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func int32P(i int32) *int32 { return &i }

func TestWorkloadPolicyReconciler_ScanUpdatesStatus(t *testing.T) {
	ns := "staging"
	policy := &v1alpha1.WorkloadPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "staging-policy", Namespace: ns},
		Spec:       v1alpha1.WorkloadPolicySpec{},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api-server", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32P(2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api-server"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api-server"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "test:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1000m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					}},
				},
			},
		},
	}

	podMetrics := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-server-pod-1", Namespace: ns,
			Labels: map[string]string{"app": "api-server"},
		},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "main",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("20m"),
				corev1.ResourceMemory: resource.MustParse("50Mi"),
			},
		}},
	}

	c := fake.NewClientBuilder().
		WithScheme(wpScheme()).
		WithObjects(policy, deploy).
		WithStatusSubresource(policy).
		WithRuntimeObjects(podMetrics).
		Build()

	r := &WorkloadPolicyReconciler{Client: c, Scheme: wpScheme()}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "staging-policy", Namespace: ns},
	})
	require.NoError(t, err)

	var updated v1alpha1.WorkloadPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "staging-policy", Namespace: ns}, &updated))

	assert.Equal(t, 1, updated.Status.Summary.Total)
	assert.Equal(t, 1, updated.Status.Summary.Idle)
	assert.NotNil(t, updated.Status.LastScanAt)
	require.Len(t, updated.Status.Discovered, 1)
	assert.Equal(t, "api-server", updated.Status.Discovered[0].Name)
	assert.Equal(t, v1alpha1.ClassificationIdle, updated.Status.Discovered[0].Classification)
}

func TestWorkloadPolicyReconciler_AutoManageCreatesWorkloads(t *testing.T) {
	ns := "dev"
	mode := v1alpha1.PolicyModeAutoManage
	policy := &v1alpha1.WorkloadPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-policy", Namespace: ns},
		Spec: v1alpha1.WorkloadPolicySpec{
			Mode:   mode,
			DryRun: true,
		},
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "idle-svc", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32P(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "idle-svc"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "idle-svc"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "test:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		},
	}

	podMetrics := &metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name: "idle-svc-pod-1", Namespace: ns,
			Labels: map[string]string{"app": "idle-svc"},
		},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "main",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("5m"),
				corev1.ResourceMemory: resource.MustParse("20Mi"),
			},
		}},
	}

	c := fake.NewClientBuilder().
		WithScheme(wpScheme()).
		WithObjects(policy, deploy).
		WithStatusSubresource(policy).
		WithRuntimeObjects(podMetrics).
		Build()

	r := &WorkloadPolicyReconciler{Client: c, Scheme: wpScheme()}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "dev-policy", Namespace: ns},
	})
	require.NoError(t, err)

	var mw v1alpha1.ManagedWorkload
	err = c.Get(context.Background(), types.NamespacedName{Name: "Deployment-idle-svc", Namespace: ns}, &mw)
	require.NoError(t, err)

	assert.Equal(t, "idle-svc", mw.Spec.Target.Name)
	assert.Equal(t, v1alpha1.TargetKindDeployment, mw.Spec.Target.Kind)
	assert.True(t, mw.Spec.DryRun)
	assert.Equal(t, "true", mw.Labels[v1alpha1.LabelAutoDiscovered])
	assert.Equal(t, "dev-policy", mw.Annotations[v1alpha1.AnnotationWorkloadPolicy])
}

func TestWorkloadPolicyReconciler_PolicyNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(wpScheme()).Build()
	r := &WorkloadPolicyReconciler{Client: c, Scheme: wpScheme()}

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "missing", Namespace: "ns"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}
