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

package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

func TestClassify(t *testing.T) {
	th := DefaultThresholds()

	tests := []struct {
		name     string
		workload WorkloadInfo
		want     v1alpha1.Classification
	}{
		{
			name: "idle when CPU below threshold",
			workload: WorkloadInfo{
				CPUUsageMillis:   30,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationIdle,
		},
		{
			name: "idle at exactly threshold",
			workload: WorkloadInfo{
				CPUUsageMillis:   50,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationIdle,
		},
		{
			name: "wasteful when utilization below wasteful threshold",
			workload: WorkloadInfo{
				CPUUsageMillis:   200,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationWasteful,
		},
		{
			name: "active when utilization above wasteful threshold",
			workload: WorkloadInfo{
				CPUUsageMillis:   500,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationActive,
		},
		{
			name: "active at exactly wasteful boundary",
			workload: WorkloadInfo{
				CPUUsageMillis:   300,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationActive,
		},
		{
			name: "active when no CPU request set",
			workload: WorkloadInfo{
				CPUUsageMillis:   200,
				CPURequestMillis: 0,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationActive,
		},
		{
			name: "zero CPU usage is idle",
			workload: WorkloadInfo{
				CPUUsageMillis:   0,
				CPURequestMillis: 1000,
				Replicas:         1,
			},
			want: v1alpha1.ClassificationIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.workload, th)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestUtilizationPercent(t *testing.T) {
	tests := []struct {
		name    string
		usage   int64
		request int64
		want    int
	}{
		{"50% utilization", 500, 1000, 50},
		{"100% utilization", 1000, 1000, 100},
		{"zero request", 500, 0, 0},
		{"zero usage", 0, 1000, 0},
		{"over 100%", 1500, 1000, 150},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, UtilizationPercent(tt.usage, tt.request))
		})
	}
}

func TestEstimateMonthlyCost(t *testing.T) {
	rates := cost.DefaultRates

	w := WorkloadInfo{
		CPURequestMillis:   1000, // 1 core
		MemoryRequestBytes: 1 << 30, // 1 GiB
		StorageBytes:       10 << 30, // 10 GiB
		Replicas:           2,
	}

	got := EstimateMonthlyCost(w, rates)

	// 2 cores × $0.031/hr × 730h + 2 GiB × $0.004/hr × 730h + 10 GiB × $0.08/mo
	expected := 2*0.031*730 + 2*0.004*730 + 10*0.08
	assert.InDelta(t, expected, got, 0.01)
}

func TestEstimateSavings_Idle(t *testing.T) {
	th := DefaultThresholds()

	w := WorkloadInfo{
		CPURequestMillis:   1000,
		MemoryRequestBytes: 1 << 30,
		StorageBytes:       10 << 30,
		Replicas:           2,
	}

	got := EstimateSavings(w, v1alpha1.ClassificationIdle, th)

	// Idle saves full compute, not storage.
	expected := 2*0.031*730 + 2*0.004*730
	assert.InDelta(t, expected, got, 0.01)
}

func TestEstimateSavings_Wasteful(t *testing.T) {
	th := DefaultThresholds() // rightSizeTarget = 70%

	w := WorkloadInfo{
		CPUUsageMillis:     200,
		CPURequestMillis:   1000,
		MemoryUsageBytes:   512 << 20, // 0.5 GiB
		MemoryRequestBytes: 2 << 30,   // 2 GiB
		Replicas:           1,
	}

	got := EstimateSavings(w, v1alpha1.ClassificationWasteful, th)

	// right-sized CPU = 0.2 cores / 0.7 ≈ 0.2857 cores → delta = 1.0 - 0.2857 = 0.7143
	// right-sized mem = 0.5 GiB / 0.7 ≈ 0.7143 GiB → delta = 2.0 - 0.7143 = 1.2857
	cpuDelta := 1.0 - 0.2/0.7
	memDelta := 2.0 - 0.5/0.7
	expected := cpuDelta*0.031*730 + memDelta*0.004*730
	assert.InDelta(t, expected, got, 0.01)
}

func TestEstimateSavings_Active(t *testing.T) {
	th := DefaultThresholds()
	w := WorkloadInfo{CPUUsageMillis: 500, CPURequestMillis: 1000, Replicas: 1}
	assert.Equal(t, 0.0, EstimateSavings(w, v1alpha1.ClassificationActive, th))
}

func TestBuildDiscovered(t *testing.T) {
	th := DefaultThresholds()

	w := WorkloadInfo{
		Name:               "api-server",
		Kind:               v1alpha1.TargetKindDeployment,
		CPUUsageMillis:     30,
		CPURequestMillis:   1000,
		MemoryUsageBytes:   256 << 20,
		MemoryRequestBytes: 1 << 30,
		Replicas:           2,
		Managed:            false,
	}

	d := BuildDiscovered(w, th)

	assert.Equal(t, "api-server", d.Name)
	assert.Equal(t, v1alpha1.TargetKindDeployment, d.Kind)
	assert.Equal(t, v1alpha1.ClassificationIdle, d.Classification)
	assert.Equal(t, int32(2), d.Replicas)
	assert.Equal(t, 3, d.UtilizationPercent)
	assert.False(t, d.Managed)
	assert.NotEmpty(t, d.EstimatedMonthlyCost)
	assert.NotEmpty(t, d.EstimatedSavings)
}
