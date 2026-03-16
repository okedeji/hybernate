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
	"fmt"
	"time"
)

// Rates holds per-unit cost rates for compute and storage resources.
type Rates struct {
	CPUPerHour      float64
	MemoryPerHour   float64
	StoragePerMonth float64
}

// DefaultRates are derived from AWS on-demand pricing (us-east-1, 2026)
// and serve as reasonable defaults when users don't configure their own.
var DefaultRates = Rates{
	// ~$0.029-0.035/vCPU-hour depending on instance family.
	// Based on m6i.large ($0.096/hr, 2 vCPU, 60% CPU cost weight).
	// GKE Autopilot charges $0.0445/vCPU-hour for comparison.
	CPUPerHour: 0.031,
	// ~$0.004-0.005/GiB-hour depending on instance family.
	// Based on m6i.large ($0.096/hr, 8 GiB, 40% memory cost weight).
	// GKE Autopilot charges $0.0049/GiB-hour for comparison.
	MemoryPerHour: 0.004,
	// AWS EBS gp3 (General Purpose SSD) in us-east-1.
	// Billed on provisioned capacity, not actual usage.
	// Other volume types: io2 $0.125, st1 $0.045, sc1 $0.015.
	StoragePerMonth: 0.08,
}

// Snapshot holds accumulated resource consumption for a billing period.
type Snapshot struct {
	CPUHours     float64
	MemoryHours  float64
	StorageHours float64
	SavedCost    float64
}

const maxElapsed = 2 * time.Hour

// Accumulate adds time-weighted resource usage to an existing snapshot.
// elapsed is capped at 2h to bound error after operator restarts.
func Accumulate(s Snapshot, cpuCores, memoryGiB, storageGiB float64, elapsed time.Duration) Snapshot {
	hours := clampElapsed(elapsed)
	s.CPUHours += cpuCores * hours
	s.MemoryHours += memoryGiB * hours
	s.StorageHours += storageGiB * hours
	return s
}

// AccumulateSavings adds savings for a workload that Hybernate has acted on.
// For paused workloads, storageGiB should be 0 since PVCs persist while paused.
// For destroyed workloads with cleaned-up PVCs, include storageGiB.
func AccumulateSavings(s Snapshot, cpuCores, memoryGiB, storageGiB float64, elapsed time.Duration, rates Rates) Snapshot {
	hours := clampElapsed(elapsed)
	s.SavedCost += (cpuCores*rates.CPUPerHour + memoryGiB*rates.MemoryPerHour + storageGiB*storageHourlyRate(rates)) * hours
	return s
}

// TotalCost computes the dollar cost from accumulated resource consumption.
func TotalCost(s Snapshot, rates Rates) float64 {
	return s.CPUHours*rates.CPUPerHour +
		s.MemoryHours*rates.MemoryPerHour +
		s.StorageHours*storageHourlyRate(rates)
}

// EstimateMonthlyCost projects the full-month cost based on usage so far.
// Returns -1 when there's not enough data (day 1) so the caller can
// display "pending" or omit the field.
func EstimateMonthlyCost(s Snapshot, rates Rates, dayOfMonth, daysInMonth int) float64 {
	if dayOfMonth <= 1 {
		return -1
	}
	costSoFar := TotalCost(s, rates)
	return costSoFar / float64(dayOfMonth) * float64(daysInMonth)
}

// CostWithoutManagement returns what the workload would have cost if
// Hybernate hadn't acted — current spend plus everything we saved.
func CostWithoutManagement(s Snapshot, rates Rates) float64 {
	return TotalCost(s, rates) + s.SavedCost
}

// FormatDollars formats a cost value as a dollar string.
func FormatDollars(amount float64) string {
	return fmt.Sprintf("$%.2f", amount)
}

// storageHourlyRate converts $/GiB-month to $/GiB-hour (730 hours/month avg).
func storageHourlyRate(r Rates) float64 {
	return r.StoragePerMonth / 730
}

func clampElapsed(d time.Duration) float64 {
	if d > maxElapsed {
		d = maxElapsed
	}
	if d < 0 {
		return 0
	}
	return d.Hours()
}
