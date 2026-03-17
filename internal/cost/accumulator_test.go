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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAccumulate(t *testing.T) {
	tests := []struct {
		name       string
		initial    Snapshot
		cpuCores   float64
		memoryGiB  float64
		storageGiB float64
		elapsed    time.Duration
		wantCPU    float64
		wantMem    float64
		wantStore  float64
	}{
		{
			name:       "basic 1h accumulation",
			cpuCores:   2,
			memoryGiB:  8,
			storageGiB: 20,
			elapsed:    1 * time.Hour,
			wantCPU:    2,
			wantMem:    8,
			wantStore:  20,
		},
		{
			name:       "1 minute accumulation",
			cpuCores:   2,
			memoryGiB:  8,
			storageGiB: 20,
			elapsed:    1 * time.Minute,
			wantCPU:    2.0 / 60,
			wantMem:    8.0 / 60,
			wantStore:  20.0 / 60,
		},
		{
			name:       "capped at 2h",
			cpuCores:   1,
			memoryGiB:  4,
			storageGiB: 10,
			elapsed:    8 * time.Hour,
			wantCPU:    2,
			wantMem:    8,
			wantStore:  20,
		},
		{
			name:       "negative elapsed treated as zero",
			cpuCores:   2,
			memoryGiB:  8,
			storageGiB: 20,
			elapsed:    -5 * time.Minute,
			wantCPU:    0,
			wantMem:    0,
			wantStore:  0,
		},
		{
			name:       "accumulates on top of existing",
			initial:    Snapshot{CPUHours: 10, MemoryHours: 40, StorageHours: 100},
			cpuCores:   2,
			memoryGiB:  8,
			storageGiB: 20,
			elapsed:    1 * time.Hour,
			wantCPU:    12,
			wantMem:    48,
			wantStore:  120,
		},
		{
			name:    "zero usage",
			elapsed: 1 * time.Hour,
			wantCPU: 0, wantMem: 0, wantStore: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Accumulate(tt.initial, tt.cpuCores, tt.memoryGiB, tt.storageGiB, tt.elapsed)
			assert.InDelta(t, tt.wantCPU, got.CPUHours, 0.001)
			assert.InDelta(t, tt.wantMem, got.MemoryHours, 0.001)
			assert.InDelta(t, tt.wantStore, got.StorageHours, 0.001)
		})
	}
}

func TestAccumulateSavings(t *testing.T) {
	rates := Rates{CPUPerHour: 0.031, MemoryPerHour: 0.004, StoragePerMonth: 0.08}

	tests := []struct {
		name       string
		initial    Snapshot
		cpuCores   float64
		memoryGiB  float64
		storageGiB float64
		elapsed    time.Duration
		wantSaved  float64
	}{
		{
			name:      "paused workload, no storage savings",
			cpuCores:  2,
			memoryGiB: 8,
			elapsed:   1 * time.Hour,
			// (2 * 0.031 + 8 * 0.004 + 0 * storageHourly) * 1h
			wantSaved: 2*0.031 + 8*0.004,
		},
		{
			name:       "destroyed with PVCs cleaned",
			cpuCores:   2,
			memoryGiB:  8,
			storageGiB: 20,
			elapsed:    1 * time.Hour,
			wantSaved:  2*0.031 + 8*0.004 + 20*(0.08/730),
		},
		{
			name:      "capped at 2h",
			cpuCores:  1,
			memoryGiB: 4,
			elapsed:   10 * time.Hour,
			wantSaved: (1*0.031 + 4*0.004) * 2,
		},
		{
			name:      "accumulates on existing savings",
			initial:   Snapshot{SavedCost: 5.0},
			cpuCores:  2,
			memoryGiB: 8,
			elapsed:   1 * time.Hour,
			wantSaved: 5.0 + 2*0.031 + 8*0.004,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AccumulateSavings(tt.initial, tt.cpuCores, tt.memoryGiB, tt.storageGiB, tt.elapsed, rates)
			assert.InDelta(t, tt.wantSaved, got.SavedCost, 0.0001)
		})
	}
}

func TestTotalCost(t *testing.T) {
	rates := DefaultRates
	s := Snapshot{CPUHours: 100, MemoryHours: 400, StorageHours: 200}
	got := TotalCost(s, rates)
	want := 100*0.031 + 400*0.004 + 200*(0.08/730)
	assert.InDelta(t, want, got, 0.0001)
}

func TestCostWithoutManagement(t *testing.T) {
	rates := DefaultRates
	s := Snapshot{CPUHours: 100, MemoryHours: 400, StorageHours: 200, SavedCost: 10.0}
	got := CostWithoutManagement(s, rates)
	want := TotalCost(s, rates) + 10.0
	assert.InDelta(t, want, got, 0.0001)
}

func TestEstimateMonthlyCost(t *testing.T) {
	rates := DefaultRates
	s := Snapshot{CPUHours: 50, MemoryHours: 200, StorageHours: 100}
	costSoFar := TotalCost(s, rates)

	tests := []struct {
		name        string
		dayOfMonth  int
		daysInMonth int
		want        float64
	}{
		{
			name:        "day 1 returns pending",
			dayOfMonth:  1,
			daysInMonth: 30,
			want:        -1,
		},
		{
			name:        "day 10 of 30",
			dayOfMonth:  10,
			daysInMonth: 30,
			want:        costSoFar / 10 * 30,
		},
		{
			name:        "day 15 of 31",
			dayOfMonth:  15,
			daysInMonth: 31,
			want:        costSoFar / 15 * 31,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateMonthlyCost(s, rates, tt.dayOfMonth, tt.daysInMonth)
			assert.InDelta(t, tt.want, got, 0.0001)
		})
	}
}

func TestFormatDollars(t *testing.T) {
	assert.Equal(t, "$0.00", FormatDollars(0))
	assert.Equal(t, "$1.50", FormatDollars(1.5))
	assert.Equal(t, "$123.46", FormatDollars(123.456))
}
