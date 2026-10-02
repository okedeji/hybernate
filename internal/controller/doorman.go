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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/doorman"
)

const (
	conditionWakeOnRequest = "WakeOnRequest"
	actionRouteDoorman     = "RouteDoorman"

	// annotationGKENEG puts a Service behind GKE's container-native load
	// balancing. GKE adds it to Services used by an Ingress by default.
	annotationGKENEG = "cloud.google.com/neg"

	labelServiceProxyName = "service.kubernetes.io/service-proxy-name"
)

// wakeOnRequest reports whether the workload's Services should route to the
// doorman while it's paused. A manual pause is excluded: the doorman wakes
// workloads, and a workload paused by desiredState doesn't wake until the
// user says so, so holding its connections would only delay a failure.
func wakeOnRequest(workload *v1alpha1.ManagedWorkload) bool {
	if w := workload.Spec.Wake; w != nil && w.OnRequest != nil && !*w.OnRequest {
		return false
	}
	return workload.Spec.DesiredState == nil
}

// doormanEligible reports whether doorman routes should exist now. They're
// added when a workload is paused and kept through Resuming, so connections
// arriving mid-wake are held too; they're removed once it's Running, which
// means its pods are Ready.
func doormanEligible(workload *v1alpha1.ManagedWorkload) bool {
	switch workload.Status.Phase {
	case v1alpha1.PhasePaused:
		return wakeOnRequest(workload)
	case v1alpha1.PhaseResuming:
		return len(workload.Status.Doorman) > 0
	default:
		return false
	}
}

// reconcileDoorman makes the workload's doorman EndpointSlices and routes
// match its phase.
func (r *Reconciler) reconcileDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) error {
	if r.DoormanService == "" {
		return nil
	}
	if !doormanEligible(workload) || target == nil {
		r.clearCondition(workload, conditionWakeOnRequest, "NotPaused")
		workload.Status.Doorman = nil
		return r.deleteDoormanSlices(ctx, workload, nil)
	}

	endpoints, err := r.doormanEndpoints(ctx)
	if err != nil {
		return err
	}
	if len(endpoints) == 0 {
		r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, "DoormanUnavailable",
			"no doorman pods are ready, so requests to this paused workload fail until one is")
		workload.Status.Doorman = nil
		return r.deleteDoormanSlices(ctx, workload, nil)
	}

	services, unsupported, err := r.servicesFor(ctx, workload, target)
	if err != nil {
		return err
	}
	routes, err := r.allocateRoutes(ctx, workload, services)
	if err != nil {
		if errors.Is(err, doorman.ErrNoFreePort) {
			r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, "NoFreePort", err.Error())
			return nil
		}
		return err
	}

	keep := map[string]bool{}
	for _, svc := range services {
		svcRoutes := routesFor(routes, svc.Name)
		if len(svcRoutes) == 0 {
			continue
		}
		if err := r.applyDoormanSlice(ctx, workload, svc, svcRoutes, endpoints); err != nil {
			return err
		}
		keep[doorman.SliceName(svc.Name)] = true
	}
	if err := r.deleteDoormanSlices(ctx, workload, keep); err != nil {
		return err
	}

	workload.Status.Doorman = routes
	switch {
	case len(routes) > 0:
		r.setDoormanCondition(workload, metav1.ConditionTrue, "DoormanRouted",
			"requests are held and wake the workload", unsupported)
	case len(unsupported) > 0:
		r.setDoormanCondition(workload, metav1.ConditionFalse, "UnsupportedLoadBalancer",
			"no Service can be routed through the doorman", unsupported)
	default:
		r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, "NoServices",
			"no Service routes TCP traffic to this workload")
	}
	return nil
}

// setDoormanCondition sets the WakeOnRequest condition, naming the Services
// left unrouted because of their load balancer. Requests through those fail
// while the workload is paused, so a warning event says so whenever that
// list changes.
func (r *Reconciler) setDoormanCondition(workload *v1alpha1.ManagedWorkload, status metav1.ConditionStatus,
	reason, message string, unsupported []string) {
	if len(unsupported) > 0 {
		message = fmt.Sprintf("%s; not routed, since GKE container-native load balancing (NEGs) can't use the doorman: %s",
			message, strings.Join(unsupported, ", "))
	}
	previous := meta.FindStatusCondition(workload.Status.Conditions, conditionWakeOnRequest)
	r.setCondition(workload, conditionWakeOnRequest, status, reason, message)
	if len(unsupported) > 0 && (previous == nil || previous.Message != message) {
		r.emitEvent(workload, false, "Warning", "UnsupportedLoadBalancer", actionRouteDoorman,
			"requests to Services %s won't wake the workload: GKE container-native load balancing (NEGs) can't use the doorman",
			strings.Join(unsupported, ", "))
	}
}

