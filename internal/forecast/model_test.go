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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Monday 00:00 UTC as test epoch.
var testEpoch = time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)

func hourAt(offset int) time.Time {
	return testEpoch.Add(time.Duration(offset) * time.Hour)
}

func TestModel_FirstDataPoint(t *testing.T) {
	m := NewModel(DefaultParams())
	forecast := m.Update(100, testEpoch)

	assert.Equal(t, 100.0, forecast)
	assert.Equal(t, 100.0, m.level)
	assert.Equal(t, 1, m.DataPoints())
}

func TestModel_LevelTracksConstantInput(t *testing.T) {
	m := NewModel(DefaultParams())

	for i := 0; i < 48; i++ {
		m.Update(50, hourAt(i))
	}

	assert.InDelta(t, 50, m.level, 1.0)
	assert.InDelta(t, 0, m.trend, 0.5)
}

func TestModel_ForecastReflectsDailyPattern(t *testing.T) {
	m := NewModel(DefaultParams())

	// Feed 3 days of data with a clear daily pattern:
	// hours 8-17 = 100 (busy), all other hours = 20 (quiet)
	h := 0
	for day := 0; day < 3; day++ {
		for hour := 0; hour < 24; hour++ {
			if hour >= 8 && hour <= 17 {
				m.Update(100, hourAt(h))
			} else {
				m.Update(20, hourAt(h))
			}
			h++
		}
	}

	// After 72 data points, the model should have learned the daily pattern.
	// Current position is hour 0 of day 4 (Thursday 00:00 UTC).
	now := hourAt(h)
	busyForecast := m.Forecast(9, now)  // 9am
	quietForecast := m.Forecast(2, now) // 2am

	assert.Greater(t, busyForecast, quietForecast,
		"9am forecast (%f) should be higher than 2am forecast (%f)", busyForecast, quietForecast)
}

func TestModel_ForecastNeverNegative(t *testing.T) {
	m := NewModel(DefaultParams())

	for i := 0; i < 48; i++ {
		m.Update(math.Max(0, 100-float64(i)*3), hourAt(i))
	}

	now := hourAt(48)
	for h := 1; h <= 24; h++ {
		assert.GreaterOrEqual(t, m.Forecast(h, now), 0.0)
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

func TestModel_WallClockSlotAlignment(t *testing.T) {
	m := NewModel(DefaultParams())

	// Feed one data point at Monday 14:00 UTC
	monday2pm := time.Date(2026, 3, 16, 14, 0, 0, 0, time.UTC)
	m.Update(100, monday2pm)

	// Feed second point at Monday 15:00 UTC so daily factor at slot 15 gets touched
	monday3pm := monday2pm.Add(time.Hour)
	m.Update(200, monday3pm)

	// Daily slot 15 (3pm) should have been used for the update
	assert.Equal(t, 2, m.DataPoints())
	// Weekly slot for Monday 3pm: Monday=0, so 0*24+15 = 15
	assert.Equal(t, 15, weeklyIndex(monday3pm))
	assert.Equal(t, 15, dailyIndex(monday3pm))
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

	for i := 0; i < DailySeason; i++ {
		e.Observe(50, hourAt(i))
	}
	assert.Equal(t, DailySuggesting, e.Phase)
}

func TestEngine_PredictReturnsZeroBeforeDailyActive(t *testing.T) {
	e := NewEngine(DefaultParams(), 50)

	for i := 0; i < 10; i++ {
		e.Observe(50, hourAt(i))
	}

	assert.Equal(t, 0.0, e.Predict(1, hourAt(10)))
}

func TestEngine_DailyActiveProducesPredictions(t *testing.T) {
	e := NewEngine(DefaultParams(), 0) // threshold=0 so it promotes immediately

	n := DailySeason + defaultWindow + 1
	for i := 0; i < n; i++ {
		e.Observe(50, hourAt(i))
	}

	require.Equal(t, DailyActive, e.Phase,
		"expected DailyActive but got %s after %d points", e.Phase, e.Model.DataPoints())

	p := e.Predict(1, hourAt(n))
	assert.Greater(t, p, 0.0)
}

func TestEngine_FullLifecycle(t *testing.T) {
	e := NewEngine(DefaultParams(), 0) // threshold=0 for easy promotion
	h := 0

	for i := 0; i < DailySeason; i++ {
		e.Observe(50, hourAt(h))
		h++
	}
	assert.Equal(t, DailySuggesting, e.Phase)

	for i := 0; i < defaultWindow; i++ {
		e.Observe(50, hourAt(h))
		h++
	}
	assert.Equal(t, DailyActive, e.Phase)

	for e.Model.DataPoints() < WeeklySeason {
		e.Observe(50, hourAt(h))
		h++
	}
	assert.Equal(t, WeeklySuggesting, e.Phase)

	for i := 0; i < defaultWindow; i++ {
		e.Observe(50, hourAt(h))
		h++
	}
	assert.Equal(t, FullyActive, e.Phase)
}

func TestEngine_ExportImportRoundTrip(t *testing.T) {
	e := NewEngine(DefaultParams(), 85)

	// Train to DailyActive
	n := DailySeason + defaultWindow + 1
	for i := 0; i < n; i++ {
		e.Observe(50, hourAt(i))
	}
	require.Equal(t, DailyActive, e.Phase)

	now := hourAt(n)
	forecastBefore := e.Predict(1, now)

	data, err := e.Export()
	require.NoError(t, err)

	restored, err := ImportEngine(data)
	require.NoError(t, err)

	assert.Equal(t, e.Phase, restored.Phase)
	assert.Equal(t, e.Threshold, restored.Threshold)
	assert.Equal(t, e.Model.DataPoints(), restored.Model.DataPoints())
	assert.Equal(t, e.DailyConfidence(), restored.DailyConfidence())
	assert.Equal(t, forecastBefore, restored.Predict(1, now))
}

func TestEngine_ImportRejectsUnsupportedVersion(t *testing.T) {
	_, err := ImportEngine([]byte(`{"v":999}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported state version")
}

func TestEngine_ImportRejectsCorruptJSON(t *testing.T) {
	_, err := ImportEngine([]byte(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshaling")
}

func TestEngine_RegimeChangedSignal(t *testing.T) {
	e := NewEngine(DefaultParams(), 0)
	h := 0

	// Advance to DailyActive.
	for i := 0; i < DailySeason+defaultWindow+1; i++ {
		e.Observe(50, hourAt(h))
		h++
	}
	require.Equal(t, DailyActive, e.Phase)
	assert.False(t, e.RegimeChanged(), "no regime change yet")

	// Build up anomaly detector baseline.
	for i := 0; i < 100; i++ {
		e.Observe(50, hourAt(h))
		h++
	}
	assert.False(t, e.RegimeChanged())

	// Inject anomalies to trigger regime change.
	for i := 0; i < anomalyWindow; i++ {
		e.Observe(500, hourAt(h))
		h++
	}
	assert.True(t, e.RegimeChanged(), "regime change should be signaled")
	assert.NotEqual(t, DailyActive, e.Phase, "phase should have demoted")
}
