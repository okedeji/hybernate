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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/doorman"
)

// doormanEndpoints is the doorman Service's own slice, as the EndpointSlice
// controller writes it: one endpoint per pod.
func doormanEndpoints(ips ...string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "hybernate-doorman-abc", Namespace: "hybernate-system",
			Labels: map[string]string{discoveryv1.LabelServiceName: "hybernate-doorman"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for i, ip := range ips {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(fmt.Sprintf("node-%d", i)),
			Zone:       ptr.To("zone-a"),
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "hybernate-system", Name: fmt.Sprintf("doorman-%d", i)},
		})
	}
	return slice
}

func appTarget() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", "tier": "web"}},
		}},
	}
}

func service(name string, selector map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: selector, Ports: ports},
	}
}

func doormanReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	r := depReconciler(t, &stubPauser{}, objs...)
	r.DoormanService = "hybernate-doorman"
	r.DoormanNamespace = "hybernate-system"
	return r
}

func doormanSlices(t *testing.T, r *Reconciler) []discoveryv1.EndpointSlice {
	t.Helper()
	var list discoveryv1.EndpointSliceList
	require.NoError(t, r.List(context.Background(), &list, client.InNamespace("default"),
		client.MatchingLabels{discoveryv1.LabelManagedBy: doorman.ManagedBy}))
	return list.Items
}

func TestReconcileDoorman_RoutesPausedWorkload(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	headless := service("api-headless", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
	headless.Spec.ClusterIP = corev1.ClusterIPNone
	r := doormanReconciler(t, workload,
		doormanEndpoints("10.0.0.7", "10.0.0.8"),
		service("api", map[string]string{"app": "api"},
			corev1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Name: "metrics", Port: 9090},
			corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}),
		service("other", map[string]string{"app": "other"}, corev1.ServicePort{Name: "http", Port: 80}),
		service("manual", nil, corev1.ServicePort{Name: "http", Port: 80}),
		headless,
	)

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	slices := doormanSlices(t, r)
	require.Len(t, slices, 1, "only the selector-based, non-headless Service that selects the workload is routed")
	slice := slices[0]
	assert.Equal(t, doorman.SliceName("api"), slice.Name)
	assert.Equal(t, "api", slice.Labels[discoveryv1.LabelServiceName])
	assert.Equal(t, "api", slice.Labels[doorman.LabelManagedWorkload])
	require.Len(t, slice.Endpoints, 2, "one endpoint per doorman pod, so every pod gets traffic")
	for i, ep := range slice.Endpoints {
		assert.Equal(t, []string{[]string{"10.0.0.7", "10.0.0.8"}[i]}, ep.Addresses)
		assert.Equal(t, fmt.Sprintf("node-%d", i), *ep.NodeName, "Local traffic policies need the node")
		assert.Equal(t, "zone-a", *ep.Zone)
		require.NotNil(t, ep.TargetRef, "load balancers that register pods need the pod")
		assert.Equal(t, fmt.Sprintf("doorman-%d", i), ep.TargetRef.Name)
	}

	require.Len(t, workload.Status.Doorman, 2, "each TCP port gets a route; UDP doesn't")
	byName := map[string]int32{}
	for _, route := range workload.Status.Doorman {
		byName[route.PortName] = route.DoormanPort
	}
	require.Len(t, slice.Ports, 2)
	for _, p := range slice.Ports {
		assert.Equal(t, byName[*p.Name], *p.Port, "each slice port carries the Service port name and its doorman port")
	}
	assert.True(t, meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest))
}

func TestReconcileDoorman_KeptWhileResumingRemovedWhenRunning(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))
	port := workload.Status.Doorman[0].DoormanPort

	workload.Status.Phase = v1alpha1.PhaseResuming
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))
	assert.Len(t, doormanSlices(t, r), 1, "connections arriving mid-wake are still held")
	assert.Equal(t, port, workload.Status.Doorman[0].DoormanPort, "the port stays the same across the wake")

	workload.Status.Phase = v1alpha1.PhaseRunning
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))
	assert.Empty(t, doormanSlices(t, r), "once Running, traffic goes straight to the pods")
	assert.Empty(t, workload.Status.Doorman)
}

func TestReconcileDoorman_NotRouted(t *testing.T) {
	tests := []struct {
		name       string
		workload   func() *v1alpha1.ManagedWorkload
		doorman    *discoveryv1.EndpointSlice
		wantReason string
	}{
		{
			name: "manual pause doesn't wake on request",
			workload: func() *v1alpha1.ManagedWorkload {
				return lifecycleWorkload("api", ptr.To(v1alpha1.DesiredStatePaused), v1alpha1.PhasePaused)
			},
			doorman: doormanEndpoints("10.0.0.7"),
		},
		{
			name: "opted out",
			workload: func() *v1alpha1.ManagedWorkload {
				w := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
				w.Spec.Wake = &v1alpha1.WakeSpec{OnRequest: ptr.To(false)}
				return w
			},
			doorman: doormanEndpoints("10.0.0.7"),
		},
		{
			name:       "no doorman pod is ready",
			workload:   func() *v1alpha1.ManagedWorkload { return lifecycleWorkload("api", nil, v1alpha1.PhasePaused) },
			wantReason: "DoormanUnavailable",
		},
		{
			name:     "doorman pods aren't ready",
			workload: func() *v1alpha1.ManagedWorkload { return lifecycleWorkload("api", nil, v1alpha1.PhasePaused) },
			doorman: func() *discoveryv1.EndpointSlice {
				slice := doormanEndpoints("10.0.0.7")
				slice.Endpoints[0].Conditions.Ready = ptr.To(false)
				return slice
			}(),
			wantReason: "DoormanUnavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := tt.workload()
			objs := []client.Object{workload, service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})}
			if tt.doorman != nil {
				objs = append(objs, tt.doorman)
			}
			r := doormanReconciler(t, objs...)

			require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

			assert.Empty(t, doormanSlices(t, r))
			assert.Empty(t, workload.Status.Doorman)
			if tt.wantReason != "" {
				cond := meta.FindStatusCondition(workload.Status.Conditions, conditionWakeOnRequest)
				require.NotNil(t, cond)
				assert.Equal(t, tt.wantReason, cond.Reason)
			}
		})
	}
}

