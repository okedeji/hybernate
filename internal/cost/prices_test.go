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

package cost

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each provider's labels find the node's list price. The bounds hold
// across price changes; the exact figures come from the generated table.
func TestListPriceOf(t *testing.T) {
	tests := []struct {
		name             string
		labels           map[string]string
		provider         string
		vcpu, memoryGiB  float64
		minHour, maxHour float64
	}{
		{name: "EKS", provider: "aws", vcpu: 2, memoryGiB: 8, minHour: 0.05, maxHour: 0.2,
			labels: nodeLabels("m6i.large", "us-east-1")},
		{name: "GKE", provider: "gcp", vcpu: 4, memoryGiB: 16, minHour: 0.1, maxHour: 0.4,
			labels: nodeLabels("n2-standard-4", "us-central1")},
		{name: "AKS, by ARM size name", provider: "azure", vcpu: 4, memoryGiB: 16, minHour: 0.1, maxHour: 0.4,
			labels: nodeLabels("Standard_D4s_v5", "eastus")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := ListPriceOf(NodeTypeOf(tt.labels))

			require.True(t, ok)
			assert.Equal(t, tt.provider, p.Provider)
			hourly := ComputeHourly(tt.vcpu, tt.memoryGiB, p.Rates)
			assert.Greater(t, hourly, tt.minHour)
			assert.Less(t, hourly, tt.maxHour)
		})
	}
}

func TestListPriceOf_NotListed(t *testing.T) {
	tests := map[string]map[string]string{
		"unknown type":         nodeLabels("custom.metal", "us-east-1"),
		"region it isn't sold": nodeLabels("m6i.large", "mars-north-1"),
		"no labels":            {},
		"kind node":            {"kubernetes.io/hostname": "kind-control-plane"},
	}
	for name, labels := range tests {
		t.Run(name, func(t *testing.T) {
			_, ok := ListPriceOf(NodeTypeOf(labels))
			assert.False(t, ok)
		})
	}
}

// The instance's whole price is split between its vCPUs and memory, so the
// rates price the instance itself at exactly its list price.
func TestSplitHourly(t *testing.T) {
	for _, shape := range []struct{ vcpu, memoryGiB float64 }{{2, 8}, {2, 16}, {8, 16}, {96, 768}} {
		r := splitHourly(0.5, shape.vcpu, shape.memoryGiB)

		assert.InDelta(t, 0.5, ComputeHourly(shape.vcpu, shape.memoryGiB, r), 1e-9)
		assert.InDelta(t, cpuToMemory, r.CPUPerHour/r.MemoryPerHour, 1e-9, "CPU and memory keep the default proportion")
		assert.Equal(t, DefaultRates.StoragePerMonth, r.StoragePerMonth)
	}
}

func TestNodeTypeOf_Spot(t *testing.T) {
	tests := map[string]struct {
		key, value string
		spot       bool
	}{
		"Karpenter":            {"karpenter.sh/capacity-type", "spot", true},
		"Karpenter, on-demand": {"karpenter.sh/capacity-type", "on-demand", false},
		"EKS node group":       {"eks.amazonaws.com/capacityType", "SPOT", true},
		"GKE spot":             {"cloud.google.com/gke-spot", "true", true},
		"GKE preemptible":      {"cloud.google.com/gke-preemptible", "true", true},
		"AKS":                  {"kubernetes.azure.com/scalesetpriority", "spot", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			labels := nodeLabels("m6i.large", "us-east-1")
			labels[tt.key] = tt.value
			assert.Equal(t, tt.spot, NodeTypeOf(labels).Spot)
		})
	}
}

// Every line of the generated table parses, so a bad regeneration fails
// here rather than leaving nodes priced at the defaults.
func TestPriceTableParses(t *testing.T) {
	gz, err := gzip.NewReader(bytes.NewReader(pricesFile))
	require.NoError(t, err)
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	providers := map[string]int{}
	for line := 1; sc.Scan(); line++ {
		if strings.HasPrefix(sc.Text(), "#") {
			continue
		}
		fields := strings.Split(sc.Text(), ",")
		require.GreaterOrEqual(t, len(fields), 5, "line %d", line)
		for _, field := range fields[4:] {
			_, price, ok := strings.Cut(field, "=")
			require.True(t, ok, "line %d: %q", line, field)
			p, err := parseListing(fields, price, line)
			require.NoError(t, err)
			require.Positive(t, p.Rates.CPUPerHour, "line %d", line)
		}
		providers[fields[0]]++
	}
	require.NoError(t, sc.Err())
	for _, provider := range []string{"aws", "azure", "gcp"} {
		assert.Greater(t, providers[provider], 100, "%s instance types", provider)
	}
}

func TestMean(t *testing.T) {
	m := Mean([]Rates{{CPUPerHour: 0.02, MemoryPerHour: 0.002}, {CPUPerHour: 0.04, MemoryPerHour: 0.006}})

	assert.InDelta(t, 0.03, m.CPUPerHour, 1e-9)
	assert.InDelta(t, 0.004, m.MemoryPerHour, 1e-9)
	assert.Equal(t, Rates{}, Mean(nil))
}

func nodeLabels(instanceType, region string) map[string]string {
	return map[string]string{
		"node.kubernetes.io/instance-type": instanceType,
		"topology.kubernetes.io/region":    region,
	}
}
