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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

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

func doormanEndpointsV6(ips ...string) *discoveryv1.EndpointSlice {
	slice := doormanEndpoints(ips...)
	slice.Name = "hybernate-doorman-v6"
	slice.AddressType = discoveryv1.AddressTypeIPv6
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

// servingSlice is a Service's own slice with one Ready pod, as the
// EndpointSlice controller writes it while something behind it runs.
func servingSlice() *discoveryv1.EndpointSlice {
	const svc = "shop"
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: svc + "-abcde", Namespace: "default",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: svc,
				discoveryv1.LabelManagedBy:   "endpointslice-controller.k8s.io",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.244.1.9"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: svc + "-pod"},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(int32(8080))}},
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

func wakeCondition(t *testing.T, workload *v1alpha1.ManagedWorkload) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(workload.Status.Conditions, conditionWakeOnRequest)
	require.NotNil(t, cond)
	return cond
}

// lowestFreePort makes allocation predictable, so tests can make two
// workloads want the same port.
func lowestFreePort(used func(int32) bool) (int32, error) {
	for p := int32(20000); p <= 29999; p++ {
		if !used(p) {
			return p, nil
		}
	}
	return 0, doorman.ErrNoFreePort
}

func TestReconcileDoorman_RoutesPausedWorkload(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	headless := service("api-headless", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
	headless.Spec.ClusterIP = corev1.ClusterIPNone
	external := service("api-external", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
	external.Spec.Type = corev1.ServiceTypeExternalName
	r := doormanReconciler(t, workload,
		doormanEndpoints("10.0.0.7", "10.0.0.8"),
		service("api", map[string]string{"app": "api"},
			corev1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP},
			corev1.ServicePort{Name: "metrics", Port: 9090},
			corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}),
		service("other", map[string]string{"app": "other"}, corev1.ServicePort{Name: "http", Port: 80}),
		service("manual", nil, corev1.ServicePort{Name: "http", Port: 80}),
		headless, external,
	)

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	slices := doormanSlices(t, r)
	require.Len(t, slices, 1, "only the selector-based, cluster-IP Service that selects the workload is routed")
	slice := slices[0]
	assert.Equal(t, doorman.SliceName("api", "api", discoveryv1.AddressTypeIPv4), slice.Name)
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
				assert.Equal(t, tt.wantReason, wakeCondition(t, workload).Reason)
			}
		})
	}
}

// One doorman serves the whole cluster, so two workloads must never share a
// doorman port.
func TestReconcileDoorman_PortsAreUniqueAcrossWorkloads(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	other := lifecycleWorkload("other", nil, v1alpha1.PhasePaused)
	other.Namespace = "preview-9"
	other.UID = "other-uid"
	other.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "other", PortName: "http", DoormanPort: 20000}}
	r := doormanReconciler(t, workload, other, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	r.doormanPorts.pick = lowestFreePort

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	require.Len(t, workload.Status.Doorman, 1)
	assert.Equal(t, int32(20001), workload.Status.Doorman[0].DoormanPort)
}

// A port this workload held while paused before may since have gone to
// another workload; keeping it would send one workload's traffic to the other.
func TestReconcileDoorman_MovesOffAPortTakenByAnotherWorkload(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	const taken = int32(21000)
	workload.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "api", PortName: "http", DoormanPort: taken}}
	other := lifecycleWorkload("other", nil, v1alpha1.PhasePaused)
	other.Namespace = "preview-9"
	other.UID = "other-uid"
	other.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "other", PortName: "http", DoormanPort: taken}}
	r := doormanReconciler(t, other, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	require.NoError(t, r.doormanPorts.load(context.Background(), routeReader{reader: r.Client}, r.now()))
	r.doormanPorts.claims[taken] = portClaim{owner: other.UID, at: r.now()}

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	require.Len(t, workload.Status.Doorman, 1)
	assert.NotEqual(t, taken, workload.Status.Doorman[0].DoormanPort)
}

