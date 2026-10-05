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

package forecast

import (
	"math"
	"math/bits"
)

const (
	zScoreThreshold       = 3.0
	regimeChangeThreshold = 3
	anomalyWindow         = 24
	anomalyWindowMask     = 1<<anomalyWindow - 1

	// anomalyMemory is how many hours the error statistics effectively
	// remember: a week, so that a weekday/weekend pattern's errors are all
	// part of what is normal.
	anomalyMemory = WeeklySeason
)

// AnomalyDetector flags forecast errors that are far outside the errors
// seen recently, and declares a regime change when they cluster: 3 or more
// in the last 24 observations.
//
// The error is signed (actual - forecast) and the z-score two-sided,
// |error - mean| / stddev, so a sudden surge and a sudden disappearance of
// demand both count. The mean and variance are exponentially weighted, so
// the detector follows the model as it improves, and each error is clipped
// to 3 standard deviations before it updates them, so one spike doesn't
// widen what counts as normal for a week.
//
// An error beyond 3 standard deviations in the same direction as the last
// one in the same hour of the week is not an outlier but a weekly pattern
// the model hasn't learned yet, such as a Tuesday night batch job. It isn't
// flagged, and the model learns it in full.
//
// For the first day after it starts or resets nothing is flagged or
// clipped, so the errors of a new regime are learned in full.
type AnomalyDetector struct {
	mean   float64
	vari   float64
	count  int
	recent uint32

	// above and below are the hours of the week whose last error was beyond
	// 3 standard deviations above or below the mean.
	above, below weekSlots
}

// Record scores the forecast error of one observation in the hour of the
// week slot. It reports whether the error is anomalous, and returns the
// error the model should learn from: an anomaly clipped to 3 standard
// deviations, so one outlier doesn't distort the model for weeks, and any
// other error as it is. floor is the smallest standard deviation errors are
// judged against, so a workload whose forecast has been exact (zero demand,
// forecast zero) isn't alarmed by a single millicore.
func (a *AnomalyDetector) Record(forecast, actual, floor float64, slot int) (anomaly bool, learn float64) {
	err := actual - forecast
	stddev := math.Max(math.Sqrt(a.vari), floor)

	clipped := err
	var high, low bool
	if a.count >= anomalyWindow {
		limit := zScoreThreshold * stddev
		clipped = math.Max(a.mean-limit, math.Min(a.mean+limit, err))
		high, low = stddev > 0 && err > a.mean+limit, stddev > 0 && err < a.mean-limit
	}
	recurring := high && a.above.has(slot) || low && a.below.has(slot)
	anomaly = (high || low) && !recurring
	a.above.set(slot, high)
	a.below.set(slot, low)

	a.count = min(a.count+1, anomalyMemory)
	weight := 1 / float64(a.count)
	delta := clipped - a.mean
	a.mean = clampMagnitude(a.mean + weight*delta)
	a.vari = math.Min(maxMagnitude*maxMagnitude, (1-weight)*(a.vari+weight*delta*delta))

	a.recent = (a.recent << 1) & anomalyWindowMask
	if anomaly {
		a.recent |= 1
		return true, clipped
	}
	return false, err
}

// RegimeChange reports whether anomalies have clustered, meaning the
// learned patterns no longer match reality.
func (a *AnomalyDetector) RegimeChange() bool {
	return bits.OnesCount32(a.recent) >= regimeChangeThreshold
}

// Reset forgets the error statistics and the recent anomalies, so the
// errors of a new regime become the new normal rather than one long string
// of anomalies.
func (a *AnomalyDetector) Reset() {
	*a = AnomalyDetector{}
}

// weekSlots is a set of hours of the week.
type weekSlots [3]uint64

func (w *weekSlots) has(slot int) bool {
	return w[slot/64]&(1<<(slot%64)) != 0
}

func (w *weekSlots) set(slot int, on bool) {
	if on {
		w[slot/64] |= 1 << (slot % 64)
	} else {
		w[slot/64] &^= 1 << (slot % 64)
	}
}

// valid reports whether every slot is an hour of the week.
func (w weekSlots) valid() bool {
	return w[2]>>(WeeklySeason-128) == 0
}
