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
	"errors"
	"fmt"
	"math"
	"math/bits"
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

const (
	// maxDemand bounds an observation. No workload uses a million cores, so
	// anything larger is a corrupt metric, and the bound keeps every sum the
	// engine takes finite.
	maxDemand = 1e9

	// demotionMargin is how far below the threshold confidence must fall to
	// lose a phase that was earned at the threshold. Without it, confidence
	// hovering at the threshold would switch the forecast on and off hourly.
	demotionMargin = 5

	// scaleMemory is how many hours the workload's mean demand remembers.
	scaleMemory = WeeklySeason
)

var (
	// ErrInvalidObservation is returned for an observation that is not a
	// finite demand between zero and maxDemand.
	ErrInvalidObservation = errors.New("invalid observation")

	// ErrAlreadyObserved is returned for a second observation of an hour.
	ErrAlreadyObserved = errors.New("hour already observed")
)

// Settings are the engine's configuration, applied on every reconcile
// rather than persisted with what it has learned.
type Settings struct {
	// Threshold is the confidence percentage a season must reach to drive
	// decisions.
	Threshold int

	// Location is where the seasonal slots are counted: the hour of day and
	// day of week of an observation are those of its time in Location. Nil
	// means UTC.
	Location *time.Location
}

// Engine wraps a Model with phase lifecycle management, confidence scoring,
// and anomaly detection. It coordinates the progression from Observing
// through to FullyActive.
//
// Observations are hourly and keyed to the wall clock: the slot an
// observation fills is the hour it was made in, so a gap in observations,
// from a pause or an operator restart, never shifts the seasonality. Missed
// hours are skipped, not backfilled, since there is nothing honest to fill
// them with.
type Engine struct {
	Model   *Model
	Scorer  *Scorer
	Anomaly *AnomalyDetector
	Phase   Phase

	settings Settings

	// lastHour is the Unix time of the start of the last observed hour, in
	// the engine's location, or 0 before any observation.
	lastHour int64

	// scale is the workload's mean demand over about a week.
	scale float64

	// coverage has a bit set for each hour of the week observed since the
	// engine started or last saw its patterns break.
	coverage [3]uint64

	lastRegimeChange bool
	lastAnomaly      bool
}

func NewEngine(params Params, settings Settings) *Engine {
	return &Engine{
		Model:    NewModel(params),
		Scorer:   &Scorer{},
		Anomaly:  &AnomalyDetector{},
		Phase:    Observing,
		settings: settings,
	}
}

// Configure applies new settings. What the engine has learned is kept.
func (e *Engine) Configure(settings Settings) {
	e.settings = settings
}

func (e *Engine) local(t time.Time) time.Time {
	if e.settings.Location == nil {
		return t.UTC()
	}
	return t.In(e.settings.Location)
}

// hourStart is the Unix time at which the local hour containing t began.
// Truncating the absolute time would be wrong in zones offset from UTC by
// a fraction of an hour.
func hourStart(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Unix()
}

// hoursSinceLast is how many hours separate the last observation from t.
func (e *Engine) hoursSinceLast(t time.Time) int {
	if e.lastHour == 0 {
		return 1
	}
	return int(math.Round(float64(hourStart(t)-e.lastHour) / 3600))
}

// Observed reports whether the hour containing now has been observed.
func (e *Engine) Observed(now time.Time) bool {
	return e.lastHour != 0 && hourStart(e.local(now)) <= e.lastHour
}

// Observe feeds the demand seen in the hour containing now and advances
// the phase lifecycle. It returns the forecast that was made for the hour
// before seeing it. A rejected observation leaves the engine unchanged.
func (e *Engine) Observe(actual float64, now time.Time) (float64, error) {
	if math.IsNaN(actual) || actual < 0 || actual > maxDemand {
		return 0, fmt.Errorf("%w: %g", ErrInvalidObservation, actual)
	}
	if e.Observed(now) {
		return 0, ErrAlreadyObserved
	}
	t := e.local(now)

	saved := *e
	model, scorer, anomaly := *e.Model, *e.Scorer, *e.Anomaly

	steps := e.hoursSinceLast(t)
	forecast, fit := actual, actual
	e.lastAnomaly = false
	if e.Model.n > 0 {
		forecast = e.Model.forecast(t, steps)
		e.Scorer.Record(forecast, actual)
		var clipped float64
		e.lastAnomaly, clipped = e.Anomaly.Record(forecast, actual, e.floor())
		fit = math.Max(0, forecast+clipped)
	}
	e.Model.update(fit, t, steps)
	e.lastHour = hourStart(t)
	e.updateScale(actual)
	wi := weeklyIndex(t)
	e.coverage[wi/64] |= 1 << (wi % 64)
	e.advancePhase()

	if !e.Model.finite() || !isFinite(e.scale) {
		*e = saved
		*e.Model, *e.Scorer, *e.Anomaly = model, scorer, anomaly
		return 0, fmt.Errorf("%w: %g made the model diverge", ErrInvalidObservation, actual)
	}
	return forecast, nil
}

