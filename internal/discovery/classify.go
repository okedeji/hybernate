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
	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

const hoursPerMonth = 730

// Thresholds holds classification parameters extracted from a WorkloadPolicy spec.
type Thresholds struct {
	IdleMillis            int
	MemoryIdleBytes       int64
	WastefulPercent       int
	MemoryWastefulPercent int
	RightSizePercent      int
	Rates                 cost.Rates
}

// DefaultThresholds returns classification parameters matching the CRD defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		IdleMillis:            50,
		MemoryIdleBytes:       104857600, // 100Mi
		WastefulPercent:       30,
		MemoryWastefulPercent: 30,
		RightSizePercent:      70,
		Rates:                 cost.DefaultRates,
	}
}

// WorkloadInfo captures the resource profile of a single workload for classification.
type WorkloadInfo struct {
	Name               string
	Kind               v1alpha1.TargetKind
	Replicas           int32
	CPUUsageMillis     int64
	CPURequestMillis   int64
	MemoryUsageBytes   int64
	MemoryRequestBytes int64
	StorageBytes       int64
	Managed            bool
	Ignored            bool
}

// Classify determines whether a workload is Active, Idle, or Wasteful.
// Idle requires both CPU and memory to be below their thresholds.
// Wasteful requires both CPU and memory utilization to be below their thresholds.
func Classify(w WorkloadInfo, t Thresholds) v1alpha1.Classification {
	cpuIdle := w.CPUUsageMillis <= int64(t.IdleMillis)
	memIdle := w.MemoryUsageBytes <= t.MemoryIdleBytes

	if cpuIdle && memIdle {
		return v1alpha1.ClassificationIdle
	}

	cpuWasteful := false
	if w.CPURequestMillis > 0 {
		cpuUtil := float64(w.CPUUsageMillis) / float64(w.CPURequestMillis) * 100
		cpuWasteful = cpuUtil < float64(t.WastefulPercent)
	}

	memWasteful := false
	if t.MemoryWastefulPercent > 0 && w.MemoryRequestBytes > 0 {
		memUtil := float64(w.MemoryUsageBytes) / float64(w.MemoryRequestBytes) * 100
		memWasteful = memUtil < float64(t.MemoryWastefulPercent)
	}

	if cpuWasteful && memWasteful {
		return v1alpha1.ClassificationWasteful
	}

	return v1alpha1.ClassificationActive
}

// UtilizationPercent returns CPU utilization as an integer percentage.
func UtilizationPercent(usageMillis, requestMillis int64) int {
	if requestMillis <= 0 {
		return 0
	}
	return int(float64(usageMillis) / float64(requestMillis) * 100)
}

// EstimateMonthlyCost estimates the monthly cost based on current resource requests.
func EstimateMonthlyCost(w WorkloadInfo, rates cost.Rates) float64 {
	r := float64(w.Replicas)
	cpuCores := float64(w.CPURequestMillis) / 1000 * r
	memGiB := float64(w.MemoryRequestBytes) / (1 << 30) * r
	storageGiB := float64(w.StorageBytes) / (1 << 30)

	return cpuCores*rates.CPUPerHour*hoursPerMonth +
		memGiB*rates.MemoryPerHour*hoursPerMonth +
		storageGiB*rates.StoragePerMonth
}

// EstimateSavings returns the monthly savings if Hybernate manages this workload.
// Idle: full compute cost saved (storage remains).
// Wasteful: delta between current and right-sized requests at target utilization.
func EstimateSavings(w WorkloadInfo, class v1alpha1.Classification, t Thresholds) float64 {
	r := float64(w.Replicas)

	switch class {
	case v1alpha1.ClassificationIdle:
		cpuCores := float64(w.CPURequestMillis) / 1000 * r
		memGiB := float64(w.MemoryRequestBytes) / (1 << 30) * r
		return cpuCores*t.Rates.CPUPerHour*hoursPerMonth +
			memGiB*t.Rates.MemoryPerHour*hoursPerMonth

	case v1alpha1.ClassificationWasteful:
		if w.CPURequestMillis <= 0 || t.RightSizePercent <= 0 {
			return 0
		}
		target := float64(t.RightSizePercent) / 100

		currentCPU := float64(w.CPURequestMillis) / 1000 * r
		rightCPU := float64(w.CPUUsageMillis) / 1000 * r / target
		cpuDelta := currentCPU - rightCPU
		if cpuDelta < 0 {
			cpuDelta = 0
		}

		currentMem := float64(w.MemoryRequestBytes) / (1 << 30) * r
		rightMem := float64(w.MemoryUsageBytes) / (1 << 30) * r / target
		memDelta := currentMem - rightMem
		if memDelta < 0 {
			memDelta = 0
		}

		return cpuDelta*t.Rates.CPUPerHour*hoursPerMonth +
			memDelta*t.Rates.MemoryPerHour*hoursPerMonth

	default:
		return 0
	}
}

// BuildDiscovered constructs a DiscoveredWorkload from a WorkloadInfo and thresholds.
func BuildDiscovered(w WorkloadInfo, t Thresholds) v1alpha1.DiscoveredWorkload {
	class := Classify(w, t)
	return v1alpha1.DiscoveredWorkload{
		Name:                      w.Name,
		Kind:                      w.Kind,
		Classification:            class,
		CPUUsageMillis:            w.CPUUsageMillis,
		CPURequestMillis:          w.CPURequestMillis,
		Replicas:                  w.Replicas,
		MemoryUsageBytes:          w.MemoryUsageBytes,
		MemoryRequestBytes:        w.MemoryRequestBytes,
		StorageBytes:              w.StorageBytes,
		CPUUtilizationPercent:     UtilizationPercent(w.CPUUsageMillis, w.CPURequestMillis),
		MemoryUtilizationPercent:  UtilizationPercent(w.MemoryUsageBytes, w.MemoryRequestBytes),
		EstimatedMonthlyCost:      cost.FormatDollars(EstimateMonthlyCost(w, t.Rates)),
		EstimatedPotentialSavings: cost.FormatDollars(EstimateSavings(w, class, t)),
		Managed:                   w.Managed,
		Ignored:                   w.Ignored,
	}
}
