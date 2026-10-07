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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestAllocatePort(t *testing.T) {
	used := map[int32]bool{}
	for range 200 {
		port, err := AllocatePort(func(p int32) bool { return used[p] })
		require.NoError(t, err)
		assert.True(t, InRange(port))
		assert.False(t, used[port], "a port in use is never handed out again")
		used[port] = true
	}
}

// A port that can be worked out from a workload's names can be aimed at
// without scanning.
func TestAllocatePort_IsNotDerivedFromNames(t *testing.T) {
	seen := map[int32]bool{}
	for range 20 {
		port, err := AllocatePort(func(int32) bool { return false })
		require.NoError(t, err)
		seen[port] = true
	}
	assert.Greater(t, len(seen), 1)
}

func TestAllocatePort_Exhausted(t *testing.T) {
	_, err := AllocatePort(func(int32) bool { return true })

	assert.ErrorIs(t, err, ErrNoFreePort)
}

func TestSliceName(t *testing.T) {
	assert.True(t, strings.HasPrefix(SliceName("api", "stable", discoveryv1.AddressTypeIPv4), "api-o"))
	assert.NotEqual(t, SliceName("api", "stable", discoveryv1.AddressTypeIPv4), SliceName("api", "canary", discoveryv1.AddressTypeIPv4),
		"two workloads behind one Service each get a slice")
	assert.NotEqual(t, SliceName("api", "stable", discoveryv1.AddressTypeIPv4), SliceName("api", "stable", discoveryv1.AddressTypeIPv6),
		"and one per address family")
	assert.Equal(t, SliceName("api", "stable", discoveryv1.AddressTypeIPv4), SliceName("api", "stable", discoveryv1.AddressTypeIPv4))

	for _, n := range []int{1, 45, 46, 51, 52, 57, 58, 63} {
		service := strings.Repeat("a", n)
		name := SliceName(service, strings.Repeat("w", 253), discoveryv1.AddressTypeIPv6)
		assert.LessOrEqual(t, len(name), 63)
		assert.Empty(t, validation.IsDNS1123Subdomain(name))
		assert.True(t, strings.HasPrefix(name, service[:min(n, 57)]),
			"ingress-nginx finds a Service's slices by up to 57 characters of its name: %d", n)
	}
}

func TestWorkloadLabel(t *testing.T) {
	assert.Equal(t, "api", WorkloadLabel("api"))

	long := strings.Repeat("a", 200)
	assert.Empty(t, validation.IsValidLabelValue(WorkloadLabel(long)))
	assert.NotEqual(t, WorkloadLabel(long), WorkloadLabel(long[:199]+"b"))
}

// The fake client doesn't validate objects, so this is the check that stands
// in for the API server rejecting a doorman slice.
func TestSliceMetadataIsValid(t *testing.T) {
	assert.Empty(t, validation.IsValidLabelValue(ManagedBy))
	assert.Empty(t, validation.IsQualifiedName(LabelManagedWorkload))
}
