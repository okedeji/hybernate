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

package predict

import "math"

const (
	zScoreThreshold       = 3.0
	regimeChangeThreshold = 3
	anomalyWindow         = 24
)

// AnomalyDetector identifies regime changes by tracking z-score anomalies
// in a rolling window. When anomalies cluster (3+ in 24 hours), the model's
// learned patterns are no longer valid.
type AnomalyDetector struct {
	errors []float64
	pos    int
	full   bool
	mean   float64
	m2     float64
	count  int
	recent []bool
	rPos   int
	rFull  bool
}

func NewAnomalyDetector() *AnomalyDetector {
	return &AnomalyDetector{
		errors: make([]float64, anomalyWindow),
		recent: make([]bool, anomalyWindow),
	}
}

// Record checks whether the error between forecast and actual is anomalous.
// Uses Welford's online algorithm for running mean and variance.
func (a *AnomalyDetector) Record(forecast, actual float64) bool {
	err := actual - forecast

	a.count++
	delta := err - a.mean
	a.mean += delta / float64(a.count)
	delta2 := err - a.mean
	a.m2 += delta * delta2

	anomaly := false
	if a.count > anomalyWindow {
		stddev := math.Sqrt(a.m2 / float64(a.count-1))
		if stddev > 0 {
			z := math.Abs(err-a.mean) / stddev
			anomaly = z > zScoreThreshold
		}
	}

	a.recent[a.rPos] = anomaly
	a.rPos = (a.rPos + 1) % anomalyWindow
	if a.rPos == 0 {
		a.rFull = true
	}

	return anomaly
}

// RegimeChange returns true when anomalies cluster, indicating the model's
// learned patterns no longer match reality.
func (a *AnomalyDetector) RegimeChange() bool {
	if !a.rFull {
		return false
	}

	count := 0
	for _, v := range a.recent {
		if v {
			count++
		}
	}
	return count >= regimeChangeThreshold
}
