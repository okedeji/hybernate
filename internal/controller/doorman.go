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
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
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
	reasonRoutingFailed    = "RoutingFailed"

	// annotationGKENEG puts a Service behind GKE's container-native load
	// balancing. GKE adds it to Services used by an Ingress by default.
	annotationGKENEG = "cloud.google.com/neg"

	labelServiceProxyName = "service.kubernetes.io/service-proxy-name"

	// doormanRetryMin and doormanRetryMax bound how soon a workload whose
	// doorman routing failed is reconciled again, doubling with each failure
	// in a row.
	doormanRetryMin = 5 * time.Second
	doormanRetryMax = 5 * time.Minute

	// portDrain is how long a doorman port stays out of reach of other
	// workloads once its route is gone: the doorman's own drain, plus the
	// status write and the doorman's next sync that start it.
	portDrain = 2 * doorman.RouteDrain

	// portsReload is how often allocation re-reads every workload's routes,
	// letting go of ports claimed by workloads that went without releasing
	// them.
	portsReload = 30 * time.Minute

	// servedRecheck is how soon a paused workload is reconciled again while
	// a Service that selects it is left to other Ready pods. Those pods'
	// endpoints aren't cached, so nothing reports them going, and until the
	// workload is reconciled, requests through the Service fail rather than
	// wake it.
	servedRecheck = 30 * time.Second

	// endpointsReadTimeout bounds each uncached read of a Service's own
	// EndpointSlices.
	endpointsReadTimeout = 10 * time.Second
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

// routeDoorman brings the workload's doorman routing up to date. Routing is
// only ever a convenience on top of the lifecycle, so a failure is reported
// on the WakeOnRequest condition and retried, and never stops the reconcile.
// It returns how soon to reconcile again, to retry or to recheck, or zero.
func (r *Reconciler) routeDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) time.Duration {
	failing := conditionFalseWith(workload, conditionWakeOnRequest, reasonRoutingFailed)
	recheck, err := r.reconcileDoorman(ctx, workload, target)
	if err == nil {
		r.doormanFailures.reset(workload.UID)
		return recheck
	}
	log.FromContext(ctx).Error(err, "routing requests through the doorman",
		"workload", workload.Name, "namespace", workload.Namespace)
	r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, reasonRoutingFailed,
		fmt.Sprintf("requests may not wake the workload; retrying: %v", err))
	if !failing {
		r.announceRouting(ctx, workload, reasonRoutingFailed, "requests may not wake the workload: %v", err)
	}
	return r.doormanFailures.next(workload.UID)
}

// announceRouting warns about a change in the workload's routing once the
// condition reporting it is written. A pause or wake in progress is retried
// every few seconds without writing status, so a change announced but left
// unwritten would be announced again on every retry. If the write fails,
// the change is announced when a later reconcile finds it again.
func (r *Reconciler) announceRouting(ctx context.Context, workload *v1alpha1.ManagedWorkload, reason, msgFmt string, args ...any) {
	if err := r.Status().Update(ctx, workload); err != nil {
		log.FromContext(ctx).Error(err, "recording a change in doorman routing",
			"workload", workload.Name, "namespace", workload.Namespace, "reason", reason)
		return
	}
	r.emitEvent(workload, false, "Warning", reason, actionRouteDoorman, msgFmt, args...)
}

