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

const defaultWindow = 24

// Scorer tracks rolling prediction accuracy using Mean Absolute Percentage
// Error (MAPE). Confidence is 1 - MAPE, clamped to [0, 1]. A confidence of
// 0.85 means predictions are on average within 15% of actual values.
type Scorer struct {
	window int
	errors []float64
	pos    int
	full   bool
}

func NewScorer() *Scorer {
	return &Scorer{
		window: defaultWindow,
		errors: make([]float64, defaultWindow),
	}
}

// Record computes the absolute percentage error between forecast and actual,
// and adds it to the rolling window.
func (s *Scorer) Record(forecast, actual float64) {
	var ape float64
	if actual != 0 {
		ape = math.Abs(forecast-actual) / math.Abs(actual)
	} else if forecast != 0 {
		ape = 1.0
	}

	s.errors[s.pos] = ape
	s.pos = (s.pos + 1) % s.window
	if s.pos == 0 {
		s.full = true
	}
}

// Confidence returns 1 - MAPE over the rolling window.
// Returns 0 if fewer than window data points have been recorded.
func (s *Scorer) Confidence() float64 {
	if !s.full {
		return 0
	}

	var sum float64
	for _, e := range s.errors {
		sum += e
	}
	mape := sum / float64(s.window)

	c := 1.0 - mape
	if c < 0 {
		return 0
	}
	return c
}

// Ready returns true when enough data has been recorded to produce
// a meaningful confidence score.
func (s *Scorer) Ready() bool {
	return s.full
}
