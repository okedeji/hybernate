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
	"strconv"
	"sync"

	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServesTraffic reports whether any of a Service's EndpointSlices has a
// Ready pod the doorman would pass a connection to. While one does, the
// Service has somewhere to send traffic, so it isn't routed to the doorman.
func ServesTraffic(slices []discoveryv1.EndpointSlice) bool {
	for i := range slices {
		if trusted(&slices[i]) {
			for _, ep := range slices[i].Endpoints {
				if _, ok := readyAddress(ep); ok {
					return true
				}
			}
		}
	}
	return false
}

// trusted reports whether a slice says it was written by the EndpointSlice
// controller for a Service's selector, rather than by hand or mirrored from
// an Endpoints object, which can name any address. The label is only a
// claim, which anyone allowed to write EndpointSlices in the namespace can
// make, so the addresses are checked as well (see routable).
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

// backends shares the Ready addresses of each Service port among every
// connection held for it. The addresses are listed once per change, however
// many connections wait, and the waiters are woken together when the
// Service's EndpointSlices change.
type backends struct {
	client  client.Reader
	allowed func(netip.Addr) bool

	mu   sync.Mutex
	sets map[serviceRef]map[string]*backendSet
}

func newBackends(c client.Reader) *backends {
	return &backends{client: c, allowed: routable, sets: map[serviceRef]map[string]*backendSet{}}
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
			hostPort := net.JoinHostPort(addr.String(), strconv.Itoa(int(port)))
			if !seen[hostPort] {
				seen[hostPort] = true
				addrs = append(addrs, hostPort)
			}
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
// waiting on them.
func (b *backends) retain(keep map[serviceRef]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