func (e *Engine) updateScale(actual float64) {
	n := min(e.Model.n, scaleMemory)
	e.scale += (actual - e.scale) / float64(n)
}

// floor is the least demand per hour accuracy and anomalies are judged
// against: the workload's mean demand, so a quiet day of a busy workload is
// scored against what a typical day asks of it rather than against zero.
func (e *Engine) floor() float64 {
	return math.Max(e.Model.params.Floor, e.scale)
}

// Predict returns the demand forecast for the hour h hours after the one
// containing now. It is 0 until the phase is DailyActive or beyond.
func (e *Engine) Predict(h int, now time.Time) float64 {
	switch e.Phase {
	case DailyActive, WeeklySuggesting, FullyActive:
	default:
		return 0
	}
	t := e.local(now).Add(time.Duration(h) * time.Hour)
	return e.Model.forecast(t, max(0, e.hoursSinceLast(t)))
}

// DailyConfidence is the forecast's accuracy over the last day, as a
// percentage.
func (e *Engine) DailyConfidence() int {
	return int(e.Scorer.Confidence(dailyWindow, e.floor()) * 100)
}

// WeeklyConfidence is the forecast's accuracy over the last week, as a
// percentage. It is the stricter gate: a whole week of weekdays and weekend
// must be forecast well, not just the day just gone.
func (e *Engine) WeeklyConfidence() int {
	return int(e.Scorer.Confidence(weeklyWindow, e.floor()) * 100)
}

func (e *Engine) coveredDay() bool {
	return e.hoursOfDayCovered() == DailySeason
}

func (e *Engine) hoursOfDayCovered() int {
	var seen uint32
	for wi := range WeeklySeason {
		if e.coverage[wi/64]&(1<<(wi%64)) != 0 {
			seen |= 1 << (wi % DailySeason)
		}
	}
	return bits.OnesCount32(seen)
}

func (e *Engine) coveredWeek() bool {
	n := 0
	for _, word := range e.coverage {
		n += bits.OnesCount64(word)
	}
	return n == WeeklySeason
}

func (e *Engine) advancePhase() {
	e.lastRegimeChange = false
	if e.Anomaly.RegimeChange() {
		e.handleRegimeChange()
		return
	}

	threshold := e.settings.Threshold
	dailyLow := e.Scorer.Ready(dailyWindow) && e.DailyConfidence() < threshold-demotionMargin
	weeklyLow := e.Scorer.Ready(weeklyWindow) && e.WeeklyConfidence() < threshold-demotionMargin

	switch e.Phase {
	case Observing:
		if e.coveredDay() {
			e.Phase = DailySuggesting
		}

	case DailySuggesting:
		if e.Scorer.Ready(dailyWindow) && e.DailyConfidence() >= threshold {
			e.Phase = DailyActive
		}

	case DailyActive:
		if dailyLow {
			e.Phase = DailySuggesting
		} else if e.coveredWeek() {
			e.Phase = WeeklySuggesting
		}

	case WeeklySuggesting:
		if dailyLow {
			e.Phase = DailySuggesting
		} else if e.Scorer.Ready(weeklyWindow) && e.WeeklyConfidence() >= threshold {
			e.Phase = FullyActive
		}

	case FullyActive:
		if dailyLow {
			e.Phase = DailySuggesting
		} else if weeklyLow {
			e.Phase = WeeklySuggesting
		}
	}
}

// handleRegimeChange demotes the engine one step and discards the evidence
// its confidence rested on, so that confidence is earned again on the new
// pattern. Clearing the anomalies means one regime change demotes once,
// not once for every hour the anomalies stay in the window.
func (e *Engine) handleRegimeChange() {
	e.lastRegimeChange = true
	switch e.Phase {
	case FullyActive:
		e.Phase = WeeklySuggesting
	case WeeklySuggesting, DailyActive:
		e.Phase = DailySuggesting
	case DailySuggesting:
		e.Phase = Observing
	}
	e.Scorer.Reset()
	e.Anomaly.Reset()
	e.coverage = [3]uint64{}
}

func (e *Engine) GetPhase() Phase    { return e.Phase }
func (e *Engine) GetDataPoints() int { return e.Model.DataPoints() }

// RegimeChanged reports whether the last observation broke the learned
// patterns and demoted the engine.
func (e *Engine) RegimeChanged() bool { return e.lastRegimeChange }

// AnomalyDetected reports whether the last observation was anomalous.
func (e *Engine) AnomalyDetected() bool { return e.lastAnomaly }