// reconcileDoorman makes the workload's doorman EndpointSlices and routes
// match its phase. It returns how soon to look again, or zero when only a
// change the operator watches can alter them.
func (r *Reconciler) reconcileDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (time.Duration, error) {
	if r.DoormanService == "" {
		r.clearCondition(workload, conditionWakeOnRequest, "DoormanDisabled")
		return 0, r.removeDoorman(ctx, workload)
	}
	if !doormanEligible(workload) || target == nil {
		r.clearCondition(workload, conditionWakeOnRequest, "NotPaused")
		return 0, r.removeDoorman(ctx, workload)
	}

	endpoints, err := r.doormanEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	if len(endpoints) == 0 {
		r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, "DoormanUnavailable",
			"no doorman pods are ready, so requests to this paused workload fail until one is")
		return 0, r.removeDoorman(ctx, workload)
	}

	plan, err := r.planDoorman(ctx, workload, target, endpoints)
	if err != nil {
		return 0, err
	}
	var recheck time.Duration
	if len(plan.skipped.served) > 0 {
		recheck = servedRecheck
	}
	current, err := r.listDoormanSlices(ctx, workload)
	if err != nil {
		return 0, err
	}
	routes, err := r.allocateRoutes(ctx, workload, plan.services)
	if errors.Is(err, doorman.ErrNoFreePort) {
		r.setCondition(workload, conditionWakeOnRequest, metav1.ConditionFalse, "NoFreePort", err.Error())
		return recheck, nil
	}
	if err != nil {
		return 0, err
	}
	// Status names the ports before any slice sends traffic to them, so
	// the doorman is never sent traffic for a port it can't place.
	workload.Status.Doorman = routes

	keep := map[string]bool{}
	var errs []error
	for _, s := range plan.services {
		svcRoutes := routesFor(routes, s.svc.Name)
		for _, family := range s.families {
			name := doorman.SliceName(s.svc.Name, workload.Name, family)
			keep[name] = true
			if err := r.applyDoormanSlice(ctx, workload, s.svc, name, family, svcRoutes, endpoints[family]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := r.deleteDoormanSlices(ctx, current, keep); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	r.reportDoorman(ctx, workload, routes, plan.skipped)
	return recheck, nil
}

// removeDoorman stops routing the workload through the doorman: its slices
// are deleted, not left to garbage collection, which an orphaning delete
// would skip, and its ports start draining.
func (r *Reconciler) removeDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	r.doormanPorts.release(workload.UID, r.now())
	workload.Status.Doorman = nil
	current, err := r.listDoormanSlices(ctx, workload)
	if err != nil {
		return err
	}
	return r.deleteDoormanSlices(ctx, current, nil)
}

// skippedServices are the Services that select the workload but aren't
// routed, by why.
type skippedServices struct {
	unsupported []string
	served      []string
	family      []string
}

func (s skippedServices) notes() string {
	var notes []string
	if len(s.served) > 0 {
		notes = append(notes, "served by other Ready pods, so not routed: "+strings.Join(s.served, ", "))
	}
	if len(s.family) > 0 {
		notes = append(notes, "no doorman pod has an address in the IP family of: "+strings.Join(s.family, ", "))
	}
	return strings.Join(notes, "; ")
}

// reportDoorman sets the WakeOnRequest condition. Requests through a Service
// left unrouted because of its load balancer fail while the workload is
// paused, so a warning event says so whenever that list changes.
func (r *Reconciler) reportDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	routes []v1alpha1.DoormanRoute, skipped skippedServices) {
	status, reason, message := metav1.ConditionTrue, "DoormanRouted", "requests are held and wake the workload"
	switch {
	case len(routes) > 0:
	case len(skipped.unsupported) > 0:
		status, reason, message = metav1.ConditionFalse, "UnsupportedLoadBalancer", "no Service can be routed through the doorman"
	case len(skipped.served) > 0:
		status, reason, message = metav1.ConditionFalse, "ServedByOtherPods",
			"every Service that selects the workload has other Ready pods, so requests go to them and don't wake it"
	case len(skipped.family) > 0:
		status, reason, message = metav1.ConditionFalse, "UnsupportedIPFamily",
			"no doorman pod has an address in the IP family of the workload's Services"
	default:
		status, reason, message = metav1.ConditionFalse, "NoServices", "no Service routes TCP traffic to this workload"
	}
	if len(skipped.unsupported) > 0 {
		message = fmt.Sprintf("%s; not routed, since GKE container-native load balancing (NEGs) can't use the doorman: %s",
			message, strings.Join(skipped.unsupported, ", "))
	}
	if notes := skipped.notes(); notes != "" {
		message += "; " + notes
	}
	previous := meta.FindStatusCondition(workload.Status.Conditions, conditionWakeOnRequest)
	r.setCondition(workload, conditionWakeOnRequest, status, reason, message)
	if len(skipped.unsupported) > 0 && (previous == nil || previous.Message != message) {
		r.announceRouting(ctx, workload, "UnsupportedLoadBalancer",
			"requests to Services %s won't wake the workload: GKE container-native load balancing (NEGs) can't use the doorman",
			strings.Join(skipped.unsupported, ", "))
	}
}

