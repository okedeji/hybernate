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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func encodeState(t testing.TB, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func trainedEngine(t testing.TB) *Engine {
	t.Helper()
	e := newTestEngine()
	for i := range 5 * WeeklySeason {
		at := hourAt(i)
		_, err := e.Observe(officeHours(at)*0.9+float64(i%7), at)
		require.NoError(t, err)
	}
	return e
}

func TestState_RoundTrip(t *testing.T) {
	e := trainedEngine(t)
	require.Equal(t, FullyActive, e.Phase)

	state, err := e.Export()
	require.NoError(t, err)
	t.Logf("state of a trained engine is %d bytes", len(state))
	assert.Less(t, len(state), 4096, "the state is written with every hourly observation")

	restored, err := ImportEngine(state, Settings{Threshold: 85})
	require.NoError(t, err)

	now := hourAt(5 * WeeklySeason)
	assert.Equal(t, e.Phase, restored.Phase)
	assert.Equal(t, e.GetDataPoints(), restored.GetDataPoints())
	assert.InDelta(t, e.DailyConfidence(), restored.DailyConfidence(), 1)
	assert.InDelta(t, e.WeeklyConfidence(), restored.WeeklyConfidence(), 1)
	for h := range DailySeason {
		assert.InDelta(t, e.Predict(h, now), restored.Predict(h, now), 0.01, "hour %d", h)
	}
	assert.True(t, restored.Observed(hourAt(5*WeeklySeason-1).Add(30*time.Minute)),
		"the last observed hour survives a restart, so it isn't fed twice")

	again, err := restored.Export()
	require.NoError(t, err)
	assert.Equal(t, state, again, "an engine that hasn't changed exports the same state")
}

func TestState_ImportRejectsInvalidState(t *testing.T) {
	valid := func() engineState {
		e := trainedEngine(t)
		state, err := e.Export()
		require.NoError(t, err)
		zr, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(state)))
		require.NoError(t, err)
		var st engineState
		require.NoError(t, json.NewDecoder(zr).Decode(&st))
		return st
	}

	tests := []struct {
		name   string
		mutate func(st *engineState)
	}{
		{"old version", func(st *engineState) { st.Version = 1 }},
		{"phase out of range", func(st *engineState) { st.Phase = 9 }},
		{"negative phase", func(st *engineState) { st.Phase = -1 }},
		{"negative data points", func(st *engineState) { st.N = -1 }},
		{"data points without a last hour", func(st *engineState) { st.LastHour = 0 }},
		{"last hour without data points", func(st *engineState) { st.N = 0 }},
		{"negative level", func(st *engineState) { st.Level = -1 }},
		{"huge trend", func(st *engineState) { st.Trend = 1e300 }},
		{"negative scale", func(st *engineState) { st.Scale = -5 }},
		{"negative variance", func(st *engineState) { st.AnVar = -1 }},
		{"anomaly count out of range", func(st *engineState) { st.AnCount = anomalyMemory + 1 }},
		{"anomalies beyond the window", func(st *engineState) { st.AnRecent = 1 << anomalyWindow }},
		{"coverage beyond the week", func(st *engineState) { st.Coverage[2] |= 1 << 63 }},
		{"error window too long", func(st *engineState) {
			st.AbsErr = make([]float32, weeklyWindow+1)
			st.Actual = make([]float32, weeklyWindow+1)
		}},
		{"error windows disagree", func(st *engineState) { st.Actual = st.Actual[:len(st.Actual)-1] }},
		{"negative error", func(st *engineState) { st.AbsErr[0] = -1 }},
		{"infinite weekly component", func(st *engineState) { st.Weekly[3] = float32(math.Inf(1)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := valid()
			tt.mutate(&st)
			data, err := json.Marshal(st)
			if err != nil {
				return // not representable as JSON, so it can't be persisted either
			}

			_, err = ImportEngine(encodeState(t, data), Settings{})
			require.ErrorIs(t, err, ErrInvalidState)
		})
	}
}

