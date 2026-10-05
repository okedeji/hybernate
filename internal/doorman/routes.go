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

// Package doorman holds connections to paused workloads, wakes them, and
// passes the connections through once they're Ready.
//
// While a workload is paused, the operator adds an EndpointSlice to each of
// its Services that points at the doorman instead of the workload's pods.
// Each Service port gets its own doorman port, so the port a connection
// arrives on identifies the workload it's for. A Service is routed only
// while none of the pods it selects can serve: a Service shared by two
// workloads, such as canary and stable, keeps sending traffic to whichever
// is still running.
//
// # What wakes a workload
//
// A connection wakes the workload only once the caller shows it's a real
// client, so that probes and scrapes don't undo the pause:
//
//   - A connection that is opened and closed without sending anything, as a
//     TCP health check does, is closed without a wake.
//   - A caller that sends bytes wakes the workload. One that stays connected
//     without sending anything for silentWake is taken to speak a protocol
//     where the server talks first, such as MySQL, and wakes it too.
//   - An HTTP request whose User-Agent belongs to a known health checker or
//     metrics scraper (see healthCheckAgents) is answered 503 and closed
//     without a wake.
//   - Service ports listed in the hybernate.io/doorman-ignore-ports
//     annotation, by name or number, are never routed to the doorman, so
//     they fail as they would without it. Use it for metrics ports scraped
//     over TLS and for health-checked ports whose checker isn't recognised.
//
// # Security
//
// Any client that can open a connection to a doorman pod can reach any
// workload the doorman routes, in any namespace, on the ports its Services
// expose: a workload's own NetworkPolicy sees the connection come from the
// doorman. Restrict who may connect to the doorman pods with a NetworkPolicy
// on the doorman itself, and where they may connect with an egress policy.
//
// The doorman proxies only to Ready pod addresses in the routed Service's
// EndpointSlices that are labelled as the EndpointSlice controller's, never
// to a hostname, a loopback or a link-local address. Anyone who can write
// EndpointSlices in a namespace can forge that label, so an egress policy
// limiting the doorman to pod addresses is what makes it unable to reach
// anything else. It caps held connections, overall and per source address,
// and how often one source can wake workloads, which slows a scan of the
// doorman's ports but doesn't stop one.
package doorman

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
)

const (
	// ManagedBy marks the EndpointSlices Hybernate adds. The EndpointSlice
	// controller only manages slices labeled with its own value, so these
	// coexist with the Service's real slice.
	ManagedBy = "doorman.hybernate.io"

	// LabelManagedWorkload names the ManagedWorkload a doorman slice serves,
	// as WorkloadLabel gives it.
	LabelManagedWorkload = "hybernate.io/managed-workload"

	// AnnotationIgnorePorts on a Service lists, comma-separated, the names or
	// numbers of the Service ports the doorman must not stand in for.
	AnnotationIgnorePorts = "hybernate.io/doorman-ignore-ports"

	// RouteDrain is how long the doorman keeps serving a port after its
	// route leaves status. Proxies and kube-proxy take a moment to stop
	// sending there; without it, a browser's last refresh of the waking-up
	// page would land on a closed port and show the proxy's error page,
	// which doesn't refresh.
	RouteDrain = 30 * time.Second

	// Ports are allocated from this range. It sits below the NodePort range
	// and above common application ports.
	minPort = 20000
	maxPort = 29999
)

// endpointSliceController is the managed-by value of the slices Kubernetes
// writes for a Service with a selector. Only those are trusted as backends.
const endpointSliceController = "endpointslice-controller.k8s.io"

// ErrNoFreePort means every doorman port is allocated.
var ErrNoFreePort = errors.New("no free doorman port")

// AllocatePort picks a free doorman port, starting the search at a random
// port. A random start doesn't hide routed ports from a scan, which is what
// a NetworkPolicy on the doorman is for, but it keeps a workload's port from
// being worked out from its namespace and Service name.
func AllocatePort(used func(int32) bool) (int32, error) {
	const span = maxPort - minPort + 1
	n, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return 0, err
	}
	start := int32(n.Int64())
	for i := range int32(span) {
		port := minPort + (start+i)%span
		if !used(port) {
			return port, nil
		}
	}
	return 0, ErrNoFreePort
}

// InRange reports whether port is one the doorman allocates.
func InRange(port int32) bool {
	return port >= minPort && port <= maxPort
}

// SliceName is the name of the doorman EndpointSlice that stands in for one
// address family of a Service while a workload is paused. Two workloads
// behind one Service each get their own.
//
// It starts with the Service name, as the EndpointSlice controller's slices
// do, because ingress-nginx only reads a Service's slices by that prefix: up
// to 57 characters of it, which is as much as fits in a generated name. The
// rest of the 63 characters is a hash of the Service, workload, and family.
// The hash follows "-o", a letter the EndpointSlice controller's random
// suffixes never use, so the name can't be one of its.
func SliceName(service, workload string, family discoveryv1.AddressType) string {
	sum := sha256.Sum256([]byte(service + "/" + workload + "/" + string(family)))
	prefix := service[:min(len(service), 57)]
	hashLen := min(63-len(prefix)-2, 10)
	return prefix + "-o" + hex.EncodeToString(sum[:])[:hashLen]
}

// WorkloadLabel is the LabelManagedWorkload value for a ManagedWorkload: its
// name, or for a name too long for a label, a prefix of it and a hash.
func WorkloadLabel(name string) string {
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:52], "-.") + "-" + hex.EncodeToString(sum[:5])
}