// doormanEndpoints returns an endpoint for each Ready doorman pod, by
// address family, from the doorman Service's own EndpointSlices. Each keeps
// its pod's node, zone, and pod reference: load balancers that register pods
// directly skip endpoints without a pod, and traffic policies of Local skip
// those without a node.
func (r *Reconciler) doormanEndpoints(ctx context.Context) (map[discoveryv1.AddressType][]discoveryv1.Endpoint, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list, client.InNamespace(r.DoormanNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: r.DoormanService}); err != nil {
		return nil, fmt.Errorf("listing doorman endpoints: %w", err)
	}
	out := map[discoveryv1.AddressType][]discoveryv1.Endpoint{}
	for _, slice := range list.Items {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
		for _, ep := range slice.Endpoints {
			if len(ep.Addresses) == 0 || (ep.Conditions.Ready != nil && !*ep.Conditions.Ready) {
				continue
			}
			out[slice.AddressType] = append(out[slice.AddressType], discoveryv1.Endpoint{
				Addresses:  ep.Addresses[:1],
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
				NodeName:   ep.NodeName,
				Zone:       ep.Zone,
				TargetRef:  ep.TargetRef,
			})
		}
	}
	for family, eps := range out {
		slices.SortFunc(eps, func(a, b discoveryv1.Endpoint) int { return cmp.Compare(a.Addresses[0], b.Addresses[0]) })
		out[family] = slices.CompactFunc(eps, func(a, b discoveryv1.Endpoint) bool { return a.Addresses[0] == b.Addresses[0] })
	}
	return out, nil
}

// doormanService is a Service to route through the doorman: the address
// families to route it for, and the ports.
type doormanService struct {
	svc      corev1.Service
	families []discoveryv1.AddressType
	ports    []corev1.ServicePort
}

type doormanPlan struct {
	services []doormanService
	skipped  skippedServices
}

// planDoorman picks the Services in the workload's namespace to route
// through the doorman: those that select its pods and have nowhere else to
// send traffic.
//
// Services without a selector manage their own endpoints, and headless and
// ExternalName Services hand callers addresses directly, so none of those
// is routed. A Service with other Ready pods, such as one shared by a
// canary and a stable Deployment of which only one is paused, is left to
// them: requests reach a running workload, and waking the paused one for
// them would be no use. Once every workload behind it is paused, it's
// routed by each, so a request wakes one of them.
func (r *Reconciler) planDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object,
	endpoints map[discoveryv1.AddressType][]discoveryv1.Endpoint) (doormanPlan, error) {
	podLabels := labels.Set(podTemplateLabels(target))
	var list corev1.ServiceList
	if err := r.List(ctx, &list, client.InNamespace(workload.Namespace)); err != nil {
		return doormanPlan{}, fmt.Errorf("listing services: %w", err)
	}
	var plan doormanPlan
	for _, svc := range list.Items {
		if len(svc.Spec.Selector) == 0 || svc.Spec.ClusterIP == corev1.ClusterIPNone ||
			svc.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		if !labels.SelectorFromSet(svc.Spec.Selector).Matches(podLabels) {
			continue
		}
		if usesGKENEG(svc) {
			plan.skipped.unsupported = append(plan.skipped.unsupported, svc.Name)
			continue
		}
		ports := routablePorts(svc)
		if len(ports) == 0 {
			continue
		}
		served, err := r.servedByOthers(ctx, svc)
		if err != nil {
			return doormanPlan{}, err
		}
		if served {
			plan.skipped.served = append(plan.skipped.served, svc.Name)
			continue
		}
		var families []discoveryv1.AddressType
		for _, family := range serviceFamilies(svc) {
			if len(endpoints[family]) > 0 {
				families = append(families, family)
			} else {
				plan.skipped.family = append(plan.skipped.family, fmt.Sprintf("%s (%s)", svc.Name, family))
			}
		}
		if len(families) > 0 {
			plan.services = append(plan.services, doormanService{svc: svc, families: families, ports: ports})
		}
	}
	slices.SortFunc(plan.services, func(a, b doormanService) int { return cmp.Compare(a.svc.Name, b.svc.Name) })
	slices.Sort(plan.skipped.unsupported)
	slices.Sort(plan.skipped.served)
	slices.Sort(plan.skipped.family)
	return plan, nil
}

