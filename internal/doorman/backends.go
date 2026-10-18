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

package doorman

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodEndpoint is a Ready endpoint of a Service: a pod, by name, and the
// address its EndpointSlice gives for it.
type PodEndpoint struct {
	Namespace string
	Pod       string
	Addr      netip.Addr
}

// ReadyPodEndpoints are the Ready pod endpoints of a Service's trusted
// EndpointSlices: where the doorman would pass a connection once each is
// confirmed to be a pod that has its address, since a slice's label is only
// a claim.
func ReadyPodEndpoints(endpointSlices []discoveryv1.EndpointSlice) []PodEndpoint {
	var endpoints []PodEndpoint
	for i := range endpointSlices {
		if !trusted(&endpointSlices[i]) {
			continue
		}
		for _, ep := range endpointSlices[i].Endpoints {
			if addr, ok := readyAddress(ep); ok {
				endpoints = append(endpoints, PodEndpoint{Namespace: ep.TargetRef.Namespace, Pod: ep.TargetRef.Name, Addr: addr})
			}
		}
	}
	return endpoints
}

// trusted reports whether a slice says it was written by the EndpointSlice
// controller for a Service's selector, rather than by hand or mirrored from
// an Endpoints object, which can name any address. The label is only a
// claim, which anyone allowed to write EndpointSlices in the namespace can
// make, so the addresses are checked as well (see routable and backends).
func trusted(slice *discoveryv1.EndpointSlice) bool {
	if slice.Labels[discoveryv1.LabelManagedBy] != endpointSliceController {
		return false
	}
	return slice.AddressType == discoveryv1.AddressTypeIPv4 || slice.AddressType == discoveryv1.AddressTypeIPv6
}