func TestState_ImportRejectsMalformedText(t *testing.T) {
	bomb := encodeState(t, bytes.Repeat([]byte(" "), maxStateBytes+1))
	for name, state := range map[string]string{
		"not base64":       "%%%",
		"not gzip":         base64.StdEncoding.EncodeToString([]byte("plain")),
		"not json":         encodeState(t, []byte("not json")),
		"bare version":     encodeState(t, []byte(`{"v":2}`)),
		"version 1":        encodeState(t, []byte(`{"v":1}`)),
		"empty slices":     encodeState(t, []byte(`{"v":1,"ds":{"w":24,"e":[]},"ws":{"w":24,"e":[]}}`)),
		"decompresses big": bomb,
	} {
		t.Run(name, func(t *testing.T) {
			e, err := ImportEngine(state, Settings{})
			if name == "bare version" {
				// An empty engine is valid state: nothing observed yet.
				require.NoError(t, err)
				assert.Equal(t, Observing, e.Phase)
				return
			}
			require.ErrorIs(t, err, ErrInvalidState)
		})
	}
}

// exercise drives an engine through everything a reconcile does with it,
// checking it stays sound.
func exercise(t *testing.T, e *Engine) {
	start := time.Unix(e.lastHour, 0).Add(time.Hour)
	if e.lastHour == 0 {
		start = testEpoch
	}
	for i := range 30 {
		at := start.Add(time.Duration(i) * time.Hour)
		_, err := e.Observe(float64(i%5)*100, at)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidObservation)
		}
		for h := range 2 {
			p := e.Predict(h, at)
			require.False(t, math.IsNaN(p) || math.IsInf(p, 0) || p < 0, "predict %g", p)
		}
		d, w := e.DailyConfidence(), e.WeeklyConfidence()
		require.True(t, d >= 0 && d <= 100 && w >= 0 && w <= 100, "confidence %d %d", d, w)
	}
	require.True(t, e.Model.finite())
	require.GreaterOrEqual(t, e.Model.level, 0.0)
	state, err := e.Export()
	require.NoError(t, err)
	_, err = ImportEngine(state, Settings{Threshold: 85})
	require.NoError(t, err, "an engine imports what it exported")
}

// An engine restored from state at the edge of what import accepts mustn't
// learn its way past it: the next restart would discard everything.
func TestState_ExtremeStateStaysImportable(t *testing.T) {
	for _, sign := range []float64{1, -1} {
		st := engineState{
			Version:  stateVersion,
			Phase:    int(FullyActive),
			N:        1000,
			LastHour: testEpoch.Unix(),
			Level:    maxMagnitude,
			Trend:    sign * maxMagnitude,
			AnMean:   sign * maxMagnitude,
			AnVar:    maxMagnitude * maxMagnitude,
			AnCount:  anomalyMemory,
		}
		for i := range st.Daily {
			st.Daily[i] = float32(sign * maxMagnitude * 0.99)
		}
		for i := range st.Weekly {
			st.Weekly[i] = float32(-sign * maxMagnitude * 0.99)
		}
		data, err := json.Marshal(st)
		require.NoError(t, err)
		e, err := decodeEngine(data, Settings{Threshold: 85})
		require.NoError(t, err)

		exercise(t, e)
	}
}

func FuzzDecodeEngine(f *testing.F) {
	e := trainedEngine(f)
	st := engineState{Version: stateVersion}
	empty, err := json.Marshal(st)
	require.NoError(f, err)
	f.Add(empty)
	f.Add([]byte(`{"v":2,"p":4,"n":1,"lh":1,"ae":[1],"ay":[2]}`))
	f.Add([]byte(`{"v":1}`))
	state, err := e.Export()
	require.NoError(f, err)
	zr, err := gzip.NewReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(state)))
	require.NoError(f, err)
	var buf bytes.Buffer
	_, err = buf.ReadFrom(zr)
	require.NoError(f, err)
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := decodeEngine(data, Settings{Threshold: 85})
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidState)
			return
		}
		exercise(t, e)
	})
}

func FuzzImportEngine(f *testing.F) {
	state, err := trainedEngine(f).Export()
	require.NoError(f, err)
	f.Add(state)
	f.Add("")
	f.Add("H4sIAAAAAAAA/w==")

	f.Fuzz(func(t *testing.T, state string) {
		e, err := ImportEngine(state, Settings{Threshold: 85})
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidState)
			return
		}
		exercise(t, e)
	})
}
