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

// Additive Holt-Winters double seasonal smoothing (Taylor's method).
//
// Each update has the exponential smoothing form
//
//	new = weight × (fresh evidence) + (1 - weight) × (old belief)
//
// and each component strips out the others before updating:
//
//	Level:   L(t) = α × (Y(t) - D(t) - W(t))  + (1-α) × (L(t-k) + k×T(t-k))
//	Trend:   T(t) = β × (L(t) - L(t-k)) / k   + (1-β) × T(t-k)
//	Daily:   D(t) = γ₁ × (Y(t) - L(t) - W(t)) + (1-γ₁) × D(t-s₁)
//	Weekly:  W(t) = γ₂ × (Y(t) - L(t) - D(t)) + (1-γ₂) × W(t-s₂)
//
// where k is the number of hours since the previous observation, s₁ = 24
// and s₂ = 168. The forecast h hours after the last observation is
//
//	F(t+h) = max(0, L(t) + h×T(t) + D(t+h) + W(t+h))
//
// The additive form is used rather than the multiplicative one because
// idle workloads spend much of their time at zero demand: a multiplicative
// model divides by the level and the seasonal factors, which reach zero
// there, and it diverges. Additive components stay bounded by the demand
// they were fitted to.
//
// The components are kept identifiable by renormalising after each update:
// the daily components sum to zero, and for every hour of the day the
// weekly components across the seven days sum to zero. A shift in a mean is
// moved into the component above it (weekly into daily, daily into the
// level), which leaves every forecast unchanged. The level is then the mean
// demand, the daily components the shape of a day, and the weekly
// components how each weekday departs from that shape, so a level held at
// zero or above means what it says.

package forecast

import (
	"math"
	"time"
)

const (
	DailySeason  = 24
	WeeklySeason = 168

	daysPerWeek = WeeklySeason / DailySeason

	// maxTrendSteps bounds how far the trend is extrapolated across a gap in
	// observations. The trend is fitted to hour-on-hour change; carried
	// over a long outage it would dominate the forecast.
	maxTrendSteps = DailySeason
)

// dailyIndex is the hour of day of t, in t's location.
func dailyIndex(t time.Time) int {
	return t.Hour()
}

// weeklyIndex is the hour of week of t, in t's location: Monday 00:00 is 0,
// Sunday 23:00 is 167.
func weeklyIndex(t time.Time) int {
	monday0 := (int(t.Weekday()) + 6) % 7
	return monday0*DailySeason + t.Hour()
}

// Params controls how quickly the model adapts to new data.
// Higher values = more reactive. Lower values = more stable.
type Params struct {
	Alpha  float64 // level smoothing
	Beta   float64 // trend smoothing
	Gamma1 float64 // daily seasonality smoothing, per day
	Gamma2 float64 // weekly seasonality smoothing, per week

	// Floor is the demand, in the unit observed, below which a difference
	// doesn't matter. Accuracy and anomalies are judged against at least
	// this much, so a workload idling at a few millicores isn't scored on
	// its noise.
	Floor float64
}

// DefaultParams suit hourly CPU in millicores, and were chosen by
// simulating office-hours, always-on, idle, and noisy traces. A weekly slot
// is updated once a week, so Gamma2 is high enough for a weekday/weekend
// pattern to be learned in about three weeks. Alpha and Gamma1 are low so
// that the level stays the mean demand and the daily components the
// average day, leaving the weekly components to carry how weekdays differ.
func DefaultParams() Params {
	return Params{
		Alpha:  0.01,
		Beta:   0.001,
		Gamma1: 0.1,
		Gamma2: 0.5,
		Floor:  10,
	}
}

// Model is an additive Holt-Winters double seasonal model of hourly demand.
// It learns a daily (24h) and a weekly (168h) pattern.
type Model struct {
	params Params

	level  float64
	trend  float64
	daily  [DailySeason]float64
	weekly [WeeklySeason]float64

	n int
}

func NewModel(params Params) *Model {
	return &Model{params: params}
}

// forecast is the demand expected at t, steps hours after the last
// observation.
func (m *Model) forecast(t time.Time, steps int) float64 {
	steps = min(steps, maxTrendSteps)
	f := m.level + float64(steps)*m.trend + m.daily[dailyIndex(t)] + m.weekly[weeklyIndex(t)]
	return math.Min(maxMagnitude, math.Max(0, f))
}

// update fits an observation y made at t, steps hours after the previous
// one.
func (m *Model) update(y float64, t time.Time, steps int) {
	if m.n == 0 {
		m.level = y
		m.n = 1
		return
	}
	steps = max(1, min(steps, maxTrendSteps))

	di, wi := dailyIndex(t), weeklyIndex(t)
	d, w := m.daily[di], m.weekly[wi]
	p := m.params

	prevLevel := m.level
	m.level = math.Max(0, p.Alpha*(y-d-w)+(1-p.Alpha)*(prevLevel+float64(steps)*m.trend))
	m.trend = p.Beta*(m.level-prevLevel)/float64(steps) + (1-p.Beta)*m.trend

	newD := p.Gamma1*(y-m.level-w) + (1-p.Gamma1)*d
	newW := p.Gamma2*(y-m.level-newD) + (1-p.Gamma2)*w
	m.setDaily(di, newD)
	m.setWeekly(wi, newW)
	m.bound()
	m.n++
}

// bound holds every component within what persisted state may hold. Only a
// model restored from state at those limits comes near them, but one that
// learned its way past them couldn't be restored again.
func (m *Model) bound() {
	m.level = math.Min(m.level, maxMagnitude)
	m.trend = clampMagnitude(m.trend)
	for i := range m.daily {
		m.daily[i] = clampMagnitude(m.daily[i])
	}
	for i := range m.weekly {
		m.weekly[i] = clampMagnitude(m.weekly[i])
	}
}

func clampMagnitude(v float64) float64 {
	return math.Max(-maxMagnitude, math.Min(maxMagnitude, v))
}

func (m *Model) setDaily(i int, v float64) {
	shift := (v - m.daily[i]) / DailySeason
	m.daily[i] = v
	for j := range m.daily {
		m.daily[j] -= shift
	}
	m.level = math.Max(0, m.level+shift)
}

func (m *Model) setWeekly(i int, v float64) {
	hour := i % DailySeason
	shift := (v - m.weekly[i]) / daysPerWeek
	m.weekly[i] = v
	for day := range daysPerWeek {
		m.weekly[day*DailySeason+hour] -= shift
	}
	m.setDaily(hour, m.daily[hour]+shift)
}

// DataPoints is the number of observations the model has fitted.
func (m *Model) DataPoints() int {
	return m.n
}

func (m *Model) finite() bool {
	if !isFinite(m.level) || !isFinite(m.trend) {
		return false
	}
	for _, v := range m.daily {
		if !isFinite(v) {
			return false
		}
	}
	for _, v := range m.weekly {
		if !isFinite(v) {
			return false
		}
	}
	return true
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
