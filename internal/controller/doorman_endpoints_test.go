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
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/doorman"
)

// pausedRoutedMany routes n paused workloads through a doorman with one
// pod, at 10.0.0.7.
func pausedRoutedMany(t *testing.T, n int) *Reconciler {
	t.Helper()
	objs := make([]client.Object, 0, 1+2*n)
	objs = append(objs, doormanEndpoints("10.0.0.7"))
	workloads := make([]*v1alpha1.ManagedWorkload, 0, n)
	for i := range n {
		name := fmt.Sprintf("app-%d", i)
		w := lifecycleWorkload(name, v1alpha1.PhasePaused)
		workloads = append(workloads, w)
		objs = append(objs, w, service(name, map[string]string{"app": name}, corev1.ServicePort{Name: "http", Port: 80}))
	}
	r := doormanReconciler(t, objs...)
	for i, w := range workloads {
		target := appTarget()
		target.Name = w.Name
		target.Spec.Template.Labels = map[string]string{"app": fmt.Sprintf("app-%d", i)}
		require.NoError(t, doormanErr(context.Background(), r, w, target))
	}
	require.Len(t, doormanSlices(t, r), n)
	return r
}

func replaceDoormanPods(t *testing.T, r *Reconciler, ips ...string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, r.Delete(ctx, doormanEndpoints()))
	require.NoError(t, r.Create(ctx, doormanEndpoints(ips...)))
}

// When the doorman's pods change, as in a rolling update, every doorman
// slice is pointed at the new ones directly, without a full reconcile of
// each paused workload.
func TestDoormanSliceSync_MovesEverySliceToTheNewPods(t *testing.T) {
	r := pausedRoutedMany(t, 5)
	ctx := context.Background()
	var patches atomic.Int32
	writer := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches.Add(1)
			return c.Patch(ctx, obj, patch, opts...)
		}})
	sync := &doormanSliceSync{reader: r.Client, writer: writer, namespace: r.DoormanNamespace, service: r.DoormanService}

	replaceDoormanPods(t, r, "10.0.0.8", "10.0.0.9")
	_, err := sync.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)

	for _, slice := range doormanSlices(t, r) {
		addrs := make([]string, 0, len(slice.Endpoints))
		for _, ep := range slice.Endpoints {
			addrs = append(addrs, ep.Addresses...)
		}
		assert.Equal(t, []string{"10.0.0.8", "10.0.0.9"}, addrs, slice.Name)
	}
	assert.Equal(t, int32(5), patches.Load())

	_, err = sync.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, int32(5), patches.Load(), "slices already current aren't written again")
}

// A terminating doorman pod stops accepting connections within seconds,
// so it's left out at once, even while its endpoint is still serving.
func TestDoormanSliceSync_LeavesOutATerminatingPod(t *testing.T) {
	r := pausedRoutedMany(t, 1)
	ctx := context.Background()
	endpoints := doormanEndpoints("10.0.0.7", "10.0.0.8")
	endpoints.Endpoints[0].Conditions = discoveryv1.EndpointConditions{
		Serving: ptr.To(true), Terminating: ptr.To(true),
	}
	require.NoError(t, r.Delete(ctx, doormanEndpoints()))
	require.NoError(t, r.Create(ctx, endpoints))
	sync := &doormanSliceSync{reader: r.Client, writer: r.Client, namespace: r.DoormanNamespace, service: r.DoormanService}

	_, err := sync.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)

	slices := doormanSlices(t, r)
	require.Len(t, slices[0].Endpoints, 1)
	assert.Equal(t, []string{"10.0.0.8"}, slices[0].Endpoints[0].Addresses)
}

// Paused workloads are reconciled in full only when the doorman gains or
// loses every Ready pod in an IP family, which changes whether they can be
// routed at all, not on every change of pods.
func TestFindWorkloadsForDoorman_OnlyWhenFamiliesChange(t *testing.T) {
	r := pausedRoutedMany(t, 2)
	ctx := context.Background()

	assert.Len(t, r.findWorkloadsForDoorman(ctx, nil), 2, "the first look is a change")
	replaceDoormanPods(t, r, "10.0.0.8")
	assert.Empty(t, r.findWorkloadsForDoorman(ctx, nil), "the slice sync moves the slices")

	replaceDoormanPods(t, r)
	assert.Len(t, r.findWorkloadsForDoorman(ctx, nil), 2, "no Ready doorman pod: routing is removed")
	replaceDoormanPods(t, r, "10.0.0.9")
	assert.Len(t, r.findWorkloadsForDoorman(ctx, nil), 2, "a Ready doorman pod again: routed anew")
}

func TestDoormanSliceSync_IgnoresOtherSlices(t *testing.T) {
	r := pausedRoutedMany(t, 1)
	ctx := context.Background()
	theirs := servingSlice()
	require.NoError(t, r.Create(ctx, theirs))
	sync := &doormanSliceSync{reader: r.Client, writer: r.Client, namespace: r.DoormanNamespace, service: r.DoormanService}

	replaceDoormanPods(t, r, "10.0.0.8")
	_, err := sync.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)

	var got discoveryv1.EndpointSlice
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(theirs), &got))
	assert.Equal(t, []string{"10.244.1.9"}, got.Endpoints[0].Addresses)
	assert.Equal(t, doorman.ManagedBy, doormanSlices(t, r)[0].Labels[discoveryv1.LabelManagedBy])
}
