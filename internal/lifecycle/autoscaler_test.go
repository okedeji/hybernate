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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/okedeji/hybernate/internal/autoscaler"
)

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
			c := newClient(t, deployment(3, 0), so)
			p := newTestPauser(c)
			workload := apiWorkload()

			require.NoError(t, p.Prepare(context.Background(), workload))
			paused, err := p.Pause(context.Background(), workload)
			require.NoError(t, err)
			require.True(t, paused.Done)

			assert.Equal(t, scaledObjectName, workload.Status.Pause.ScaledObject)
			assert.Equal(t, tt.previous, workload.Status.Pause.ScaledObjectPausedReplicas)
			v, held := pausedAnnotation(t, c)
			assert.True(t, held)
			assert.Equal(t, "0", v)
			assert.Equal(t, int32(0), replicas(t, c))

			settle(t, c, 0)
			done, err := resume(p, workload)
			require.NoError(t, err)
			require.False(t, done)
			v, _ = pausedAnnotation(t, c)
			assert.Equal(t, "3", v, "held at the restored replicas while they start")

			settle(t, c, 3)
			done, err = resume(p, workload)
			require.NoError(t, err)
			require.True(t, done)

			v, held = pausedAnnotation(t, c)
			assert.Equal(t, tt.previous != nil, held)
			if tt.previous != nil {
				assert.Equal(t, *tt.previous, v, "the user's own value is put back")
			}
			assert.Equal(t, int32(3), replicas(t, c))
		})
	}
}

// What a workload resumes to stays within its autoscaler's range. One that
// was at zero before its pause, such as one KEDA had scaled to zero, goes
// back to zero, and KEDA's triggers start it as they would have.
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
		{name: "KEDA that may go to zero", objs: []client.Object{scaledObjectFor(0)}, previous: 1, want: 1},
		{name: "at zero before the pause", objs: []client.Object{scaledObjectFor(0)}, previous: 0, want: 0},
		{name: "at zero before the pause, below an HPA's minimum", objs: []client.Object{hpa}, previous: 0, want: 0},
		{name: "no autoscaler", previous: 5, want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, append(tt.objs, deployment(0, 0))...)
			p := newTestPauser(c)
			workload := pausedWorkload(tt.previous)

			_, err := resume(p, workload)
			require.NoError(t, err)
			settle(t, c, tt.want)
			done, err := resume(p, workload)

			require.NoError(t, err)
			assert.True(t, done)
			assert.Equal(t, tt.want, replicas(t, c))
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
	c := newClient(t, deployment(3, 0), hpa)
	workload := apiWorkload()

	p := newTestPauser(c)
	require.NoError(t, p.Prepare(context.Background(), workload))
	paused, err := p.Pause(context.Background(), workload)

	require.NoError(t, err)
	require.True(t, paused.Done)
	assert.Empty(t, workload.Status.Pause.ScaledObject)
}

// A resume retried while its pods start doesn't annotate a ScaledObject
// that already holds the replicas it's resuming to.
func TestResume_HoldsKEDAOnce(t *testing.T) {
	patches := 0
	c := interceptor.NewClient(newClient(t, deployment(0, 0), scaledObjectFor(0)), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, patch, opts...)
		}})
	workload := pausedWorkload(3)
	workload.Status.Pause.ScaledObject = scaledObjectName
	p := newTestPauser(c)

	for range 3 {
		done, err := resume(p, workload)
		require.NoError(t, err)
		require.False(t, done)
	}

	assert.Equal(t, 1, patches)
	v, _ := pausedAnnotation(t, c)
	assert.Equal(t, "3", v)
}