// readyAddress returns the address of a Ready pod endpoint. A nil Ready
// condition means unknown, which the API asks consumers to treat as ready.
func readyAddress(ep discoveryv1.Endpoint) (netip.Addr, bool) {
	if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
		return netip.Addr{}, false
	}
	if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
		return netip.Addr{}, false
	}
	if ep.TargetRef == nil || ep.TargetRef.Kind != "Pod" || len(ep.Addresses) == 0 {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(ep.Addresses[0])
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// cloudMetadata are the cloud metadata services outside link-local space:
// AWS's IPv6 instance services, at fd00:ec2::254 and its neighbours, and
// Alibaba Cloud's. The rest of unique-local and shared (100.64.0.0/10)
// space stays routable, since pod networks are allocated from both.
var cloudMetadata = []netip.Prefix{
	netip.MustParsePrefix("fd00:ec2::/32"),
	netip.MustParsePrefix("100.100.100.200/32"),
}

// routable reports whether addr could be a pod's. Loopback would reach the
// doorman's own pod, and link-local or cloudMetadata a metadata service,
// which a forged EndpointSlice could otherwise send the doorman to.
func routable(addr netip.Addr) bool {
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, p := range cloudMetadata {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

func slicePort(slice *discoveryv1.EndpointSlice, name string) (int32, bool) {
	for _, p := range slice.Ports {
		if p.Port == nil {
			continue
		}
		if (p.Name == nil && name == "") || (p.Name != nil && *p.Name == name) {
			return *p.Port, true
		}
	}
	return 0, false
}

// serviceRef names a Service.
type serviceRef struct {
	namespace, name string
}

// backendKey identifies the backends of one Service port.
type backendKey struct {
	service  serviceRef
	portName string
}

// backendSet is the cached answer for one Service port, and a channel closed
// when it may have changed.
type backendSet struct {
	addrs   []string
	fresh   bool
	changed chan struct{}
}

func (s *backendSet) invalidate() {
	s.fresh = false
	close(s.changed)
	s.changed = make(chan struct{})
}

const (
	// podAddressesTTL is how long a pod's addresses are trusted once read.
	// A pod recreated under the same name with a new address is read again
	// at once, but no more than every podRecheck.
	podAddressesTTL = 30 * time.Second
	podRecheck      = time.Second

	podReadTimeout = 2 * time.Second
)

// backends shares the Ready addresses of each Service port among every
// connection held for it. The addresses are listed once per change, however
// many connections wait, and the waiters are woken together when the
// Service's EndpointSlices change.
//
// The EndpointSlice controller's label on a slice can be forged by anyone
// who may write EndpointSlices in its namespace, so each endpoint must also
// name a pod in the Service's namespace that has its address. Otherwise a
// forged slice could send the doorman to any pod in the cluster, past the
// NetworkPolicies that keep its namespace's users from reaching it.
type backends struct {
	client  client.Reader
	pods    client.Reader
	allowed func(netip.Addr) bool
	now     func() time.Time

	mu      sync.Mutex
	sets    map[serviceRef]map[string]*backendSet
	podAddr map[types.NamespacedName]podAddresses
}

// podAddresses are a pod's addresses, and when they were read.
type podAddresses struct {
	addrs []netip.Addr
	at    time.Time
}

func newBackends(c, pods client.Reader) *backends {
	return &backends{
		client:  c,
		pods:    pods,
		allowed: routable,
		now:     time.Now,
		sets:    map[serviceRef]map[string]*backendSet{},
		podAddr: map[types.NamespacedName]podAddresses{},
	}
}

// get returns the Ready addresses for key, as host:port, and a channel that
// is closed when they may have changed.
func (b *backends) get(ctx context.Context, key backendKey) ([]string, <-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ports, ok := b.sets[key.service]
	if !ok {
		ports = map[string]*backendSet{}
		b.sets[key.service] = ports
	}
	set, ok := ports[key.portName]
	if !ok {
		set = &backendSet{changed: make(chan struct{})}
		ports[key.portName] = set
	}
	if !set.fresh {
		addrs, err := b.list(ctx, key)
		if err != nil {
			return nil, set.changed, err
		}
		set.addrs, set.fresh = addrs, true
	}
	return set.addrs, set.changed, nil
}

func (b *backends) list(ctx context.Context, key backendKey) ([]string, error) {
	var list discoveryv1.EndpointSliceList
	if err := b.client.List(ctx, &list, client.InNamespace(key.service.namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: key.service.name}); err != nil {
		return nil, fmt.Errorf("listing endpoints for service %s: %w", key.service.name, err)
	}
	seen := map[string]bool{}
	var addrs []string
	for i := range list.Items {
		slice := &list.Items[i]
		if !trusted(slice) {
			continue
		}
		port, ok := slicePort(slice, key.portName)
		if !ok {
			continue
		}
		for _, ep := range slice.Endpoints {
			addr, ok := readyAddress(ep)
			if !ok || !b.allowed(addr) {
				continue
			}
			isPod, err := b.isPod(ctx, key.service.namespace, ep.TargetRef, addr)
			if err != nil {
				return nil, err
			}
			if !isPod {
				continue
			}
			hostPort := net.JoinHostPort(addr.String(), strconv.Itoa(int(port)))
			if !seen[hostPort] {
				seen[hostPort] = true
				addrs = append(addrs, hostPort)
			}
		}
	}
	return addrs, nil
}

// isPod reports whether ref names a pod in namespace that has addr. b.mu
// must be held.
func (b *backends) isPod(ctx context.Context, namespace string, ref *corev1.ObjectReference, addr netip.Addr) (bool, error) {
	if ref.Namespace != namespace || ref.Name == "" {
		return false, nil
	}
	name := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	now := b.now()
	known, ok := b.podAddr[name]
	if ok && slices.Contains(known.addrs, addr) && now.Sub(known.at) < podAddressesTTL {
		return true, nil
	}
	if ok && now.Sub(known.at) < podRecheck {
		return false, nil
	}
	addrs, err := b.readPodAddresses(ctx, name)
	if err != nil {
		return false, err
	}
	b.podAddr[name] = podAddresses{addrs: addrs, at: now}
	return slices.Contains(addrs, addr), nil
}

// readPodAddresses reads a pod's addresses from the API server, or none for
// a pod that doesn't exist.
func (b *backends) readPodAddresses(ctx context.Context, name types.NamespacedName) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, podReadTimeout)
	defer cancel()
	var pod corev1.Pod
	if err := b.pods.Get(ctx, name, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading pod %s: %w", name, err)
	}
	addrs := make([]netip.Addr, 0, len(pod.Status.PodIPs))
	for _, ip := range pod.Status.PodIPs {
		if addr, err := netip.ParseAddr(ip.IP); err == nil {
			addrs = append(addrs, addr.Unmap())
		}
	}
	return addrs, nil
}

// changed marks a Service's backends stale and wakes everything waiting on
// them.
func (b *backends) changed(svc serviceRef) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, set := range b.sets[svc] {
		set.invalidate()
	}
}

func (b *backends) changedAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ports := range b.sets {
		for _, set := range ports {
			set.invalidate()
		}
	}
}

// retain forgets the Services no route uses any more, waking anything still
// waiting on them, and the pod addresses past their TTL.
func (b *backends) retain(keep map[serviceRef]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for name, known := range b.podAddr {
		if now.Sub(known.at) >= podAddressesTTL {
			delete(b.podAddr, name)
		}
	}
	for svc, ports := range b.sets {
		if keep[svc] {
			continue
		}
		for _, set := range ports {
			close(set.changed)
		}
		delete(b.sets, svc)
	}
}