// A bulk pause reconciles many workloads at once, each before the others'
// routes are written, or seen in the cache. Every one still gets its own port.
func TestReconcileDoorman_ConcurrentPausesGetDistinctPorts(t *testing.T) {
	const n = 40
	objs := make([]client.Object, 0, 1+2*n)
	objs = append(objs, doormanEndpoints("10.0.0.7"))
	workloads := make([]*v1alpha1.ManagedWorkload, n)
	targets := make([]*appsv1.Deployment, n)
	for i := range n {
		name := fmt.Sprintf("app-%d", i)
		workloads[i] = lifecycleWorkload(name, nil, v1alpha1.PhasePaused)
		workloads[i].UID = types.UID(name)
		targets[i] = appTarget()
		targets[i].Name = name
		targets[i].Spec.Template.Labels = map[string]string{"app": name}
		objs = append(objs, workloads[i], service(name, map[string]string{"app": name}, corev1.ServicePort{Name: "http", Port: 80}))
	}
	r := doormanReconciler(t, objs...)
	r.doormanPorts.pick = lowestFreePort

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			assert.NoError(t, r.reconcileDoorman(context.Background(), workloads[i], targets[i]))
		})
	}
	wg.Wait()

	seen := map[int32]string{}
	for _, w := range workloads {
		require.Len(t, w.Status.Doorman, 1)
		port := w.Status.Doorman[0].DoormanPort
		assert.Empty(t, seen[port], "port %d given to both %s and %s", port, seen[port], w.Name)
		seen[port] = w.Name
	}
}

// Kube-proxy and load balancers keep sending to a released port for a
// moment. It isn't handed to another workload until the doorman's drain is
// over, or that workload would get the first one's traffic.
func TestReconcileDoorman_DrainingPortsAreNotReused(t *testing.T) {
	first := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	first.UID = "api-uid"
	second := lifecycleWorkload("billing", nil, v1alpha1.PhasePaused)
	second.UID = "billing-uid"
	billing := appTarget()
	billing.Name = "billing"
	billing.Spec.Template.Labels = map[string]string{"app": "billing"}
	r := doormanReconciler(t, first, second, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}),
		service("billing", map[string]string{"app": "billing"}, corev1.ServicePort{Name: "http", Port: 80}))
	r.doormanPorts.pick = lowestFreePort
	now := fixedTime
	r.clock = func() time.Time { return now }

	require.NoError(t, r.reconcileDoorman(context.Background(), first, appTarget()))
	released := first.Status.Doorman[0].DoormanPort
	first.Status.Phase = v1alpha1.PhaseRunning
	require.NoError(t, r.reconcileDoorman(context.Background(), first, appTarget()))

	require.NoError(t, r.reconcileDoorman(context.Background(), second, billing))
	assert.NotEqual(t, released, second.Status.Doorman[0].DoormanPort)

	now = now.Add(portDrain)
	second.Status.Phase = v1alpha1.PhaseRunning
	require.NoError(t, r.reconcileDoorman(context.Background(), second, billing))
	second.Status.Phase = v1alpha1.PhasePaused
	require.NoError(t, r.reconcileDoorman(context.Background(), second, billing))
	assert.Equal(t, released, second.Status.Doorman[0].DoormanPort, "free again once drained")
}

// The doorman's slices outlast any path that stops routing: turning the
// doorman off removes every one of the workload's.
func TestReconcileDoorman_DisablingRemovesSlices(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))
	require.Len(t, doormanSlices(t, r), 1)

	r.DoormanService = ""
	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	assert.Empty(t, doormanSlices(t, r))
	assert.Empty(t, workload.Status.Doorman)
	assert.False(t, meta.IsStatusConditionTrue(workload.Status.Conditions, conditionWakeOnRequest))
}

