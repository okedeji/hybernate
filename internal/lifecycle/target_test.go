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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var errAPIServer = errors.New("the API server is unavailable")

// A StatefulSet, such as a database, is paused and resumed as a Deployment
// is, and its resume waits for its own status to show the replicas Ready.
func TestPauseAndResume_StatefulSet(t *testing.T) {
	c := newClient(t, statefulSet(0))
	p := newTestPauser(c)
	workload := apiWorkload()
	workload.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet

	require.NoError(t, p.Prepare(context.Background(), workload))
	paused, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	require.True(t, paused.Done)
	ready := func(n int32) *appsv1.StatefulSet {
		var sts appsv1.StatefulSet
		require.NoError(t, c.Get(context.Background(), apiKey, &sts))
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas, sts.Status.ReadyReplicas = *sts.Spec.Replicas, n
		require.NoError(t, c.Status().Update(context.Background(), &sts))
		return &sts
	}
	assert.Equal(t, int32(0), *ready(0).Spec.Replicas)

	settled, err := resume(p, workload)
	require.NoError(t, err)
	assert.False(t, settled, "its pods aren't Ready yet")

	ready(2)
	settled, err = resume(p, workload)
	require.NoError(t, err)
	assert.True(t, settled)
}

func TestIsReady(t *testing.T) {
	stale := func(o client.Object) client.Object {
		o.SetGeneration(2)
		return o
	}
	tests := []struct {
		name   string
		target client.Object
		want   bool
	}{
		{name: "a Deployment, every replica Ready", target: deployment(3, 3), want: true},
		{name: "a Deployment, one not Ready", target: deployment(3, 2)},
		{name: "a Deployment at zero", target: deployment(0, 0)},
		{name: "a Deployment's status from before its spec", target: stale(deployment(3, 3))},
		{name: "a StatefulSet, every replica Ready", target: statefulSet(2), want: true},
		{name: "a StatefulSet, one not Ready", target: statefulSet(1)},
		{name: "a StatefulSet's status from before its spec", target: stale(statefulSet(2))},
		{name: "neither", target: &appsv1.DaemonSet{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsReady(tt.target))
		})
	}
}

// failingPatches is a client whose patches, which KEDA is held and released
// with, fail.
func failingPatches(c client.WithWatch) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errAPIServer
		}})
}

// A KEDA ScaledObject that can't be held at zero leaves the workload
// running, rather than paused while KEDA scales it straight back up; one
// that can't be held, or released, through a resume leaves it resuming, to
// be retried.
func TestKEDAHoldFailures(t *testing.T) {
	t.Run("pausing", func(t *testing.T) {
		c := newClient(t, deployment(3, 3), scaledObjectFor(0))
		workload := pausedWorkload(3)
		workload.Status.Pause.ScaledObject = scaledObjectName

		_, err := newTestPauser(failingPatches(c)).Pause(context.Background(), workload)

		require.ErrorIs(t, err, errAPIServer)
		assert.Equal(t, int32(3), replicas(t, c), "not scaled to zero")
	})
	t.Run("holding a resume", func(t *testing.T) {
		c := newClient(t, deployment(0, 0), scaledObjectFor(0))
		workload := pausedWorkload(3)
		workload.Status.Pause.ScaledObject = scaledObjectName

		_, err := newTestPauser(failingPatches(c)).Resume(context.Background(), workload, Waited{})

		require.ErrorIs(t, err, errAPIServer)
		assert.Zero(t, replicas(t, c), "not scaled up for KEDA to take back down")
	})
	t.Run("releasing a resume", func(t *testing.T) {
		c := newClient(t, deployment(0, 0), scaledObjectFor(0))
		workload := pausedWorkload(3)
		workload.Status.Pause.ScaledObject = scaledObjectName
		_, err := resume(newTestPauser(c), workload)
		require.NoError(t, err)
		settle(t, c, 3)

		r, err := newTestPauser(failingPatches(c)).Resume(context.Background(), workload, Waited{})

		require.ErrorIs(t, err, errAPIServer)
		assert.False(t, r.Settled)
		v, held := pausedAnnotation(t, c)
		assert.True(t, held)
		assert.Equal(t, "3", v)
	})
	t.Run("restoring", func(t *testing.T) {
		c := newClient(t, deployment(0, 0), scaledObjectFor(0))
		workload := pausedWorkload(3)
		workload.Status.Pause.ScaledObject = scaledObjectName

		_, err := newTestPauser(failingPatches(c)).Restore(context.Background(), workload)

		require.ErrorIs(t, err, errAPIServer)
	})
}

// A ScaledObject removed while the workload is paused leaves nothing to
// hold or release, and the resume finishes.
func TestResume_ScaledObjectGone(t *testing.T) {
	c := newClient(t, deployment(0, 0), scaledObjectFor(0))
	workload := pausedWorkload(3)
	workload.Status.Pause.ScaledObject = scaledObjectName
	require.NoError(t, c.Delete(context.Background(), scaledObjectFor(0)))
	p := newTestPauser(c)

	_, err := resume(p, workload)
	require.NoError(t, err)
	settle(t, c, 3)
	settled, err := resume(p, workload)

	require.NoError(t, err)
	assert.True(t, settled)
}

// Failures reading or scaling the target are returned, wrapped, for the
// caller to retry, and change nothing.
func TestTargetFailures(t *testing.T) {
	failingGets := func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok {
					return c.Get(ctx, key, obj, opts...)
				}
				return errAPIServer
			}})
	}
	failingScales := func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{
			SubResourceGet: func(context.Context, client.Client, string, client.Object, client.Object,
				...client.SubResourceGetOption) error {
				return errAPIServer
			}})
	}
	tests := []struct {
		name string
		wrap func(client.WithWatch) client.WithWatch
	}{
		{name: "reading the target", wrap: failingGets},
		{name: "reading its scale", wrap: failingScales},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, deployment(0, 0))
			p := newTestPauser(tt.wrap(c))

			assert.ErrorIs(t, p.Prepare(context.Background(), apiWorkload()), errAPIServer)
			_, err := p.Pause(context.Background(), pausedWorkload(3))
			assert.ErrorIs(t, err, errAPIServer)
			_, err = p.Resume(context.Background(), pausedWorkload(3), Waited{})
			assert.ErrorIs(t, err, errAPIServer)
			_, err = p.Restore(context.Background(), pausedWorkload(3))
			assert.ErrorIs(t, err, errAPIServer)
			assert.Zero(t, replicas(t, c))
		})
	}
}

func TestUnsupportedTargetKind(t *testing.T) {
	workload := pausedWorkload(3)
	workload.Spec.Target.Kind = "DaemonSet"

	_, err := newTestPauser(newClient(t)).Resume(context.Background(), workload, Waited{})

	assert.ErrorContains(t, err, "unsupported kind: DaemonSet")
}
