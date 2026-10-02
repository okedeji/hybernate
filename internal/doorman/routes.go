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
// arrives on identifies the workload it's for.
package doorman

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

const (
	// ManagedBy marks the EndpointSlices Hybernate adds. The EndpointSlice
	// controller only manages slices labeled with its own value, so these
	// coexist with the Service's real slice.
	ManagedBy = "doorman.hybernate.io"

	// LabelManagedWorkload names the ManagedWorkload a doorman slice serves.
	LabelManagedWorkload = "hybernate.io/managed-workload"

	// Ports are allocated from this range. It sits below the NodePort range
	// and above common application ports.
	minPort = 20000
	maxPort = 29999
)

// ErrNoFreePort means every doorman port is allocated.
var ErrNoFreePort = errors.New("no free doorman port")

// AllocatePort picks a doorman port for a Service port. It starts from a
// hash of the port's identity, so a port keeps the same number across
// pauses when it can, and probes upward past ports already in use.
func AllocatePort(namespace, service, portName string, used map[int32]bool) (int32, error) {
	sum := sha256.Sum256([]byte(namespace + "/" + service + "/" + portName))
	span := uint32(maxPort - minPort + 1)
	start := binary.BigEndian.Uint32(sum[:4]) % span
	for i := range span {
		port := int32(minPort + (start+i)%span)
		if !used[port] {
			return port, nil
		}
	}
	return 0, ErrNoFreePort
}

// SliceName is the name of the doorman EndpointSlice for a Service. It
// starts with the Service name, as the EndpointSlice controller's slices do,
// because ingress-nginx only reads a Service's slices by that prefix: up to
// 57 characters of it, which is as much as fits in a generated name.
//
// Names are limited to 63 characters, so a long Service name is cut to those
// 57 and given a hash. The hash starts with "o", a letter the EndpointSlice
// controller's random suffixes never use, so the name can't be one of its.
func SliceName(service string) string {
	const suffix = "-hybernate-doorman"
	if len(service)+len(suffix) <= 63 {
		return service + suffix
	}
	sum := sha256.Sum256([]byte(service))
	return service[:min(len(service), 57)] + "-o" + hex.EncodeToString(sum[:2])
}
