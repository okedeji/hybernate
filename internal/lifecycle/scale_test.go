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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func newTestScaler(t *testing.T, currentReplicas, readyReplicas int32) (*Scaler, *fakeScaler) {
	t.Helper()
	scheme := testScheme(t)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(currentReplicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	fs := &fakeScaler{replicas: currentReplicas, readyReplicas: readyReplicas}
	s := &Scaler{
		client: c,
		scaler: fs,
		clock:  func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
	}
	return s, fs
}

func testWorkload() *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
	}
}

func TestScale_ScalesUp(t *testing.T) {
	s, fs := newTestScaler(t, 5, 10)
	workload := testWorkload()

	done, err := s.Scale(context.Background(), workload, 10)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(10), fs.replicas)
	assert.Equal(t, int32(5), workload.Status.Scale.PreviousReplicas)
	assert.Equal(t, int32(10), workload.Status.Scale.CurrentReplicas)
	assert.NotNil(t, workload.Status.Scale.ScaledAt)
}

func TestScale_ScalesDown(t *testing.T) {
	s, fs := newTestScaler(t, 10, 2)
	workload := testWorkload()

	done, err := s.Scale(context.Background(), workload, 2)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(2), fs.replicas)
	assert.Equal(t, int32(10), workload.Status.Scale.PreviousReplicas)
	assert.Equal(t, int32(2), workload.Status.Scale.CurrentReplicas)
}

func TestScale_NoOpWhenAlreadyAtTarget(t *testing.T) {
	s, fs := newTestScaler(t, 5, 5)
	workload := testWorkload()

	done, err := s.Scale(context.Background(), workload, 5)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(5), fs.replicas)
	assert.Equal(t, int32(5), workload.Status.Scale.PreviousReplicas)
	assert.Equal(t, int32(5), workload.Status.Scale.CurrentReplicas)
}

func TestScale_NotReadyReturnsFalse(t *testing.T) {
	s, _ := newTestScaler(t, 5, 3)
	workload := testWorkload()

	done, err := s.Scale(context.Background(), workload, 10)

	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, int32(10), workload.Status.Scale.CurrentReplicas)
}