// servedByOthers reports whether a Service's own EndpointSlices have a
// Ready pod, which can only be another workload's while this one is paused.
// They're read from the API server: the operator caches only the doorman's
// slices, since caching every Service's would cost memory in proportion to
// every pod in the cluster.
func (r *Reconciler) servedByOthers(ctx context.Context, svc corev1.Service) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, endpointsReadTimeout)
	defer cancel()
	var list discoveryv1.EndpointSliceList
	if err := r.apiReader().List(ctx, &list, client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name}); err != nil {
		return false, fmt.Errorf("listing endpoints of service %s: %w", svc.Name, err)
	}
	return doorman.ServesTraffic(list.Items), nil
}

// routablePorts are a Service's TCP ports, less those its
// hybernate.io/doorman-ignore-ports annotation lists by name or number.
// Those fail while the workload is paused, as they would without the
// doorman, which is what a port only health checks or scrapers use wants.
func routablePorts(svc corev1.Service) []corev1.ServicePort {
	ignored := map[string]bool{}
	for p := range strings.SplitSeq(svc.Annotations[doorman.AnnotationIgnorePorts], ",") {
		if p = strings.TrimSpace(p); p != "" {
			ignored[p] = true
		}
	}
	var out []corev1.ServicePort
	for _, port := range svc.Spec.Ports {
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			continue
		}
		if ignored[strconv.Itoa(int(port.Port))] || (port.Name != "" && ignored[port.Name]) {
			continue
		}
		out = append(out, port)
	}
	return out
}

// serviceFamilies are the address families a Service has cluster IPs in,
// and so the EndpointSlices kube-proxy reads for it.
func serviceFamilies(svc corev1.Service) []discoveryv1.AddressType {
	families := svc.Spec.IPFamilies
	if len(families) == 0 {
		families = []corev1.IPFamily{corev1.IPv4Protocol}
		if ip, err := netip.ParseAddr(svc.Spec.ClusterIP); err == nil && ip.Is6() {
			families = []corev1.IPFamily{corev1.IPv6Protocol}
		}
	}
	out := make([]discoveryv1.AddressType, 0, len(families))
	for _, f := range families {
		switch f {
		case corev1.IPv4Protocol:
			out = append(out, discoveryv1.AddressTypeIPv4)
		case corev1.IPv6Protocol:
			out = append(out, discoveryv1.AddressTypeIPv6)
		}
	}
	return out
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

// allocateRoutes gives each Service port a doorman port, keeping the one
// it has in status.
func (r *Reconciler) allocateRoutes(ctx context.Context, workload *v1alpha1.ManagedWorkload, services []doormanService) ([]v1alpha1.DoormanRoute, error) {
	keys := make([]routeKey, 0, len(services))
	for _, s := range services {
		for _, port := range s.ports {
			keys = append(keys, routeKey{service: s.svc.Name, portName: port.Name})
		}
	}
	existing := map[routeKey]int32{}
	for _, route := range workload.Status.Doorman {
		existing[routeKey{service: route.Service, portName: route.PortName}] = route.DoormanPort
	}
	reader := routeReader{reader: r.apiReader(), namespaces: r.WatchNamespaces}
	return r.doormanPorts.assign(ctx, reader, r.now(), workload.UID, existing, keys)
}

// apiReader reads from the API server what the cache leaves out, or may
// not have caught up with.
func (r *Reconciler) apiReader() client.Reader {
	if r.PodReader != nil {
		return r.PodReader
	}
	return r.Client
}

// routeReader lists the ManagedWorkloads the operator can see: in each
// watched namespace, which is all its Roles allow, or in every namespace.
type routeReader struct {
	reader     client.Reader
	namespaces []string
}

func (rr routeReader) list(ctx context.Context) ([]v1alpha1.ManagedWorkload, error) {
	if len(rr.namespaces) == 0 {
		var list v1alpha1.ManagedWorkloadList
		if err := rr.reader.List(ctx, &list); err != nil {
			return nil, fmt.Errorf("listing managed workloads: %w", err)
		}
		return list.Items, nil
	}
	var all []v1alpha1.ManagedWorkload
	for _, ns := range rr.namespaces {
		var list v1alpha1.ManagedWorkloadList
		if err := rr.reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return nil, fmt.Errorf("listing managed workloads in %s: %w", ns, err)
		}
		all = append(all, list.Items...)
	}
	return all, nil
}

type routeKey struct {
	service, portName string
}

type portClaim struct {
	owner types.UID
	// at is when the claim was last made, or for a draining port, when
	// the drain ends.
	at time.Time
}

