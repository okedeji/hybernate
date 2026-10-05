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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

var scaledObject = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

// readyDeployment is default/api at replicas, all of them Ready.
func readyDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 10},
	}
}

func scaledObjectFor(minReplicas int64) *unstructured.Unstructured {
	so := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"scaleTargetRef": map[string]any{"name": "api"}, "minReplicaCount": minReplicas}}}
	so.SetGroupVersionKind(scaledObject)
	so.SetNamespace("default")
	so.SetName("api-scaler")
	return so
}

func kedaClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{appsv1.SchemeGroupVersion.WithKind("Deployment"),
		autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"), scaledObject,
		v1alpha1.GroupVersion.WithKind("ManagedWorkload")} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper).WithObjects(objs...).Build()
}

func pausedAnnotation(t *testing.T, c client.Client) (string, bool) {
	t.Helper()
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(scaledObject)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api-scaler"}, so))
	v, ok := so.GetAnnotations()[autoscaler.PausedReplicasAnnotation]
	return v, ok
}

func apiWorkload() *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
	}
}

// A workload KEDA scales is held at zero by KEDA while it's paused, so KEDA
// doesn't scale it straight back up. Resuming, KEDA holds it at the restored
// replicas until they're Ready, so a ScaledObject that may scale to zero
// can't take it back down as it starts, then gets back whatever
// paused-replicas the user had set.
func TestPauseAndResume_KEDA(t *testing.T) {
	tests := []struct {
		name     string
		previous *string
	}{
		{name: "no paused-replicas of its own"},
		{name: "the user's own paused-replicas", previous: ptr.To("2")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			so := scaledObjectFor(0)
			if tt.previous != nil {
				so.SetAnnotations(map[string]string{autoscaler.PausedReplicasAnnotation: *tt.previous})
			}
			dep := readyDeployment(3)
			c := kedaClient(t, dep, so)
			scaler := &fakeScaler{replicas: 3}
			p := newTestPauser(c, scaler)
			workload := apiWorkload()

			require.NoError(t, p.Prepare(context.Background(), workload))
			done, err := p.Pause(context.Background(), workload)
			require.NoError(t, err)
			require.True(t, done)

			assert.Equal(t, "api-scaler", workload.Status.Pause.ScaledObject)
			assert.Equal(t, tt.previous, workload.Status.Pause.ScaledObjectPausedReplicas)
			v, held := pausedAnnotation(t, c)
			assert.True(t, held)
			assert.Equal(t, "0", v)
			assert.Equal(t, int32(0), scaler.replicas)

			dep.Status.ReadyReplicas = 0
			require.NoError(t, c.Status().Update(context.Background(), dep))
			done, err = p.Resume(context.Background(), workload)
			require.NoError(t, err)
			require.False(t, done)
			v, _ = pausedAnnotation(t, c)
			assert.Equal(t, "3", v, "held at the restored replicas while they start")

			dep.Status.ReadyReplicas = 3
			require.NoError(t, c.Status().Update(context.Background(), dep))
			done, err = p.Resume(context.Background(), workload)
			require.NoError(t, err)
			require.True(t, done)

			v, held = pausedAnnotation(t, c)
			assert.Equal(t, tt.previous != nil, held)
			if tt.previous != nil {
				assert.Equal(t, *tt.previous, v, "the user's own value is put back")
			}
			assert.Equal(t, int32(3), scaler.replicas)
		})
	}
}

// What a workload resumes to stays within its autoscaler's range, and is
// at least one: a ScaledObject that may go to zero would otherwise leave a
// woken workload at zero until a trigger fired.
func TestResume_WithinTheAutoscalersRange(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: ptr.To[int32](4), MaxReplicas: 8,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}
	tests := []struct {
		name     string
		objs     []client.Object
		previous int32
		want     int32
	}{
		{name: "below an HPA's minimum", objs: []client.Object{hpa}, previous: 2, want: 4},
		{name: "above its maximum", objs: []client.Object{hpa}, previous: 12, want: 8},
		{name: "KEDA down to zero", objs: []client.Object{scaledObjectFor(0)}, previous: 0, want: 1},
		{name: "no autoscaler", previous: 5, want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := kedaClient(t, append(tt.objs, readyDeployment(0))...)
			scaler := &fakeScaler{}
			workload := apiWorkload()
			workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: tt.previous}

			_, err := newTestPauser(c, scaler).Resume(context.Background(), workload)

			require.NoError(t, err)
			assert.Equal(t, tt.want, scaler.replicas)
		})
	}
}

// Only KEDA is held: an HPA leaves a workload at zero by itself.
func TestPause_HPAIsntHeld(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 4,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}
	c := kedaClient(t, readyDeployment(3), hpa)
	workload := apiWorkload()

	p := newTestPauser(c, &fakeScaler{replicas: 3})
	require.NoError(t, p.Prepare(context.Background(), workload))
	done, err := p.Pause(context.Background(), workload)

	require.NoError(t, err)
	require.True(t, done)
	assert.Empty(t, workload.Status.Pause.ScaledObject)
}
