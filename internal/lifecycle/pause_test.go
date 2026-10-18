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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
)

func TestPause_ScalesToZero(t *testing.T) {
	c := newClient(t, deployment(3, 3))
	p := newTestPauser(c)
	workload := apiWorkload()

	require.NoError(t, p.Prepare(context.Background(), workload))
	assert.Equal(t, int32(3), replicas(t, c), "preparing changes nothing")
	assert.Nil(t, workload.Status.Pause.PausedAt)

	paused, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, Suspension{Running: 3}, paused, "not done while its pods run")
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
	assert.NotNil(t, workload.Status.Pause.PausedAt, "scaled to zero")
	assert.Equal(t, int32(0), replicas(t, c))

	settle(t, c, 0)
	paused, err = p.Pause(context.Background(), workload)
	require.NoError(t, err)
	assert.Equal(t, Suspension{Done: true}, paused, "done once they've stopped")
}

// Pods that don't stop, such as a StatefulSet's waiting on one that isn't
// Ready under OrderedReady pod management, don't hold the pause for good:
// it's done, saying how many still run, once each has had its grace period
// and a minute more, one after another for a StatefulSet.
func TestPause_StopsWaitingForPodsThatDontStop(t *testing.T) {
	tests := []struct {
		name    string
		target  client.Object
		waited  time.Duration
		pending bool
	}{
		{name: "a Deployment, within its grace", target: deployment(3, 3), waited: 89 * time.Second, pending: true},
		{name: "a Deployment, past its grace", target: deployment(3, 3), waited: 90 * time.Second},
		{name: "a StatefulSet, one pod after another", target: statefulSet(2), waited: 119 * time.Second, pending: true},
		{name: "a StatefulSet, past every pod's grace", target: statefulSet(2), waited: 2 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.target)
			p := newTestPauser(c)
			workload := pausedWorkload(2)
			if _, ok := tt.target.(*appsv1.StatefulSet); ok {
				workload.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet
			}
			pausedAt := metav1.NewTime(p.clock().Add(-tt.waited))
			workload.Status.Pause.PausedAt = &pausedAt

			paused, err := p.Pause(context.Background(), workload)

			require.NoError(t, err)
			assert.Equal(t, !tt.pending, paused.Done)
			assert.Positive(t, paused.Running)
			assert.Equal(t, &pausedAt, workload.Status.Pause.PausedAt, "timed from when it was first scaled to zero")
		})
	}
}

func TestPause_RequiresAPreparedRecord(t *testing.T) {
	c := newClient(t, deployment(3, 3))

	_, err := newTestPauser(c).Pause(context.Background(), apiWorkload())

	assert.ErrorIs(t, err, ErrNotPrepared)
	assert.Equal(t, int32(3), replicas(t, c))
}

func TestPause_Idempotent(t *testing.T) {
	c := newClient(t, deployment(0, 0))
	pausedAt := metav1.NewTime(time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC))
	workload := pausedWorkload(3)
	workload.Status.Pause.PausedAt = &pausedAt

	paused, err := newTestPauser(c).Pause(context.Background(), workload)

	require.NoError(t, err)
	assert.True(t, paused.Done)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
	assert.Equal(t, &pausedAt, workload.Status.Pause.PausedAt)
}

// A pause interrupted after scaling to zero is retried from its record: the
// target, now at zero, must never become the count to restore.
func TestPause_RetryKeepsTheRecordedReplicas(t *testing.T) {
	c := newClient(t, deployment(3, 3))
	p := newTestPauser(c)
	workload := apiWorkload()

	require.NoError(t, p.Prepare(context.Background(), workload))
	_, err := p.Pause(context.Background(), workload)
	require.NoError(t, err)
	workload.Status.Pause.PausedAt = nil

	_, err = p.Pause(context.Background(), workload)

	require.NoError(t, err)
	assert.Equal(t, int32(3), workload.Status.Pause.PreviousReplicas)
}

func TestResume_ScalesBackUp(t *testing.T) {
	c := newClient(t, deployment(0, 0))
	p := newTestPauser(c)
	workload := pausedWorkload(3)

	done, err := resume(p, workload)
	require.NoError(t, err)
	assert.False(t, done, "its pods aren't Ready yet")
	assert.Equal(t, int32(3), replicas(t, c))

	settle(t, c, 3)
	done, err = resume(p, workload)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, int32(3), replicas(t, c))
}

// Pods that exist but aren't Ready, such as a database replaying its log,
// mean the resume isn't done yet.
func TestResume_WaitsForReadyNotJustExisting(t *testing.T) {
	dep := deployment(3, 0)
	dep.Status.Replicas = 3
	c := newClient(t, dep)

	done, err := resume(newTestPauser(c), pausedWorkload(3))

	require.NoError(t, err)
	assert.False(t, done)
}

// A status its controller wrote before seeing the scale-up still counts
// the pods from before it: right after a pause, those not yet stopped. A
// resume done by it would drop the doorman's routes before any pod of the
// resume is Ready.
func TestResume_WaitsForAStatusOfTheScaleUp(t *testing.T) {
	tests := []struct {
		name   string
		target func() *appsv1.Deployment
	}{
		{name: "status from before the scale-up", target: func() *appsv1.Deployment {
			d := deployment(3, 3)
			d.Generation = 2
			return d
		}},
		{name: "a read from before the scale-up", target: func() *appsv1.Deployment { return deployment(0, 3) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stale := tt.target()
			ready, err := readyOf(context.Background(), newClient(t, stale), stale.DeepCopy(), 3)
			require.NoError(t, err)
			assert.Zero(t, ready)
		})
	}
}