// portAllocator hands out doorman ports. One doorman serves the whole
// cluster, so two routes on one port would send one workload's traffic to
// another. Reconciles run in parallel, and a status written moments ago may
// not be in the cache yet, so ports aren't chosen from the cache: the
// allocator, which only the leader runs, remembers every claim it makes,
// starting from, and every so often reconciling with, a read of every
// workload's routes from the API server.
type portAllocator struct {
	mu       sync.Mutex
	claims   map[int32]portClaim
	draining map[int32]portClaim
	loadedAt time.Time

	// pick chooses a free port. Tests make it predictable.
	pick func(used func(int32) bool) (int32, error)
}

// assign gives each key a port, keeping the existing ports where they're
// still the owner's, and claims them for it. Ports it no longer needs start
// draining.
func (a *portAllocator) assign(ctx context.Context, reader routeReader, now time.Time,
	owner types.UID, existing map[routeKey]int32, keys []routeKey) ([]v1alpha1.DoormanRoute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(ctx, reader, now); err != nil {
		return nil, err
	}
	for port, d := range a.draining {
		if !now.Before(d.at) {
			delete(a.draining, port)
		}
	}

	chosen := map[int32]bool{}
	used := func(port int32) bool { return chosen[port] || !a.free(port, owner) }
	pick := a.pick
	if pick == nil {
		pick = doorman.AllocatePort
	}
	routes := make([]v1alpha1.DoormanRoute, 0, len(keys))
	for _, key := range keys {
		port, ok := existing[key]
		if !ok || !doorman.InRange(port) || used(port) {
			var err error
			if port, err = pick(used); err != nil {
				return nil, err
			}
		}
		chosen[port] = true
		routes = append(routes, v1alpha1.DoormanRoute{Service: key.service, PortName: key.portName, DoormanPort: port})
	}

	a.releaseLocked(owner, now, chosen)
	for port := range chosen {
		a.claims[port] = portClaim{owner: owner, at: now}
		delete(a.draining, port)
	}
	return routes, nil
}

func (a *portAllocator) free(port int32, owner types.UID) bool {
	if c, ok := a.claims[port]; ok && c.owner != owner {
		return false
	}
	if d, ok := a.draining[port]; ok && d.owner != owner {
		return false
	}
	return true
}

// release lets go of every port the workload holds. They drain before
// another workload may have them.
func (a *portAllocator) release(owner types.UID, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseLocked(owner, now, nil)
}

func (a *portAllocator) releaseLocked(owner types.UID, now time.Time, keep map[int32]bool) {
	for port, c := range a.claims {
		if c.owner == owner && !keep[port] {
			delete(a.claims, port)
			if a.draining == nil {
				a.draining = map[int32]portClaim{}
			}
			a.draining[port] = portClaim{owner: owner, at: now.Add(portDrain)}
		}
	}
}

// load reads every workload's routes from the API server, the first time
// and every portsReload after. Claims made since the last load are kept,
// since their status may not be written yet.
func (a *portAllocator) load(ctx context.Context, reader routeReader, now time.Time) error {
	if a.claims != nil && now.Sub(a.loadedAt) < portsReload {
		return nil
	}
	workloads, err := reader.list(ctx)
	if err != nil {
		return fmt.Errorf("reading doorman ports in use: %w", err)
	}
	claims := map[int32]portClaim{}
	for _, w := range workloads {
		for _, route := range w.Status.Doorman {
			if _, taken := claims[route.DoormanPort]; !taken {
				claims[route.DoormanPort] = portClaim{owner: w.UID, at: now}
			}
		}
	}
	for port, c := range a.claims {
		if _, ok := claims[port]; !ok && c.at.After(a.loadedAt) {
			claims[port] = c
		}
	}
	a.claims, a.loadedAt = claims, now
	if a.draining == nil {
		a.draining = map[int32]portClaim{}
	}
	return nil
}

// failureBackoff counts each workload's doorman failures in a row.
type failureBackoff struct {
	mu       sync.Mutex
	failures map[types.UID]int
}

// next records a failure and returns how long to wait before retrying.
func (b *failureBackoff) next(uid types.UID) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures == nil {
		b.failures = map[types.UID]int{}
	}
	n := b.failures[uid]
	b.failures[uid] = n + 1
	return min(doormanRetryMin<<min(n, 10), doormanRetryMax)
}

