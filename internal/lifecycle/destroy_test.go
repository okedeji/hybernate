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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func newTestDestroyer(t *testing.T) (*Destroyer, *appsv1.Deployment, *v1alpha1.ManagedWorkload) { //nolint:unparam
	t.Helper()
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
	d := &Destroyer{
		client: c,
		clock:  func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
	}
	return d, dep, workload
}

func TestDestroy_DeletesWorkload(t *testing.T) {
	d, _, workload := newTestDestroyer(t)

	done, err := d.Destroy(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
	assert.NotNil(t, workload.Status.Destroy)
	assert.NotNil(t, workload.Status.Destroy.DestroyedAt)
	assert.Nil(t, workload.Status.Destroy.PVCRetentionExpiresAt)
}

func TestDestroy_SetsPVCRetentionExpiry(t *testing.T) {
	d, _, workload := newTestDestroyer(t)
	retention := metav1.Duration{Duration: 30 * 24 * time.Hour}
	workload.Spec.Destroy = &v1alpha1.DestroySpec{PVCRetention: &retention}

	done, err := d.Destroy(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
	assert.NotNil(t, workload.Status.Destroy.PVCRetentionExpiresAt)

	expected := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, expected, workload.Status.Destroy.PVCRetentionExpiresAt.Time)
}

func TestDestroy_Idempotent(t *testing.T) {
	d, _, workload := newTestDestroyer(t)

	destroyedAt := metav1.NewTime(time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC))
	workload.Status.Destroy = &v1alpha1.DestroyStatus{DestroyedAt: &destroyedAt}

	done, err := d.Destroy(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, &destroyedAt, workload.Status.Destroy.DestroyedAt)
}

func TestCleanupPVCs_DeletesAfterRetention(t *testing.T) {
	scheme := testScheme(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-api-0",
			Namespace: "default",
			Labels:    map[string]string{"app.kubernetes.io/name": "api"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	expiry := metav1.NewTime(time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Destroy: &v1alpha1.DestroyStatus{
				DestroyedAt:           &expiry,
				PVCRetentionExpiresAt: &expiry,
			},
		},
	}

	d := &Destroyer{
		client: c,
		clock:  func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
	}

	done, err := d.CleanupPVCs(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
	assert.Nil(t, workload.Status.Destroy.PVCRetentionExpiresAt)

	var found corev1.PersistentVolumeClaim
	err = c.Get(context.Background(), types.NamespacedName{Name: "data-api-0", Namespace: "default"}, &found)
	assert.Error(t, err)
}

func TestCleanupPVCs_WaitsBeforeRetentionExpiry(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	expiry := metav1.NewTime(time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Destroy: &v1alpha1.DestroyStatus{
				DestroyedAt:           &expiry,
				PVCRetentionExpiresAt: &expiry,
			},
		},
	}

	d := &Destroyer{
		client: c,
		clock:  func() metav1.Time { return metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)) },
	}

	done, err := d.CleanupPVCs(context.Background(), workload)

	assert.False(t, done)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pvc retention expires in")
}

func TestCleanupPVCs_NoDestroyStatusIsNoOp(t *testing.T) {
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
	}

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := &Destroyer{client: c, clock: metav1.Now}

	done, err := d.CleanupPVCs(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
}

func TestCleanupPVCs_NoRetentionExpiryIsNoOp(t *testing.T) {
	destroyedAt := metav1.NewTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Status: v1alpha1.ManagedWorkloadStatus{
			Destroy: &v1alpha1.DestroyStatus{DestroyedAt: &destroyedAt},
		},
	}

	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := &Destroyer{client: c, clock: metav1.Now}

	done, err := d.CleanupPVCs(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, done)
}