// A workload that wasn't wholly Ready before its pause, such as one with a
// pod crash-looping, never has every replica Ready again. Its resume
// settles with those that are once ServingWait has passed since the first
// of them was, and with none once its controller's progress deadline has
// passed, releasing KEDA either way, rather than holding the workload
// Resuming, and KEDA, for good. Replicas slow to start together, such as
// pods waiting for new nodes, get ServingWait from when the first is Ready
// however long that took.
func TestResume_SettlesWithTheReplicasThatBecomeReady(t *testing.T) {
	tests := []struct {
		name     string
		ready    int32
		deadline *int32
		waited   time.Duration
		served   time.Duration
		settled  bool
	}{
		{name: "some Ready, not for long", ready: 2, waited: time.Hour, served: ServingWait - time.Second},
		{name: "some Ready for long enough", ready: 2, served: ServingWait, settled: true},
		{name: "none Ready, within the default deadline", waited: 9 * time.Minute},
		{name: "none Ready, past the default deadline", waited: 10 * time.Minute, settled: true},
		{name: "none Ready, past its own deadline", deadline: ptr.To[int32](60), waited: time.Minute, settled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := deployment(0, 0)
			dep.Spec.ProgressDeadlineSeconds = tt.deadline
			c := newClient(t, dep, scaledObjectFor(0))
			p := newTestPauser(c)
			workload := pausedWorkload(3)
			workload.Status.Pause.ScaledObject = scaledObjectName
			_, err := resume(p, workload)
			require.NoError(t, err)
			settle(t, c, tt.ready)

			r, err := p.Resume(context.Background(), workload, Waited{ScaledUp: tt.waited, Serving: tt.served})

			require.NoError(t, err)
			assert.Equal(t, Resumption{Replicas: 3, Ready: tt.ready, Settled: tt.settled}, r)
			_, held := pausedAnnotation(t, c)
			assert.Equal(t, !tt.settled, held, "KEDA is released once it's settled")
		})
	}
}

// An HPA scaling a resuming workload below the count it was restored to,
// as it starts with little load, isn't scaled back up on each retry, and
// the resume waits for the replicas the HPA asks for.
func TestResume_LeavesTheReplicasToTheAutoscalerOnceScaledUp(t *testing.T) {
	c := newClient(t, deployment(0, 0))
	p := newTestPauser(c)
	workload := pausedWorkload(3)
	_, err := resume(p, workload)
	require.NoError(t, err)

	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), apiKey, &d))
	d.Spec.Replicas = ptr.To[int32](2)
	require.NoError(t, c.Update(context.Background(), &d))
	settle(t, c, 2)

	r, err := p.Resume(context.Background(), workload, Waited{})

	require.NoError(t, err)
	assert.Equal(t, Resumption{Replicas: 2, Ready: 2, Settled: true}, r)
	assert.Equal(t, int32(2), replicas(t, c))
}

func TestResume_NoPauseStatusIsNoOp(t *testing.T) {
	done, err := resume(newTestPauser(newClient(t)), apiWorkload())

	require.NoError(t, err)
	assert.True(t, done)
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
			c := newClient(t, deployment(tt.current, 0), scaledObjectFor(1))
			workload := pausedWorkload(tt.previous)
			workload.Status.Pause.ScaledObject = scaledObjectName
			require.NoError(t, autoscaler.HoldKEDA(context.Background(), c, "default", scaledObjectName, 0))

			got, err := newTestPauser(c).Restore(context.Background(), workload)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "says what it left the target at")
			assert.Equal(t, tt.want, replicas(t, c))
			_, held := pausedAnnotation(t, c)
			assert.False(t, held, "KEDA scales it again")
		})
	}
}

func TestRestore_TargetGoneStillReleasesKEDA(t *testing.T) {
	c := newClient(t, scaledObjectFor(1))
	workload := pausedWorkload(3)
	workload.Status.Pause.ScaledObject = scaledObjectName
	require.NoError(t, autoscaler.HoldKEDA(context.Background(), c, "default", scaledObjectName, 0))

	got, err := newTestPauser(c).Restore(context.Background(), workload)
	require.NoError(t, err)
	assert.Zero(t, got)

	_, held := pausedAnnotation(t, c)
	assert.False(t, held)
}

func TestRestore_NoPauseIsNoOp(t *testing.T) {
	c := newClient(t, deployment(0, 0))

	got, err := newTestPauser(c).Restore(context.Background(), apiWorkload())

	require.NoError(t, err)
	assert.Zero(t, got)
	assert.Zero(t, replicas(t, c), "nothing Hybernate paused is started")
}

// KEDA, still holding a ScaledObject that may scale to zero at zero, would
// take the target straight back down if the scale-up came first, so Restore
// holds it at the restored count before scaling, as a resume does.
func TestRestore_HoldsKEDAAtTheRestoredCountWhileScaling(t *testing.T) {
	var heldAt string
	c := interceptor.NewClient(newClient(t, deployment(0, 0), scaledObjectFor(0)), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, inner client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			heldAt, _ = pausedAnnotation(t, inner)
			return inner.SubResource(sub).Update(ctx, obj, opts...)
		}})
	workload := pausedWorkload(3)
	workload.Status.Pause.ScaledObject = scaledObjectName
	require.NoError(t, autoscaler.HoldKEDA(context.Background(), c, "default", scaledObjectName, 0))

	got, err := newTestPauser(c).Restore(context.Background(), workload)

	require.NoError(t, err)
	assert.Equal(t, int32(3), got)
	assert.Equal(t, "3", heldAt, "held at the restored count as it's scaled up")
	_, held := pausedAnnotation(t, c)
	assert.False(t, held, "then released")
}