// doormanEndpoints returns an endpoint for each Ready doorman pod, from the
// doorman Service's own EndpointSlices. Each keeps its pod's node, zone, and
// pod reference: load balancers that register pods directly skip endpoints
// without a pod, and traffic policies of Local skip those without a node.
func (r *Reconciler) doormanEndpoints(ctx context.Context) ([]discoveryv1.Endpoint, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list, client.InNamespace(r.DoormanNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: r.DoormanService}); err != nil {
		return nil, fmt.Errorf("listing doorman endpoints: %w", err)
	}
	var out []discoveryv1.Endpoint
	for _, slice := range list.Items {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		for _, ep := range slice.Endpoints {
			if len(ep.Addresses) == 0 || (ep.Conditions.Ready != nil && !*ep.Conditions.Ready) {
				continue
			}
			out = append(out, discoveryv1.Endpoint{
				Addresses:  ep.Addresses[:1],
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
				NodeName:   ep.NodeName,
				Zone:       ep.Zone,
				TargetRef:  ep.TargetRef,
			})
		}
	}
	slices.SortFunc(out, func(a, b discoveryv1.Endpoint) int { return cmp.Compare(a.Addresses[0], b.Addresses[0]) })
	return slices.CompactFunc(out, func(a, b discoveryv1.Endpoint) bool { return a.Addresses[0] == b.Addresses[0] }), nil
}

// servicesFor returns the Services in the workload's namespace that select
// its pods and can be routed through the doorman, and the names of those
// that select it but can't because of their load balancer. Services without
// a selector manage their own endpoints, and headless Services hand callers
// pod addresses directly, so neither is routed.
func (r *Reconciler) servicesFor(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) ([]corev1.Service, []string, error) {
	podLabels := labels.Set(podTemplateLabels(target))
	var list corev1.ServiceList
	if err := r.List(ctx, &list, client.InNamespace(workload.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing services: %w", err)
	}
	var routable []corev1.Service
	var unsupported []string
	for _, svc := range list.Items {
		if len(svc.Spec.Selector) == 0 || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		if !labels.SelectorFromSet(svc.Spec.Selector).Matches(podLabels) {
			continue
		}
		if usesGKENEG(svc) {
			unsupported = append(unsupported, svc.Name)
			continue
		}
		routable = append(routable, svc)
	}
	slices.SortFunc(routable, func(a, b corev1.Service) int { return cmp.Compare(a.Name, b.Name) })
	slices.Sort(unsupported)
	return routable, unsupported, nil
}

// usesGKENEG reports whether a Service is behind GKE container-native load
// balancing. The NEG controller can fail a Service's whole sync on one
// endpoint it rejects, and it checks each endpoint's pod against the Service
// in the pod's own namespace, which for the doorman is the wrong one. Routing
// such a Service risks freezing its load balancer until the workload wakes.
// An annotation that can't be parsed counts as NEG, to stay safe.
func usesGKENEG(svc corev1.Service) bool {
	value, ok := svc.Annotations[annotationGKENEG]
	if !ok {
		return false
	}
	var neg struct {
		Ingress      bool                       `json:"ingress"`
		ExposedPorts map[string]json.RawMessage `json:"exposed_ports"`
	}
	if err := json.Unmarshal([]byte(value), &neg); err != nil {
		return true
	}
	return neg.Ingress || len(neg.ExposedPorts) > 0
}

func podTemplateLabels(target client.Object) map[string]string {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return t.Spec.Template.Labels
	case *appsv1.StatefulSet:
		return t.Spec.Template.Labels
	default:
		return nil
	}
}

// allocateRoutes gives each TCP Service port a doorman port. Ports already
// allocated to this workload are kept; new ones avoid every port in use by
// any workload in the cluster, since one doorman serves them all.
func (r *Reconciler) allocateRoutes(ctx context.Context, workload *v1alpha1.ManagedWorkload, services []corev1.Service) ([]v1alpha1.DoormanRoute, error) {
	existing := map[string]int32{}
	for _, route := range workload.Status.Doorman {
		existing[route.Service+"/"+route.PortName] = route.DoormanPort
	}

	var all v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("listing managed workloads: %w", err)
	}
	used := map[int32]bool{}
	for _, w := range all.Items {
		if w.UID == workload.UID {
			continue
		}
		for _, route := range w.Status.Doorman {
			used[route.DoormanPort] = true
		}
	}

	var routes []v1alpha1.DoormanRoute
	for _, svc := range services {
		for _, port := range svc.Spec.Ports {
			if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
				continue
			}
			key := svc.Name + "/" + port.Name
			allocated, ok := existing[key]
			if !ok || used[allocated] {
				var err error
				allocated, err = doorman.AllocatePort(workload.Namespace, svc.Name, port.Name, used)
				if err != nil {
					return nil, err
				}
			}
			used[allocated] = true
			routes = append(routes, v1alpha1.DoormanRoute{Service: svc.Name, PortName: port.Name, DoormanPort: allocated})
		}
	}
	return routes, nil
}

