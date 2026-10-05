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
	"math/rand"
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

// officeHours is 500m on weekdays from 9 to 17 in t's location, and nothing
// otherwise: the workload Hybernate exists for.
func officeHours(t time.Time) float64 {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return 0
	}
	if t.Hour() >= 9 && t.Hour() < 17 {
		return 500
	}
	return 0
}

func TestModel_FirstObservationSetsLevel(t *testing.T) {
	m := NewModel(DefaultParams())
	m.update(100, testEpoch, 1)

	assert.Equal(t, 100.0, m.level)
	assert.Equal(t, 1, m.DataPoints())
}

func TestModel_LevelTracksConstantInput(t *testing.T) {
	m := NewModel(DefaultParams())
	for i := range 48 {
		m.update(50, hourAt(i), 1)
	}

	assert.InDelta(t, 50, m.level, 1.0)
	assert.InDelta(t, 0, m.trend, 0.5)
	assert.InDelta(t, 50, m.forecast(hourAt(48), 1), 1.0)
}

func TestModel_SeasonalComponentsStayNormalised(t *testing.T) {
	m := NewModel(DefaultParams())
	for i := range 6 * WeeklySeason {
		m.update(officeHours(hourAt(i)), hourAt(i), 1)
	}

	var daily float64
	for _, d := range m.daily {
		daily += d
	}
	assert.InDelta(t, 0, daily, 1e-6, "daily components sum to zero")
	for hour := range DailySeason {
		var weekly float64
		for day := range daysPerWeek {
			weekly += m.weekly[day*DailySeason+hour]
		}
		assert.InDelta(t, 0, weekly, 1e-6, "weekly components for hour %d sum to zero", hour)
	}
	assert.InDelta(t, 500*40.0/WeeklySeason, m.level, 15, "the level is the mean demand")
}

// TestModel_OfficeHoursStaysBounded is the trace that drove the old
// multiplicative model's level negative every weekend and swung the 10am
// forecast between 0 and thousands.
func TestModel_OfficeHoursStaysBounded(t *testing.T) {
	m := NewModel(DefaultParams())
	for i := range 12 * WeeklySeason {
		at := hourAt(i)
		m.update(officeHours(at), at, 1)
		require.GreaterOrEqual(t, m.level, 0.0, "hour %d", i)

		next := hourAt(i + 1)
		require.LessOrEqual(t, m.forecast(next, 1), 750.0, "hour %d forecasts beyond any demand seen", i)
	}

	monday := hourAt(12 * WeeklySeason)
	assert.InDelta(t, 500, m.forecast(monday.Add(10*time.Hour), 10), 50, "Monday 10am")
	assert.Less(t, m.forecast(monday.Add(3*time.Hour), 3), 25.0, "Monday 3am")
	assert.Less(t, m.forecast(monday.Add((5*24+10)*time.Hour), 24), 25.0, "Saturday 10am")
}

func TestModel_WallClockSlots(t *testing.T) {
	monday3pm := time.Date(2026, 3, 16, 15, 0, 0, 0, time.UTC)
	sunday11pm := time.Date(2026, 3, 22, 23, 0, 0, 0, time.UTC)

	assert.Equal(t, 15, dailyIndex(monday3pm))
	assert.Equal(t, 15, weeklyIndex(monday3pm))
	assert.Equal(t, WeeklySeason-1, weeklyIndex(sunday11pm))

	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	assert.Equal(t, 11, dailyIndex(monday3pm.In(ny)), "slots are counted in the time's location")
}

// TestModel_Invariants checks, over random and adversarial traces, that the
// model never produces NaN or Inf, never lets the level go below zero, and
// never forecasts negative demand.
func TestModel_Invariants(t *testing.T) {
	traces := map[string]func(r *rand.Rand, i int) float64{
		"uniform":         func(r *rand.Rand, _ int) float64 { return r.Float64() * 1000 },
		"mostly zero":     func(r *rand.Rand, _ int) float64 { return float64(r.Intn(20)/19) * 800 },
		"alternating max": func(_ *rand.Rand, i int) float64 { return float64(i%2) * maxDemand },
		"constant max":    func(_ *rand.Rand, _ int) float64 { return maxDemand },
		"heavy tail":      func(r *rand.Rand, _ int) float64 { return math.Min(maxDemand, math.Exp(r.NormFloat64()*6)) },
		"drop to zero": func(_ *rand.Rand, i int) float64 {
			if i < 2*WeeklySeason {
				return 1000 - float64(i)
			}
			return 0
		},
	}
	for name, trace := range traces {
		t.Run(name, func(t *testing.T) {
			for seed := range int64(5) {
				r := rand.New(rand.NewSource(seed))
				m := NewModel(DefaultParams())
				at := testEpoch
				for i := range 6 * WeeklySeason {
					steps := 1
					if r.Intn(10) == 0 {
						steps = 1 + r.Intn(48)
					}
					at = at.Add(time.Duration(steps) * time.Hour)
					m.update(trace(r, i), at, steps)

					require.True(t, m.finite(), "seed %d hour %d", seed, i)
					require.GreaterOrEqual(t, m.level, 0.0, "seed %d hour %d", seed, i)
					for h := range DailySeason + 1 {
						f := m.forecast(at.Add(time.Duration(h)*time.Hour), h)
						require.False(t, math.IsNaN(f) || math.IsInf(f, 0), "seed %d hour %d", seed, i)
						require.GreaterOrEqual(t, f, 0.0, "seed %d hour %d", seed, i)
					}
				}
			}
		})
	}
}
