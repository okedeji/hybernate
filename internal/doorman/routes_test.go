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
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestAllocatePort(t *testing.T) {
	first, err := AllocatePort("sandbox", "api", "http", nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, first, int32(minPort))
	assert.LessOrEqual(t, first, int32(maxPort))

	again, err := AllocatePort("sandbox", "api", "http", nil)
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same Service port gets the same doorman port when it's free")

	next, err := AllocatePort("sandbox", "api", "http", map[int32]bool{first: true})
	require.NoError(t, err)
	assert.NotEqual(t, first, next, "a port in use is skipped")
}

func TestAllocatePort_Exhausted(t *testing.T) {
	used := map[int32]bool{}
	for p := int32(minPort); p <= maxPort; p++ {
		used[p] = true
	}

	_, err := AllocatePort("sandbox", "api", "http", used)

	assert.ErrorIs(t, err, ErrNoFreePort)
}

func TestSliceName(t *testing.T) {
	assert.Equal(t, "api-hybernate-doorman", SliceName("api"))

	for _, n := range []int{45, 46, 57, 58, 63} {
		service := strings.Repeat("a", n)
		name := SliceName(service)
		assert.LessOrEqual(t, len(name), 63)
		assert.True(t, strings.HasPrefix(name, service[:min(n, 57)]),
			"ingress-nginx finds a Service's slices by up to 57 characters of its name: %d", n)
	}

	long := strings.Repeat("a", 63)
	assert.Equal(t, SliceName(long), SliceName(long), "stable for the same Service")
	assert.NotEqual(t, SliceName(long), SliceName(long[:62]+"b"), "distinct for Services sharing a long prefix")
}

// The fake client doesn't validate objects, so this is the check that stands
// in for the API server rejecting a doorman slice.
func TestSliceMetadataIsValid(t *testing.T) {
	assert.Empty(t, validation.IsValidLabelValue(ManagedBy))
	assert.Empty(t, validation.IsQualifiedName(LabelManagedWorkload))
	assert.Empty(t, validation.IsDNS1123Subdomain(SliceName(strings.Repeat("a", 63))))
}
