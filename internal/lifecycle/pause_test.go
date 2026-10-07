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
	"github.com/okedeji/hybernate/internal/autoscaler"
)

func newTestPauser(c client.Client, scaler *fakeScaler) *Pauser {
	return &Pauser{
		client:      c,
		scaler:      scaler,
		autoscalers: autoscaler.NewFinder(c),
		clock:       func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
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
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 3}
	p := newTestPauser(c, scaler)

	require.NoError(t, p.Prepare(context.Background(), workload))
	assert.Equal(t, int32(3), scaler.replicas, "preparing changes nothing")
	assert.Nil(t, workload.Status.Pause.PausedAt)

	done, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
	assert.NotNil(t, workload.Status.Pause.PausedAt)
	assert.Equal(t, int32(0), scaler.replicas)
}

func TestPause_RequiresAPreparedRecord(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	scaler := &fakeScaler{replicas: 3}

	_, err := newTestPauser(c, scaler).Pause(context.Background(), workload)

	assert.ErrorIs(t, err, ErrNotPrepared)
	assert.Equal(t, int32(3), scaler.replicas)
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
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
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
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
	}
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 0, existingReplicas: 3}
	p := newTestPauser(c, scaler)

	done, err := p.Resume(context.Background(), workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), scaler.replicas)
}

// Pods that exist but aren't Ready, such as a database replaying its log,
// mean the resume isn't done yet.
func TestResume_WaitsForReadyNotJustExisting(t *testing.T) {
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
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Pause: &v1alpha1.PauseStatus{PreviousReplicas: 3},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	scaler := &fakeScaler{replicas: 0, existingReplicas: 3}
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
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
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

// A pause interrupted after scaling to zero is retried from its record: the
// target, now at zero, must never become the count to restore.
func TestPause_RetryKeepsTheRecordedReplicas(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(readyDeployment(3)).Build()
	scaler := &fakeScaler{replicas: 3}
	p := newTestPauser(c, scaler)
	workload := apiWorkload()

	require.NoError(t, p.Prepare(context.Background(), workload))
	_, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	workload.Status.Pause.PausedAt = nil

	done, err := p.Pause(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
}

// Restore hands the workload back without waiting for it, never scales
// down what someone else has already scaled up, and never starts one that
// was at zero before its pause.
func TestRestore(t *testing.T) {
	tests := []struct {
		name     string
		previous int32
		current  int32
		want     int32
	}{
		{name: "still at zero", previous: 3, current: 0, want: 3},
		{name: "already scaled up by someone", previous: 3, current: 5, want: 5},
		{name: "at zero before the pause", previous: 0, current: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := kedaClient(t, readyDeployment(tt.current), scaledObjectFor(1))
			scaler := &fakeScaler{replicas: tt.current}
			workload := apiWorkload()
			workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: tt.previous, ScaledObject: "api-scaler"}
			require.NoError(t, autoscaler.HoldKEDA(context.Background(), c, "default", "api-scaler", 0))

			replicas, err := newTestPauser(c, scaler).Restore(context.Background(), workload)

			require.NoError(t, err)
			assert.Equal(t, tt.want, replicas, "says what it left the target at")
			assert.Equal(t, tt.want, scaler.replicas)
			_, held := pausedAnnotation(t, c)
			assert.False(t, held, "KEDA scales it again")
		})
	}
}

func TestRestore_TargetGoneStillReleasesKEDA(t *testing.T) {
	c := kedaClient(t, scaledObjectFor(1))
	workload := apiWorkload()
	workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 3, ScaledObject: "api-scaler"}
	require.NoError(t, autoscaler.HoldKEDA(context.Background(), c, "default", "api-scaler", 0))

	replicas, err := newTestPauser(c, &fakeScaler{}).Restore(context.Background(), workload)
	require.NoError(t, err)
	assert.Zero(t, replicas)

	_, held := pausedAnnotation(t, c)
	assert.False(t, held)
}
