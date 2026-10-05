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

// hoursPerMonth is the average month that StoragePerMonth is priced over.
const hoursPerMonth = 730

// Hours are resource-hours: vCPU-hours and GiB-hours of memory and storage.
type Hours struct {
	CPU     float64
	Memory  float64
	Storage float64
}

// Price is what h costs at r.
func (h Hours) Price(r Rates) float64 {
	return h.CPU*r.CPUPerHour + h.Memory*r.MemoryPerHour + h.Storage*r.StoragePerMonth/hoursPerMonth
}

// ComputeHourly is what an hour of the given CPU and memory costs.
func ComputeHourly(cpuCores, memoryGiB float64, rates Rates) float64 {
	return Hours{CPU: cpuCores, Memory: memoryGiB}.Price(rates)
}

// MaxInterval is the most of one interval between accumulations that
// Counted counts.
const MaxInterval = 2 * time.Hour

// Counted is how much of an interval between accumulations counts. One
// longer than MaxInterval means the operator wasn't running or the clock
// jumped, and what the workload did meanwhile isn't known, so it counts as
// MaxInterval: enough to cover a slow restart, without billing hours of
// downtime at whatever phase the workload was last seen in. A negative one,
// from the clock going back, counts as nothing.
func Counted(d time.Duration) time.Duration {
	return min(max(d, 0), MaxInterval)
}

// MonthOf is the start of the calendar month t falls in, in UTC.
func MonthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Project carries amount, accrued over tracked, to the whole of the UTC
// month t falls in at the same rate. It's false until a day has been
// tracked: a shorter stretch misses a daily pattern's quiet hours, or its
// busy ones.
func Project(amount float64, tracked time.Duration, t time.Time) (float64, bool) {
	if tracked < 24*time.Hour {
		return 0, false
	}
	start := MonthOf(t)
	month := start.AddDate(0, 1, 0).Sub(start)
	return amount * month.Hours() / tracked.Hours(), true
}

// FormatDollars formats a cost value as a dollar string.
func FormatDollars(amount float64) string {
	return fmt.Sprintf("$%.2f", amount)
}
