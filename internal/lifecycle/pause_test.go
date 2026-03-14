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

package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func newTestPauser(c client.Client, scaler *fakeScaler) *Pauser {
	return &Pauser{
		client: c,
		scaler: scaler,
		clock:  func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
	}
}

func TestPause_ScalesToZero(t *testing.T) {
	scheme := testScheme(t)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
	}
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 3}
	p := newTestPauser(c, scaler)

	done, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
	assert.NotNil(t, workload.Status.Pause.PausedAt)
	assert.Equal(t, int32(0), scaler.replicas)
}

func TestPause_Idempotent(t *testing.T) {
	scheme := testScheme(t)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
	}
	pausedAt := metav1.NewTime(time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Pause: &v1alpha1.PauseStatus{
				PreviousReplicas: 3,
				PausedAt:         &pausedAt,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 0}
	p := newTestPauser(c, scaler)

	done, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
	assert.Equal(t, &pausedAt, workload.Status.Pause.PausedAt)
}

func TestResume_ScalesBackUp(t *testing.T) {
	scheme := testScheme(t)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
	}
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 0, readyReplicas: 3}
	p := newTestPauser(c, scaler)

	done, err := p.Resume(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Nil(t, workload.Status.Pause)
	assert.Equal(t, int32(3), scaler.replicas)
}

func TestResume_NotReadyRequeues(t *testing.T) {
	scheme := testScheme(t)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
	}
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 0, readyReplicas: 0}
	p := newTestPauser(c, scaler)

	done, err := p.Resume(context.Background(), workload)
	require.NoError(t, err)
	assert.False(t, done)
	assert.NotNil(t, workload.Status.Pause)
}

func TestResume_NoPauseStatusIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "api"},
		},
	}

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	scaler := &fakeScaler{}
	p := newTestPauser(c, scaler)

	done, err := p.Resume(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
}