// One doorman serves the whole cluster, so two workloads must never share a
// doorman port, even when their Service ports hash to the same one.
func TestReconcileDoorman_PortsAreUniqueAcrossWorkloads(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	want, err := doorman.AllocatePort("default", "api", "http", nil)
	require.NoError(t, err)
	other := lifecycleWorkload("other", nil, v1alpha1.PhasePaused)
	other.Namespace = "sandbox-9"
	other.UID = "other-uid"
	other.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "other", PortName: "http", DoormanPort: want}}
	r := doormanReconciler(t, workload, other, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	require.Len(t, workload.Status.Doorman, 1)
	assert.NotEqual(t, want, workload.Status.Doorman[0].DoormanPort)
}

// A port this workload held while paused before may since have gone to
// another workload; keeping it would send one workload's traffic to the other.
func TestReconcileDoorman_MovesOffAPortTakenByAnotherWorkload(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	const taken = int32(21000)
	workload.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "api", PortName: "http", DoormanPort: taken}}
	other := lifecycleWorkload("other", nil, v1alpha1.PhasePaused)
	other.Namespace = "sandbox-9"
	other.UID = "other-uid"
	other.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "other", PortName: "http", DoormanPort: taken}}
	r := doormanReconciler(t, workload, other, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	require.Len(t, workload.Status.Doorman, 1)
	assert.NotEqual(t, taken, workload.Status.Doorman[0].DoormanPort)
}

func TestReconcileDoorman_DisabledWithoutDoormanService(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	r := depReconciler(t, &stubPauser{}, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	assert.Empty(t, doormanSlices(t, r))
}

func TestReconcileDoorman_LeavesASliceItDoesNotOwn(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	theirs := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: doorman.SliceName("api"), Namespace: "default",
			Labels: map[string]string{discoveryv1.LabelServiceName: "api"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.1.1.1"}}},
	}
	r := doormanReconciler(t, workload, theirs, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))

	require.Error(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	var got discoveryv1.EndpointSlice
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(theirs), &got))
	assert.Equal(t, []string{"10.1.1.1"}, got.Endpoints[0].Addresses, "another controller's slice is never overwritten")
}

func TestReconcileDoorman_CopiesTheServiceProxyName(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	svc := service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
	svc.Labels = map[string]string{labelServiceProxyName: "custom-proxy"}
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"), svc)

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	slices := doormanSlices(t, r)
	require.Len(t, slices, 1)
	assert.Equal(t, "custom-proxy", slices[0].Labels[labelServiceProxyName],
		"a Service served by another proxy must stay with that proxy")
}

func TestReconcileDoorman_SkipsGKENEGServices(t *testing.T) {
	neg := func(name, value string) *corev1.Service {
		svc := service(name, map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
		svc.Annotations = map[string]string{annotationGKENEG: value}
		return svc
	}

	tests := []struct {
		name       string
		services   []client.Object
		wantRouted []string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "every Service uses NEGs",
			services:   []client.Object{neg("api", `{"ingress":true}`)},
			wantStatus: metav1.ConditionFalse,
			wantReason: "UnsupportedLoadBalancer",
		},
		{
			name:       "exposed NEG ports",
			services:   []client.Object{neg("api", `{"exposed_ports":{"80":{}}}`)},
			wantStatus: metav1.ConditionFalse,
			wantReason: "UnsupportedLoadBalancer",
		},
		{
			name:       "an unparseable annotation counts as NEG",
			services:   []client.Object{neg("api", `ingress`)},
			wantStatus: metav1.ConditionFalse,
			wantReason: "UnsupportedLoadBalancer",
		},
		{
			name: "only the NEG Service is skipped",
			services: []client.Object{neg("api-gclb", `{"ingress":true}`),
				service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})},
			wantRouted: []string{"api"},
			wantStatus: metav1.ConditionTrue,
			wantReason: "DoormanRouted",
		},
		{
			name:       "NEGs turned off",
			services:   []client.Object{neg("api", `{"ingress":false}`)},
			wantRouted: []string{"api"},
			wantStatus: metav1.ConditionTrue,
			wantReason: "DoormanRouted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
			r := doormanReconciler(t, append([]client.Object{workload, doormanEndpoints("10.0.0.7")}, tt.services...)...)

			require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

			slices := doormanSlices(t, r)
			routed := make([]string, 0, len(slices))
			for _, slice := range slices {
				routed = append(routed, slice.Labels[discoveryv1.LabelServiceName])
			}
			assert.ElementsMatch(t, tt.wantRouted, routed)
			cond := meta.FindStatusCondition(workload.Status.Conditions, conditionWakeOnRequest)
			require.NotNil(t, cond)
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

// Requests through a skipped Service fail while the workload is paused, so
// that's surfaced as a warning, once rather than on every reconcile.
func TestReconcileDoorman_WarnsOnceAboutASkippedService(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	svc := service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
	svc.Annotations = map[string]string{annotationGKENEG: `{"ingress":true}`}
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"), svc)

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	recorder, ok := r.Recorder.(*events.FakeRecorder)
	require.True(t, ok)
	var warnings []string
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, "UnsupportedLoadBalancer") {
			warnings = append(warnings, e)
		}
	}
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "api")
}