func routesFor(routes []v1alpha1.DoormanRoute, service string) []v1alpha1.DoormanRoute {
	var out []v1alpha1.DoormanRoute
	for _, route := range routes {
		if route.Service == service {
			out = append(out, route)
		}
	}
	return out
}

// applyDoormanSlice creates or updates the EndpointSlice that points one
// Service at the doorman. Each port keeps its Service port name, which is
// how kube-proxy matches a slice port to a Service port, so traffic to the
// Service port reaches the doorman port allocated for it.
func (r *Reconciler) applyDoormanSlice(ctx context.Context, workload *v1alpha1.ManagedWorkload, svc corev1.Service,
	routes []v1alpha1.DoormanRoute, endpoints []discoveryv1.Endpoint) error {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: doorman.SliceName(svc.Name), Namespace: workload.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, slice, func() error {
		if slice.ResourceVersion != "" && slice.Labels[discoveryv1.LabelManagedBy] != doorman.ManagedBy {
			return fmt.Errorf("endpointslice %s exists and isn't the doorman's", slice.Name)
		}
		slice.Labels = map[string]string{
			discoveryv1.LabelServiceName: svc.Name,
			discoveryv1.LabelManagedBy:   doorman.ManagedBy,
			doorman.LabelManagedWorkload: workload.Name,
		}
		// Proxies use this to pick the Services they serve, so the doorman's
		// slice must carry it whenever the Service's own slices do.
		if proxy, ok := svc.Labels[labelServiceProxyName]; ok {
			slice.Labels[labelServiceProxyName] = proxy
		}
		slice.AddressType = discoveryv1.AddressTypeIPv4
		slice.Endpoints = endpoints
		slice.Ports = make([]discoveryv1.EndpointPort, 0, len(routes))
		for _, route := range routes {
			slice.Ports = append(slice.Ports, discoveryv1.EndpointPort{
				Name:     ptr.To(route.PortName),
				Port:     ptr.To(route.DoormanPort),
				Protocol: ptr.To(corev1.ProtocolTCP),
			})
		}
		return controllerutil.SetControllerReference(workload, slice, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("applying doorman slice for service %s: %w", svc.Name, err)
	}
	return nil
}

// deleteDoormanSlices removes the workload's doorman slices except those in
// keep, so a workload that's awake again routes only to its own pods.
func (r *Reconciler) deleteDoormanSlices(ctx context.Context, workload *v1alpha1.ManagedWorkload, keep map[string]bool) error {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list, client.InNamespace(workload.Namespace), client.MatchingLabels{
		discoveryv1.LabelManagedBy:   doorman.ManagedBy,
		doorman.LabelManagedWorkload: workload.Name,
	}); err != nil {
		return fmt.Errorf("listing doorman slices: %w", err)
	}
	for i := range list.Items {
		slice := &list.Items[i]
		if keep[slice.Name] {
			continue
		}
		if err := r.Delete(ctx, slice); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting doorman slice %s: %w", slice.Name, err)
		}
	}
	return nil
}

// findWorkloadsForDoorman re-queues every paused or resuming workload when
// the doorman's own endpoints change, so their slices follow the doorman
// pods as they come and go.
func (r *Reconciler) findWorkloadsForDoorman(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.DoormanNamespace || obj.GetLabels()[discoveryv1.LabelServiceName] != r.DoormanService {
		return nil
	}
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing managed workloads for doorman change")
		return nil
	}
	var requests []reconcile.Request
	for _, w := range list.Items {
		if w.Status.Phase == v1alpha1.PhasePaused || w.Status.Phase == v1alpha1.PhaseResuming {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&w)})
		}
	}
	return requests
}

// findPausedWorkloadsInNamespace re-queues paused workloads when a Service
// in their namespace changes, since it may now select their pods.
func (r *Reconciler) findPausedWorkloadsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing managed workloads for service change", "namespace", obj.GetNamespace())
		return nil
	}
	var requests []reconcile.Request
	for _, w := range list.Items {
		if w.Status.Phase == v1alpha1.PhasePaused {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&w)})
		}
	}
	return requests
}