func (b *failureBackoff) reset(uid types.UID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, uid)
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
// address family of a Service at the doorman. Each port keeps its Service
// port name, which is how kube-proxy matches a slice port to a Service port,
// so traffic to the Service port reaches the doorman port allocated for it.
func (r *Reconciler) applyDoormanSlice(ctx context.Context, workload *v1alpha1.ManagedWorkload, svc corev1.Service,
	name string, family discoveryv1.AddressType, routes []v1alpha1.DoormanRoute, endpoints []discoveryv1.Endpoint) error {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workload.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, slice, func() error {
		if slice.ResourceVersion != "" && slice.Labels[discoveryv1.LabelManagedBy] != doorman.ManagedBy {
			return fmt.Errorf("endpointslice %s exists and isn't the doorman's", slice.Name)
		}
		slice.Labels = map[string]string{
			discoveryv1.LabelServiceName: svc.Name,
			discoveryv1.LabelManagedBy:   doorman.ManagedBy,
			doorman.LabelManagedWorkload: doorman.WorkloadLabel(workload.Name),
		}
		// Proxies use this to pick the Services they serve, so the doorman's
		// slice must carry it whenever the Service's own slices do.
		if proxy, ok := svc.Labels[labelServiceProxyName]; ok {
			slice.Labels[labelServiceProxyName] = proxy
		}
		slice.AddressType = family
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

// listDoormanSlices lists the doorman slices the workload has written.
func (r *Reconciler) listDoormanSlices(ctx context.Context, workload *v1alpha1.ManagedWorkload) ([]discoveryv1.EndpointSlice, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list, client.InNamespace(workload.Namespace), client.MatchingLabels{
		discoveryv1.LabelManagedBy:   doorman.ManagedBy,
		doorman.LabelManagedWorkload: doorman.WorkloadLabel(workload.Name),
	}); err != nil {
		return nil, fmt.Errorf("listing doorman slices: %w", err)
	}
	return list.Items, nil
}

// deleteDoormanSlices removes the slices except those in keep, so a
// workload that's awake again routes only to its own pods.
func (r *Reconciler) deleteDoormanSlices(ctx context.Context, slices []discoveryv1.EndpointSlice, keep map[string]bool) error {
	for i := range slices {
		slice := &slices[i]
		if keep[slice.Name] {
			continue
		}
		if err := r.Delete(ctx, slice); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting doorman slice %s: %w", slice.Name, err)
		}
	}
	return nil
}

// EndpointSliceCache is the operator's cache of EndpointSlices: the doorman
// slices it writes into workloads' namespaces, the watched ones or all, and
// every slice in doormanNamespace, where the doorman Service's own are.
// Empty doormanNamespace means the doorman is disabled. Every other slice,
// which on a large cluster is most of them, is left out: a Service's own
// slices are read from the API server when a paused workload is routed.
func EndpointSliceCache(watched []string, doormanNamespace string) cache.ByObject {
	doormanSlices := cache.Config{
		LabelSelector: labels.SelectorFromSet(labels.Set{discoveryv1.LabelManagedBy: doorman.ManagedBy}),
	}
	namespaces := map[string]cache.Config{}
	if len(watched) == 0 {
		namespaces[cache.AllNamespaces] = doormanSlices
	}
	for _, ns := range watched {
		namespaces[ns] = doormanSlices
	}
	if doormanNamespace != "" {
		namespaces[doormanNamespace] = cache.Config{LabelSelector: labels.Everything()}
	}
	return cache.ByObject{Namespaces: namespaces}
}

// isDoormanEndpoints reports whether obj is one of the doorman Service's own
// EndpointSlices. Of the slices the operator caches, only those say anything
// about where a paused workload's Services should send traffic.
func (r *Reconciler) isDoormanEndpoints(obj client.Object) bool {
	return obj.GetNamespace() == r.DoormanNamespace && obj.GetLabels()[discoveryv1.LabelServiceName] == r.DoormanService
}

// findWorkloadsForDoorman re-queues every paused or resuming workload when
// the doorman's own endpoints change, so their slices follow the doorman
// pods as they come and go.
func (r *Reconciler) findWorkloadsForDoorman(ctx context.Context, _ client.Object) []reconcile.Request {
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

// findWorkloadsInNamespace requeues every ManagedWorkload in a namespace
// whose labels changed, such as being protected.
func (r *Reconciler) findWorkloadsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetName())); err != nil {
		log.FromContext(ctx).Error(err, "listing managed workloads for namespace change", "namespace", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, w := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&w)})
	}
	return requests
}
