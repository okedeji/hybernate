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
	"encoding/json"
	"fmt"
	"time"
)

type Phase int

const (
	Observing Phase = iota
	DailySuggesting
	DailyActive
	WeeklySuggesting
	FullyActive
)

func (p Phase) String() string {
	switch p {
	case Observing:
		return "Observing"
	case DailySuggesting:
		return "DailySuggesting"
	case DailyActive:
		return "DailyActive"
	case WeeklySuggesting:
		return "WeeklySuggesting"
	case FullyActive:
		return "FullyActive"
	default:
		return "Unknown"
	}
}

// Engine wraps a Model with phase lifecycle management, confidence scoring,
// and anomaly detection. It coordinates the progression from Observing
// through to FullyActive.
type Engine struct {
	Model        *Model
	DailyScorer  *Scorer
	WeeklyScorer *Scorer
	Anomaly      *AnomalyDetector
	Phase        Phase
	Threshold    int // confidence threshold as percentage (0-100)

	dailyDemoted     bool
	weeklyDemoted    bool
	lastRegimeChange bool
	lastAnomaly      bool
}

func NewEngine(params Params, threshold int) *Engine {
	return &Engine{
		Model:        NewModel(params),
		DailyScorer:  NewScorer(),
		WeeklyScorer: NewScorer(),
		Anomaly:      NewAnomalyDetector(),
		Phase:        Observing,
		Threshold:    threshold,
	}
}

// Observe feeds a new hourly data point and advances the phase lifecycle.
// Returns the forecast that was made before seeing the actual value.
func (e *Engine) Observe(actual float64, now time.Time) float64 {
	forecast := e.Model.Update(actual, now)
	n := e.Model.DataPoints()

	if n > DailySeason {
		e.DailyScorer.Record(forecast, actual)
	}
	if n > WeeklySeason {
		e.WeeklyScorer.Record(forecast, actual)
	}

	e.lastAnomaly = e.Anomaly.Record(forecast, actual)

	e.advancePhase()

	return forecast
}

// Predict returns the demand forecast h hours ahead. Only returns a non-zero
// value when the phase is DailyActive or beyond.
func (e *Engine) Predict(h int, now time.Time) float64 {
	switch e.Phase {
	case DailyActive, WeeklySuggesting, FullyActive:
		return e.Model.Forecast(h, now)
	default:
		return 0
	}
}

// DailyConfidence returns the daily scorer's confidence as a percentage (0-100).
func (e *Engine) DailyConfidence() int {
	return int(e.DailyScorer.Confidence() * 100)
}

// WeeklyConfidence returns the weekly scorer's confidence as a percentage (0-100).
func (e *Engine) WeeklyConfidence() int {
	return int(e.WeeklyScorer.Confidence() * 100)
}

func (e *Engine) advancePhase() {
	e.lastRegimeChange = false
	n := e.Model.DataPoints()

	if e.Anomaly.RegimeChange() {
		e.handleRegimeChange()
		return
	}

	switch e.Phase {
	case Observing:
		if n >= DailySeason {
			e.Phase = DailySuggesting
		}

	case DailySuggesting:
		if e.DailyScorer.Ready() && e.DailyConfidence() >= e.Threshold {
			e.Phase = DailyActive
			e.dailyDemoted = false
		}

	case DailyActive:
		if e.DailyScorer.Ready() && e.DailyConfidence() < e.Threshold {
			e.Phase = DailySuggesting
			e.dailyDemoted = true
		} else if n >= WeeklySeason {
			e.Phase = WeeklySuggesting
		}

	case WeeklySuggesting:
		if e.DailyScorer.Ready() && e.DailyConfidence() < e.Threshold {
			e.Phase = DailySuggesting
			e.dailyDemoted = true
		} else if e.WeeklyScorer.Ready() && e.WeeklyConfidence() >= e.Threshold {
			e.Phase = FullyActive
			e.weeklyDemoted = false
		}

	case FullyActive:
		if e.DailyScorer.Ready() && e.DailyConfidence() < e.Threshold {
			e.Phase = DailySuggesting
			e.dailyDemoted = true
		} else if e.WeeklyScorer.Ready() && e.WeeklyConfidence() < e.Threshold {
			e.Phase = WeeklySuggesting
			e.weeklyDemoted = true
		}
	}
}

func (e *Engine) GetPhase() Phase        { return e.Phase }
func (e *Engine) GetDataPoints() int      { return e.Model.DataPoints() }
func (e *Engine) GetThreshold() int       { return e.Threshold }

func (e *Engine) RegimeChanged() bool    { return e.lastRegimeChange }
func (e *Engine) AnomalyDetected() bool  { return e.lastAnomaly }

func (e *Engine) handleRegimeChange() {
	e.lastRegimeChange = true
	switch e.Phase {
	case FullyActive:
		e.Phase = WeeklySuggesting
		e.weeklyDemoted = true
	case WeeklySuggesting, DailyActive:
		e.Phase = DailySuggesting
		e.dailyDemoted = true
	case DailySuggesting:
		e.Phase = Observing
	}
}

// Export serializes the full engine state to JSON for persistence in the CR status.
func (e *Engine) Export() ([]byte, error) {
	st := EngineState{
		Version: stateVersion,
		Model:   e.Model.export(),
		Daily:   e.DailyScorer.export(),
		Weekly:  e.WeeklyScorer.export(),
		Anomaly: e.Anomaly.export(),
		Phase:   int(e.Phase),
		Thresh:  e.Threshold,
		DDemote: e.dailyDemoted,
		WDemote: e.weeklyDemoted,
	}
	return json.Marshal(st)
}

// ImportEngine restores an engine from JSON produced by Export.
// Returns an error if the data is corrupt or the version is unsupported.
func ImportEngine(data []byte) (*Engine, error) {
	var st EngineState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshaling engine state: %w", err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d (expected %d)", st.Version, stateVersion)
	}
	return &Engine{
		Model:         importModel(st.Model),
		DailyScorer:   importScorer(st.Daily),
		WeeklyScorer:  importScorer(st.Weekly),
		Anomaly:       importAnomalyDetector(st.Anomaly),
		Phase:         Phase(st.Phase),
		Threshold:     st.Thresh,
		dailyDemoted:  st.DDemote,
		weeklyDemoted: st.WDemote,
	}, nil
}
