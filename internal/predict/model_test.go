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

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModel_FirstDataPoint(t *testing.T) {
	m := NewModel(DefaultParams())
	forecast := m.Update(100)

	assert.Equal(t, 100.0, forecast)
	assert.Equal(t, 100.0, m.level)
	assert.Equal(t, 1, m.DataPoints())
}

func TestModel_LevelTracksConstantInput(t *testing.T) {
	m := NewModel(DefaultParams())

	for i := 0; i < 48; i++ {
		m.Update(50)
	}

	assert.InDelta(t, 50, m.level, 1.0)
	assert.InDelta(t, 0, m.trend, 0.5)
}

func TestModel_ForecastReflectsDailyPattern(t *testing.T) {
	m := NewModel(DefaultParams())

	// Feed 3 days of data with a clear daily pattern:
	// hours 8-17 = 100 (busy), all other hours = 20 (quiet)
	for day := 0; day < 3; day++ {
		for hour := 0; hour < 24; hour++ {
			if hour >= 8 && hour <= 17 {
				m.Update(100)
			} else {
				m.Update(20)
			}
		}
	}

	// After 72 data points, the model should have learned the daily pattern.
	// Forecast for "next busy hour" should be higher than "next quiet hour".
	// Current position is hour 0 of day 4.
	busyForecast := m.Forecast(9)  // 9am
	quietForecast := m.Forecast(2) // 2am

	assert.Greater(t, busyForecast, quietForecast,
		"9am forecast (%f) should be higher than 2am forecast (%f)", busyForecast, quietForecast)
}

func TestModel_ForecastNeverNegative(t *testing.T) {
	m := NewModel(DefaultParams())

	// Feed decreasing values to push trend negative
	for i := 0; i < 48; i++ {
		m.Update(math.Max(0, 100-float64(i)*3))
	}

	for h := 1; h <= 24; h++ {
		assert.GreaterOrEqual(t, m.Forecast(h), 0.0)
	}
}

func TestModel_SeasonalFactorsInitializedToOne(t *testing.T) {
	m := NewModel(DefaultParams())

	for i := 0; i < DailySeason; i++ {
		assert.Equal(t, 1.0, m.daily[i])
	}
	for i := 0; i < WeeklySeason; i++ {
		assert.Equal(t, 1.0, m.weekly[i])
	}
}

func TestScorer_ConfidenceZeroBeforeReady(t *testing.T) {
	s := NewScorer()

	assert.Equal(t, 0.0, s.Confidence())
	assert.False(t, s.Ready())

	for i := 0; i < defaultWindow-1; i++ {
		s.Record(100, 100)
	}
	assert.False(t, s.Ready())
	assert.Equal(t, 0.0, s.Confidence())
}

func TestScorer_PerfectPredictions(t *testing.T) {
	s := NewScorer()

	for i := 0; i < defaultWindow; i++ {
		s.Record(100, 100)
	}

	assert.True(t, s.Ready())
	assert.Equal(t, 1.0, s.Confidence())
}

func TestScorer_TenPercentError(t *testing.T) {
	s := NewScorer()

	for i := 0; i < defaultWindow; i++ {
		s.Record(110, 100) // 10% over every time
	}

	assert.True(t, s.Ready())
	assert.InDelta(t, 0.9, s.Confidence(), 0.01)
}

func TestScorer_RollingWindow(t *testing.T) {
	s := NewScorer()

	// Fill with bad predictions (50% error)
	for i := 0; i < defaultWindow; i++ {
		s.Record(150, 100)
	}
	assert.InDelta(t, 0.5, s.Confidence(), 0.01)

	// Replace with perfect predictions
	for i := 0; i < defaultWindow; i++ {
		s.Record(100, 100)
	}
	assert.Equal(t, 1.0, s.Confidence())
}

func TestAnomalyDetector_NoAnomalyOnNormalData(t *testing.T) {
	ad := NewAnomalyDetector()

	for i := 0; i < 100; i++ {
		anomaly := ad.Record(50, 50+float64(i%3))
		_ = anomaly
	}

	assert.False(t, ad.RegimeChange())
}

func TestAnomalyDetector_RegimeChangeOnSpike(t *testing.T) {
	ad := NewAnomalyDetector()

	// Normal data for a while
	for i := 0; i < 100; i++ {
		ad.Record(50, 50)
	}

	// Sudden massive spike — 10x normal
	for i := 0; i < anomalyWindow; i++ {
		ad.Record(50, 500)
	}

	assert.True(t, ad.RegimeChange())
}

func TestEngine_PhaseProgression(t *testing.T) {
	e := NewEngine(DefaultParams(), 50)

	assert.Equal(t, Observing, e.Phase)

	// Feed 24 hours of constant data — should move to DailySuggesting
	for i := 0; i < DailySeason; i++ {
		e.Observe(50)
	}
	assert.Equal(t, DailySuggesting, e.Phase)
}

func TestEngine_PredictReturnsZeroBeforeDailyActive(t *testing.T) {
	e := NewEngine(DefaultParams(), 50)

	for i := 0; i < 10; i++ {
		e.Observe(50)
	}

	assert.Equal(t, 0.0, e.Predict(1))
}

func TestEngine_DailyActiveProducesPredictions(t *testing.T) {
	e := NewEngine(DefaultParams(), 0) // threshold=0 so it promotes immediately

	// Feed enough data to reach DailyActive
	for i := 0; i < DailySeason+defaultWindow+1; i++ {
		e.Observe(50)
	}

	require.Equal(t, DailyActive, e.Phase,
		"expected DailyActive but got %s after %d points", e.Phase, e.Model.DataPoints())

	p := e.Predict(1)
	assert.Greater(t, p, 0.0)
}

func TestEngine_FullLifecycle(t *testing.T) {
	e := NewEngine(DefaultParams(), 0) // threshold=0 for easy promotion

	// Observing → DailySuggesting (need 24 points)
	for i := 0; i < DailySeason; i++ {
		e.Observe(50)
	}
	assert.Equal(t, DailySuggesting, e.Phase)

	// DailySuggesting → DailyActive (need scorer to be ready)
	for i := 0; i < defaultWindow; i++ {
		e.Observe(50)
	}
	assert.Equal(t, DailyActive, e.Phase)

	// DailyActive → WeeklySuggesting (need 168 points total)
	for e.Model.DataPoints() < WeeklySeason {
		e.Observe(50)
	}
	assert.Equal(t, WeeklySuggesting, e.Phase)

	// WeeklySuggesting → FullyActive (need weekly scorer ready)
	for i := 0; i < defaultWindow; i++ {
		e.Observe(50)
	}
	assert.Equal(t, FullyActive, e.Phase)
}
