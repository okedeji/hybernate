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

// Holt-Winters double seasonal smoothing (Taylor's method).
//
// All updates follow the same exponential smoothing form:
//
//	new = weight × (fresh evidence) + (1 - weight) × (old belief)
//
// Each component strips out the effects of the others before updating:
//
//	Level:   L(t) = α × (Y(t) / (D(t) × W(t)))  + (1-α) × (L(t-1) + T(t-1))
//	Trend:   T(t) = β × (L(t) - L(t-1))          + (1-β) × T(t-1)
//	Daily:   D(t) = γ₁ × (Y(t) / (L(t) × W(t))) + (1-γ₁) × D(t-s₁)
//	Weekly:  W(t) = γ₂ × (Y(t) / (L(t) × D(t))) + (1-γ₂) × W(t-s₂)
//
// Forecast h steps ahead:
//
//	F(t+h) = (L(t) + h × T(t)) × D(t+h) × W(t+h)
//
// Where:
//   - Y(t) = observed value at time t
//   - s₁ = 24 (daily season), s₂ = 168 (weekly season)
//   - α, β, γ₁, γ₂ = smoothing parameters in (0, 1)


package predict

const (
	DailySeason  = 24
	WeeklySeason = 168
)

// Params controls how quickly the model adapts to new data.
// Higher values = more reactive. Lower values = more stable.
type Params struct {
	Alpha  float64 // level smoothing (default 0.1)
	Beta   float64 // trend smoothing (default 0.01)
	Gamma1 float64 // daily seasonality smoothing (default 0.05)
	Gamma2 float64 // weekly seasonality smoothing (default 0.01)
}

func DefaultParams() Params {
	return Params{
		Alpha:  0.1,
		Beta:   0.01,
		Gamma1: 0.05,
		Gamma2: 0.01,
	}
}

// Model implements Holt-Winters double seasonal smoothing (Taylor's method).
// It learns daily (24h) and weekly (168h) patterns from hourly data points
// and forecasts future demand.
type Model struct {
	params Params

	level  float64
	trend  float64
	daily  [DailySeason]float64
	weekly [WeeklySeason]float64

	n int // total data points observed
}

func NewModel(params Params) *Model {
	m := &Model{params: params}

	// Initialize seasonal factors to 1.0 (no effect until data arrives)
	for i := range m.daily {
		m.daily[i] = 1.0
	}
	for i := range m.weekly {
		m.weekly[i] = 1.0
	}

	return m
}

// Update feeds a new hourly data point into the model and returns the
// forecast that was made for this hour (before seeing the actual value).
// The caller can compare forecast vs actual to score accuracy.
func (m *Model) Update(value float64) float64 {
	m.n++

	if m.n == 1 {
		m.level = value
		return value
	}

	di := (m.n - 1) % DailySeason
	wi := (m.n - 1) % WeeklySeason

	prevForecast := m.Forecast(1)

	ds := m.daily[di]
	ws := m.weekly[wi]

	// Protect against division by zero when seasonal factors are zero
	seasonProduct := ds * ws
	if seasonProduct == 0 {
		seasonProduct = 1.0
	}

	prevLevel := m.level
	prevTrend := m.trend

	// Level: baseline demand stripped of seasonal effects
	m.level = m.params.Alpha*(value/seasonProduct) +
		(1-m.params.Alpha)*(prevLevel+prevTrend)

	// Trend: direction demand is moving
	m.trend = m.params.Beta*(m.level-prevLevel) +
		(1-m.params.Beta)*prevTrend

	// Daily seasonality: only update after first full day
	if m.n > DailySeason {
		levelWeekly := m.level * ws
		if levelWeekly == 0 {
			levelWeekly = 1.0
		}
		m.daily[di] = m.params.Gamma1*(value/levelWeekly) +
			(1-m.params.Gamma1)*ds
	}

	// Weekly seasonality: only update after first full week
	if m.n > WeeklySeason {
		levelDaily := m.level * ds
		if levelDaily == 0 {
			levelDaily = 1.0
		}
		m.weekly[wi] = m.params.Gamma2*(value/levelDaily) +
			(1-m.params.Gamma2)*ws
	}

	return prevForecast
}

// Forecast predicts demand h hours ahead based on current state.
func (m *Model) Forecast(h int) float64 {
	di := (m.n - 1 + h) % DailySeason
	wi := (m.n - 1 + h) % WeeklySeason

	f := (m.level + float64(h)*m.trend) * m.daily[di] * m.weekly[wi]
	if f < 0 {
		return 0
	}
	return f
}

// DataPoints returns the total number of data points the model has observed.
func (m *Model) DataPoints() int {
	return m.n
}
