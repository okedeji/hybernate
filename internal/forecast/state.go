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
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// stateVersion 2 is the additive model. State of any other version is
// rejected rather than migrated: an engine relearns in days.
const stateVersion = 2

// maxStateBytes bounds the decompressed state, so a hand-edited state
// can't expand into gigabytes. A real one is a few kilobytes.
const maxStateBytes = 64 << 10

// maxMagnitude bounds every value in a valid state. A model fitted to
// observations up to maxDemand stays far within it, and anything within it
// keeps the engine's sums finite.
const maxMagnitude = 1e6 * maxDemand

// maxLastHour is the year 4000, past which a last observed hour is corrupt.
var maxLastHour = time.Date(4000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

// ErrInvalidState is returned for persisted state that can't be restored.
var ErrInvalidState = errors.New("invalid forecast state")

// engineState is what is persisted of an engine, in the ManagedWorkload's
// status. Keys are short and the seasonal components and errors are single
// precision, since the state is written with every hourly observation.
type engineState struct {
	Version  int                   `json:"v"`
	Phase    int                   `json:"p"`
	N        int                   `json:"n"`
	LastHour int64                 `json:"lh"`
	Level    float64               `json:"l"`
	Trend    float64               `json:"t"`
	Daily    [DailySeason]float32  `json:"d"`
	Weekly   [WeeklySeason]float32 `json:"w"`
	Scale    float64               `json:"s"`
	Coverage weekSlots             `json:"c"`
	AbsErr   []float32             `json:"ae"`
	Actual   []float32             `json:"ay"`
	AnMean   float64               `json:"am"`
	AnVar    float64               `json:"av"`
	AnCount  int                   `json:"ac"`
	AnRecent uint32                `json:"ar"`
	AnAbove  weekSlots             `json:"aa"`
	AnBelow  weekSlots             `json:"ab"`
}

// Export encodes what the engine has learned as compact text: gzipped JSON
// in base64. Settings aren't included; they are applied afresh.
func (e *Engine) Export() (string, error) {
	st := engineState{
		Version:  stateVersion,
		Phase:    int(e.Phase),
		N:        e.Model.n,
		LastHour: e.lastHour,
		Level:    e.Model.level,
		Trend:    e.Model.trend,
		Scale:    e.scale,
		Coverage: e.coverage,
		AnMean:   e.Anomaly.mean,
		AnVar:    e.Anomaly.vari,
		AnCount:  e.Anomaly.count,
		AnRecent: e.Anomaly.recent,
		AnAbove:  e.Anomaly.above,
		AnBelow:  e.Anomaly.below,
	}
	for i, v := range e.Model.daily {
		st.Daily[i] = float32(v)
	}
	for i, v := range e.Model.weekly {
		st.Weekly[i] = float32(v)
	}
	absErr, actual := e.Scorer.ordered()
	st.AbsErr, st.Actual = toFloat32s(absErr), toFloat32s(actual)

	data, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("encoding forecast state: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return "", fmt.Errorf("compressing forecast state: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("compressing forecast state: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ImportEngine restores an engine from text produced by Export, with the
// default parameters and the given settings. State that is corrupt, of
// another version, or inconsistent is rejected with ErrInvalidState.
func ImportEngine(state string, settings Settings) (*Engine, error) {
	compressed, err := base64.StdEncoding.DecodeString(state)
	if err != nil {
		return nil, fmt.Errorf("%w: decoding base64: %w", ErrInvalidState, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("%w: decompressing: %w", ErrInvalidState, err)
	}
	data, err := io.ReadAll(io.LimitReader(zr, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: decompressing: %w", ErrInvalidState, err)
	}
	if len(data) > maxStateBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrInvalidState, maxStateBytes)
	}
	return decodeEngine(data, settings)
}

func decodeEngine(data []byte, settings Settings) (*Engine, error) {
	var st engineState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w: unmarshaling: %w", ErrInvalidState, err)
	}
	if err := st.validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidState, err)
	}

	e := NewEngine(DefaultParams(), settings)
	e.Phase = Phase(st.Phase)
	e.lastHour = st.LastHour
	e.scale = st.Scale
	e.coverage = st.Coverage
	e.Model.n = st.N
	e.Model.level = st.Level
	e.Model.trend = st.Trend
	for i, v := range st.Daily {
		e.Model.daily[i] = float64(v)
	}
	for i, v := range st.Weekly {
		e.Model.weekly[i] = float64(v)
	}
	for i := range st.AbsErr {
		e.Scorer.push(float64(st.AbsErr[i]), float64(st.Actual[i]))
	}
	e.Anomaly.mean = st.AnMean
	e.Anomaly.vari = st.AnVar
	e.Anomaly.count = st.AnCount
	e.Anomaly.recent = st.AnRecent
	e.Anomaly.above = st.AnAbove
	e.Anomaly.below = st.AnBelow
	return e, nil
}

func (st *engineState) validate() error {
	switch {
	case st.Version != stateVersion:
		return fmt.Errorf("unsupported version %d (expected %d)", st.Version, stateVersion)
	case st.Phase < int(Observing) || st.Phase > int(FullyActive):
		return fmt.Errorf("phase %d out of range", st.Phase)
	case st.N < 0:
		return fmt.Errorf("negative data points %d", st.N)
	case (st.N == 0) != (st.LastHour == 0):
		return errors.New("data points and last observed hour disagree")
	case st.LastHour < 0 || st.LastHour > maxLastHour:
		return fmt.Errorf("last observed hour %d out of range", st.LastHour)
	case !st.Coverage.valid() || !st.AnAbove.valid() || !st.AnBelow.valid():
		return errors.New("hours beyond the hours of a week")
	case len(st.AbsErr) != len(st.Actual) || len(st.AbsErr) > weeklyWindow:
		return fmt.Errorf("error window of %d and %d hours", len(st.AbsErr), len(st.Actual))
	case st.AnCount < 0 || st.AnCount > anomalyMemory:
		return fmt.Errorf("anomaly count %d out of range", st.AnCount)
	case st.AnRecent > anomalyWindowMask:
		return errors.New("anomalies beyond the anomaly window")
	}

	bounded := func(v float64) bool { return isFinite(v) && math.Abs(v) <= maxMagnitude }
	nonNegative := func(v float64) bool { return bounded(v) && v >= 0 }
	if !nonNegative(st.Level) || !bounded(st.Trend) || !nonNegative(st.Scale) || !bounded(st.AnMean) ||
		!isFinite(st.AnVar) || st.AnVar < 0 || st.AnVar > maxMagnitude*maxMagnitude {
		return errors.New("level, trend, scale, or error statistics out of range")
	}
	for _, v := range st.Daily {
		if !bounded(float64(v)) {
			return errors.New("daily component out of range")
		}
	}
	for _, v := range st.Weekly {
		if !bounded(float64(v)) {
			return errors.New("weekly component out of range")
		}
	}
	for i := range st.AbsErr {
		if !nonNegative(float64(st.AbsErr[i])) || !nonNegative(float64(st.Actual[i])) {
			return errors.New("recorded error out of range")
		}
	}
	return nil
}

func toFloat32s(vs []float64) []float32 {
	out := make([]float32, len(vs))
	for i, v := range vs {
		out[i] = float32(v)
	}
	return out
}