func TestReconcileDoorman_LeavesASliceItDoesNotOwn(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	theirs := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: doorman.SliceName("api", "api", discoveryv1.AddressTypeIPv4), Namespace: "default",
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

// Ports that only health checks or scrapers use can be left out, so they
// fail while the workload is paused instead of waking it.
func TestReconcileDoorman_IgnoresAnnotatedPorts(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	svc := service("api", map[string]string{"app": "api"},
		corev1.ServicePort{Name: "http", Port: 80},
		corev1.ServicePort{Name: "metrics", Port: 9090},
		corev1.ServicePort{Name: "health", Port: 8081})
	svc.Annotations = map[string]string{doorman.AnnotationIgnorePorts: "metrics, 8081"}
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"), svc)

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	require.Len(t, workload.Status.Doorman, 1)
	assert.Equal(t, "http", workload.Status.Doorman[0].PortName)
	slices := doormanSlices(t, r)
	require.Len(t, slices, 1)
	require.Len(t, slices[0].Ports, 1)
	assert.Equal(t, "http", *slices[0].Ports[0].Name)
}

func TestReconcileDoorman_IPFamilies(t *testing.T) {
	v4 := doormanEndpoints("10.0.0.7")
	v6 := doormanEndpointsV6("fd00::7")
	withFamilies := func(families ...corev1.IPFamily) *corev1.Service {
		svc := service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80})
		svc.Spec.IPFamilies = families
		return svc
	}
	tests := []struct {
		name       string
		doorman    []client.Object
		svc        *corev1.Service
		want       []discoveryv1.AddressType
		wantReason string
	}{
		{name: "IPv6 only", doorman: []client.Object{v6}, svc: withFamilies(corev1.IPv6Protocol),
			want: []discoveryv1.AddressType{discoveryv1.AddressTypeIPv6}, wantReason: "DoormanRouted"},
		{name: "dual-stack", doorman: []client.Object{v4, v6}, svc: withFamilies(corev1.IPv4Protocol, corev1.IPv6Protocol),
			want: []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6}, wantReason: "DoormanRouted"},
		{name: "dual-stack Service, IPv4 doorman", doorman: []client.Object{v4},
			svc:  withFamilies(corev1.IPv4Protocol, corev1.IPv6Protocol),
			want: []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4}, wantReason: "DoormanRouted"},
		{name: "IPv6 Service, IPv4 doorman", doorman: []client.Object{v4}, svc: withFamilies(corev1.IPv6Protocol),
			wantReason: "UnsupportedIPFamily"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
			r := doormanReconciler(t, append([]client.Object{workload, tt.svc}, tt.doorman...)...)

			require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

			routed := doormanSlices(t, r)
			families := make([]discoveryv1.AddressType, 0, len(routed))
			for _, slice := range routed {
				families = append(families, slice.AddressType)
				for _, ep := range slice.Endpoints {
					assert.Equal(t, slice.AddressType == discoveryv1.AddressTypeIPv6, strings.Contains(ep.Addresses[0], ":"),
						"each slice carries the doorman's addresses of its own family")
				}
			}
			assert.ElementsMatch(t, tt.want, families)
			cond := wakeCondition(t, workload)
			assert.Equal(t, tt.wantReason, cond.Reason)
			if len(tt.want) == 1 && len(tt.svc.Spec.IPFamilies) == 2 {
				assert.Contains(t, cond.Message, "api (IPv6)", "the family left unrouted is named")
			}
		})
	}
}

// Canary and stable behind one Service: each paused workload routes the
// Service to the doorman under its own slice, and once either is running,
// the Service sends traffic to it rather than waking the other.
func TestReconcileDoorman_SharedService(t *testing.T) {
	stable := lifecycleWorkload("stable", nil, v1alpha1.PhasePaused)
	stable.UID = "uid-stable"
	canary := lifecycleWorkload("canary", nil, v1alpha1.PhasePaused)
	canary.UID = "uid-canary"
	shop := service("shop", map[string]string{"app": "shop"}, corev1.ServicePort{Name: "http", Port: 80})
	r := doormanReconciler(t, stable, canary, doormanEndpoints("10.0.0.7"), shop)
	target := func(name string) *appsv1.Deployment {
		d := appTarget()
		d.Name = name
		d.Spec.Template.Labels = map[string]string{"app": "shop", "track": name}
		return d
	}
	ctx := context.Background()

	require.NoError(t, r.reconcileDoorman(ctx, stable, target("stable")))
	require.NoError(t, r.reconcileDoorman(ctx, canary, target("canary")))
	slices := doormanSlices(t, r)
	require.Len(t, slices, 2, "both paused: each routes the Service, so a request wakes one of them")
	assert.NotEqual(t, slices[0].Name, slices[1].Name)
	assert.NotEqual(t, stable.Status.Doorman[0].DoormanPort, canary.Status.Doorman[0].DoormanPort)

	stable.Status.Phase = v1alpha1.PhaseRunning
	require.NoError(t, r.reconcileDoorman(ctx, stable, target("stable")))
	require.NoError(t, r.Create(ctx, servingSlice()))
	require.NoError(t, r.reconcileDoorman(ctx, canary, target("canary")))

	assert.Empty(t, doormanSlices(t, r), "stable is running, so the Service's traffic goes to it")
	assert.Empty(t, canary.Status.Doorman)
	cond := wakeCondition(t, canary)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "ServedByOtherPods", cond.Reason)
	assert.Contains(t, cond.Message, "shop")
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
			cond := wakeCondition(t, workload)
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

