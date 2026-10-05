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

import "math"

const (
	dailyWindow  = DailySeason
	weeklyWindow = WeeklySeason
)

// Scorer keeps the forecast errors of the last week of observations and
// scores accuracy over the last day or the whole week.
//
// Accuracy is 1 - WAPE: the total absolute error over the window divided by
// the total demand, clamped to [0, 1]. Unlike MAPE it is defined when demand
// is zero, and an hour of zero demand forecast as zero adds nothing to
// either side rather than counting as a perfect hour. The denominator is
// never less than floor per hour, so a workload that is idle all day is
// judged by how far its forecasts stray from zero compared with that floor.
type Scorer struct {
	absErr [weeklyWindow]float64
	actual [weeklyWindow]float64
	next   int
	count  int
}

// Record adds the error of one hourly forecast.
func (s *Scorer) Record(forecast, actual float64) {
	s.push(math.Abs(forecast-actual), math.Abs(actual))
}

func (s *Scorer) push(absErr, actual float64) {
	s.absErr[s.next] = absErr
	s.actual[s.next] = actual
	s.next = (s.next + 1) % weeklyWindow
	s.count = min(s.count+1, weeklyWindow)
}

// Ready reports whether a full window of errors has been recorded.
func (s *Scorer) Ready(window int) bool {
	return s.count >= window
}

// Confidence is 1 - WAPE over the last window hours, or 0 before the window
// is full.
func (s *Scorer) Confidence(window int, floor float64) float64 {
	if !s.Ready(window) {
		return 0
	}
	var errSum, actualSum float64
	for k := 1; k <= window; k++ {
		i := (s.next - k + weeklyWindow) % weeklyWindow
		errSum += s.absErr[i]
		actualSum += s.actual[i]
	}
	denominator := math.Max(actualSum, float64(window)*floor)
	if denominator <= 0 {
		if errSum == 0 {
			return 1
		}
		return 0
	}
	return math.Max(0, 1-errSum/denominator)
}

// Reset discards every recorded error, so confidence is earned again.
func (s *Scorer) Reset() {
	*s = Scorer{}
}

// ordered returns the recorded errors and actuals, oldest first.
func (s *Scorer) ordered() (absErr, actual []float64) {
	absErr = make([]float64, s.count)
	actual = make([]float64, s.count)
	start := (s.next - s.count + weeklyWindow) % weeklyWindow
	for k := range s.count {
		i := (start + k) % weeklyWindow
		absErr[k] = s.absErr[i]
		actual[k] = s.actual[i]
	}
	return absErr, actual
}