// pausedRouted is a workload Hybernate paused, with its Service routed to
// the doorman.
func pausedRouted(t *testing.T) (*Reconciler, *stubPauser) {
	t.Helper()
	workload := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused)
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	workload.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 2, PausedAt: &pausedAt}
	workload.Status.Activity.LastActivityTime = metav1.NewTime(fixedTime.Add(-5 * time.Hour))
	target := targetDeploymentWithReplicas("api", "default", 0)
	target.Spec.Template.Labels = map[string]string{"app": "api"}
	pauser := &stubPauser{pauseDone: true, resumeDone: true}
	r := doormanReconciler(t, workload, target, defaultNamespace(nil), doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	r.pauser = pauser
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	require.Len(t, doormanSlices(t, r), 1)
	require.NotEmpty(t, fetch(t, r, "api").Status.Doorman)
	return r, pauser
}

// Every way Hybernate stops managing a paused workload hands it back with
// its Services routed to its own pods again.
func TestReconcile_StoppingManagementRemovesDoormanRouting(t *testing.T) {
	tests := []struct {
		name string
		stop func(t *testing.T, r *Reconciler)
	}{
		{name: "protected namespace", stop: func(_ *testing.T, r *Reconciler) {
			r.ProtectedNamespaces = []string{"def*"}
		}},
		{name: "ignore label", stop: func(t *testing.T, r *Reconciler) {
			var d appsv1.Deployment
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "api"}, &d))
			d.Labels = map[string]string{v1alpha1.LabelIgnore: v1alpha1.True}
			require.NoError(t, r.Update(context.Background(), &d))
		}},
		{name: "doorman turned off, then woken", stop: func(t *testing.T, r *Reconciler) {
			r.DoormanService = ""
			w := fetch(t, r, "api")
			w.Spec.DesiredState = desiredState(v1alpha1.DesiredStateRunning)
			require.NoError(t, r.Update(context.Background(), w))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := pausedRouted(t)
			tt.stop(t, r)

			for range 3 {
				_, err := r.Reconcile(context.Background(), reconcileFor("api"))
				require.NoError(t, err)
			}

			got := fetch(t, r, "api")
			assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
			assert.Empty(t, doormanSlices(t, r), "Services still send traffic to the doorman")
			assert.Empty(t, got.Status.Doorman, "the doorman still serves routes for it")
		})
	}
}

// A paused workload whose target is deleted has no pods to wake, so its
// Services stop sending requests to the doorman to wait for them.
func TestReconcile_TargetGoneRemovesDoormanRouting(t *testing.T) {
	r, _ := pausedRouted(t)
	require.NoError(t, r.Delete(context.Background(), targetDeploymentWithReplicas("api", "default", 0)))

	for range 2 {
		result, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)
		assert.Equal(t, targetRecheckInterval, result.RequeueAfter)
	}

	got := fetch(t, r, "api")
	assert.Empty(t, doormanSlices(t, r), "Services still send traffic to the doorman")
	assert.Empty(t, got.Status.Doorman, "the doorman still serves routes for it")
	assert.True(t, conditionIs(got, conditionTargetAvailable, metav1.ConditionFalse, "TargetNotFound"))
	assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, conditionWakeOnRequest))
}

// Deleting with --cascade=orphan leaves owned objects behind, so the slices
// are deleted explicitly rather than left to garbage collection.
func TestReconcile_OrphaningDeleteRemovesDoormanSlices(t *testing.T) {
	r, pauser := pausedRouted(t)
	require.NoError(t, r.Delete(context.Background(), fetch(t, r, "api"), client.PropagationPolicy(metav1.DeletePropagationOrphan)))

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	assert.Empty(t, doormanSlices(t, r))
	assert.Positive(t, pauser.restoreCalls, "and the target is handed back")
}

// A doorman problem, here the doorman's namespace missing from the cache of
// a watchNamespaces install, never stops a paused workload from waking.
func TestReconcile_DoormanFailureDoesNotBlockTheLifecycle(t *testing.T) {
	r, pauser := pausedRouted(t)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			o := client.ListOptions{}
			o.ApplyOptions(opts)
			if o.Namespace == "hybernate-system" {
				return fmt.Errorf("unable to list: %v because of unknown namespace for the cache", o.Namespace)
			}
			return c.List(ctx, list, opts...)
		}})
	w := fetch(t, r, "api")
	w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: fixedTime.UTC().Format(time.RFC3339)}
	require.NoError(t, r.Update(context.Background(), w))

	result, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	assert.LessOrEqual(t, result.RequeueAfter, doormanRetryMin, "retried soon")

	_, err = r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	assert.Positive(t, pauser.resumeCalls, "the workload still wakes")
	got := fetch(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseRunning, got.Status.Phase)
}

// A wake that takes a while is reconciled every few seconds without
// anything else to write. A routing failure meanwhile is recorded as it's
// announced, so it's announced once, not on every retry.
func TestReconcile_RoutingFailureWhileWakingIsAnnouncedOnce(t *testing.T) {
	r, pauser := pausedRouted(t)
	pauser.resumeDone = false
	w := fetch(t, r, "api")
	w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: fixedTime.UTC().Format(time.RFC3339)}
	require.NoError(t, r.Update(context.Background(), w))
	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)
	require.Equal(t, v1alpha1.PhaseResuming, fetch(t, r, "api").Status.Phase)
	recorder := r.Recorder.(*events.FakeRecorder)
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}

	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			o := client.ListOptions{}
			o.ApplyOptions(opts)
			if o.Namespace == r.DoormanNamespace {
				return errors.New("unable to list endpointslices")
			}
			return c.List(ctx, list, opts...)
		}})
	for range 3 {
		_, err := r.Reconcile(context.Background(), reconcileFor("api"))
		require.NoError(t, err)
	}

	var warnings int
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, reasonRoutingFailed) {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings)
	got := fetch(t, r, "api")
	assert.Equal(t, v1alpha1.PhaseResuming, got.Status.Phase)
	assert.True(t, conditionIs(got, conditionWakeOnRequest, metav1.ConditionFalse, reasonRoutingFailed), "and recorded")
}

func TestRouteDoorman_ReportsFailuresAndBacksOff(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("endpointslices is forbidden")
		}})

	first := r.routeDoorman(context.Background(), workload, appTarget())
	second := r.routeDoorman(context.Background(), workload, appTarget())

	assert.Equal(t, doormanRetryMin, first)
	assert.Equal(t, 2*doormanRetryMin, second, "backs off while it keeps failing")
	cond := wakeCondition(t, workload)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRoutingFailed, cond.Reason)
	assert.Contains(t, cond.Message, "forbidden")
	recorder := r.Recorder.(*events.FakeRecorder)
	var warnings int
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, reasonRoutingFailed) {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings, "one warning while it keeps failing")
}

// Only an EndpointSlice change that can move a paused workload's traffic
// reconciles it: the doorman's own endpoints, or a Service gaining or losing
// its last Ready pod, not every pod churn in the namespace.
func TestEndpointsChanged(t *testing.T) {
	r := doormanReconciler(t)
	p := r.endpointsChanged()
	notReady := servingSlice()
	notReady.Endpoints[0].Conditions.Ready = ptr.To(false)
	moved := servingSlice()
	moved.Endpoints[0].Addresses = []string{"10.244.1.10"}
	doormanMoved := doormanEndpoints("10.0.0.8")

	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: servingSlice(), ObjectNew: notReady}), "lost its last Ready pod")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: servingSlice(), ObjectNew: moved}), "still served")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: doormanEndpoints("10.0.0.7"), ObjectNew: doormanMoved}))
}

// With watchNamespaces, the operator has a Role in each watched namespace
// and nothing cluster-wide, so ports in use are read namespace by namespace.
func TestReconcileDoorman_AllocatesWithOnlyNamespacedAccess(t *testing.T) {
	workload := lifecycleWorkload("api", nil, v1alpha1.PhasePaused)
	r := doormanReconciler(t, workload, doormanEndpoints("10.0.0.7"),
		service("api", map[string]string{"app": "api"}, corev1.ServicePort{Name: "http", Port: 80}))
	r.WatchNamespaces = []string{"default"}
	r.PodReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			o := client.ListOptions{}
			o.ApplyOptions(opts)
			if o.Namespace == "" {
				return errors.New(`managedworkloads.hybernate.io is forbidden: cannot list resource at the cluster scope`)
			}
			return c.List(ctx, list, opts...)
		}})

	require.NoError(t, r.reconcileDoorman(context.Background(), workload, appTarget()))

	assert.Len(t, workload.Status.Doorman, 1)
}
