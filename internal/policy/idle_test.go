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

package policy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/hybernate/internal/signal"
)

type fakeChecker struct {
	confirm bool
	reason  string
	err     error
}

func (f *fakeChecker) Check(_ context.Context, _, _ string) (signal.Result, error) {
	if f.err != nil {
		return signal.Result{}, f.err
	}
	return signal.Result{Confirm: f.confirm, Reason: f.reason}, nil
}

func TestIdleEvaluate_NoSignals(t *testing.T) {
	d := NewIdleDetector()
	eval, err := d.Evaluate(context.Background(), "default", "api", nil, 30*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Contains(t, eval.Reasons[0], "no signals configured")
}

func TestIdleEvaluate_DeniedSignalShortCircuits(t *testing.T) {
	d := NewIdleDetector()
	signals := []signal.Checker{
		&fakeChecker{confirm: false, reason: "CPU at 200m"},
		&fakeChecker{confirm: true, reason: "promql confirms idle"},
	}

	eval, err := d.Evaluate(context.Background(), "default", "api", signals, 30*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Contains(t, eval.Reasons[0], "CPU at 200m")
}

func TestIdleEvaluate_TimerStartsThenExpires(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewIdleDetector()
	d.Clock = func() time.Time { return fakeTime }

	signals := []signal.Checker{
		&fakeChecker{confirm: true, reason: "CPU at 3m"},
	}
	timeout := 30 * time.Minute

	eval, err := d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")

	fakeTime = fakeTime.Add(10 * time.Minute)
	eval, err = d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Equal(t, 10*time.Minute, eval.IdleFor)
	assert.Contains(t, eval.Reasons[0], "waiting for timeout")

	fakeTime = fakeTime.Add(20 * time.Minute)
	eval, err = d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, IdleStatusIdle, eval.Status)
	assert.Equal(t, 30*time.Minute, eval.IdleFor)
	assert.Contains(t, eval.Reasons[0], "exceeds timeout")
}

func TestIdleEvaluate_DeniedSignalResetsTimer(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewIdleDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeChecker{confirm: true, reason: "CPU at 3m"}
	timeout := 30 * time.Minute

	d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)

	fakeTime = fakeTime.Add(15 * time.Minute)
	eval, _ := d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Equal(t, 15*time.Minute, eval.IdleFor)

	activeSignal := &fakeChecker{confirm: false, reason: "CPU at 500m"}
	eval, _ = d.Evaluate(context.Background(), "default", "api", []signal.Checker{activeSignal}, timeout)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Equal(t, time.Duration(0), eval.IdleFor)

	fakeTime = fakeTime.Add(1 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")

	fakeTime = fakeTime.Add(29 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusActive, eval.Status)

	fakeTime = fakeTime.Add(1 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusIdle, eval.Status)
}

func TestIdleEvaluate_SignalError(t *testing.T) {
	d := NewIdleDetector()
	signals := []signal.Checker{
		&fakeChecker{err: fmt.Errorf("prometheus unreachable")},
	}

	_, err := d.Evaluate(context.Background(), "default", "api", signals, 30*time.Minute)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus unreachable")
	assert.Contains(t, err.Error(), "default/api")
}

func TestIdleEvaluate_MultipleWorkloadsIndependent(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewIdleDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeChecker{confirm: true, reason: "idle"}
	timeout := 10 * time.Minute

	d.Evaluate(context.Background(), "staging", "api", []signal.Checker{idleSignal}, timeout)
	d.Evaluate(context.Background(), "staging", "db", []signal.Checker{idleSignal}, timeout)

	fakeTime = fakeTime.Add(10 * time.Minute)

	evalAPI, _ := d.Evaluate(context.Background(), "staging", "api", []signal.Checker{idleSignal}, timeout)
	evalDB, _ := d.Evaluate(context.Background(), "staging", "db", []signal.Checker{idleSignal}, timeout)

	assert.Equal(t, IdleStatusIdle, evalAPI.Status)
	assert.Equal(t, IdleStatusIdle, evalDB.Status)

	d.Reset("staging", "api")

	fakeTime = fakeTime.Add(1 * time.Minute)
	evalAPI, _ = d.Evaluate(context.Background(), "staging", "api", []signal.Checker{idleSignal}, timeout)
	evalDB, _ = d.Evaluate(context.Background(), "staging", "db", []signal.Checker{idleSignal}, timeout)

	assert.Equal(t, IdleStatusActive, evalAPI.Status)
	assert.Contains(t, evalAPI.Reasons[0], "starting idle timer")
	assert.Equal(t, IdleStatusIdle, evalDB.Status)
}

func TestIdleReset(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewIdleDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeChecker{confirm: true, reason: "idle"}
	timeout := 5 * time.Minute

	d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)

	fakeTime = fakeTime.Add(5 * time.Minute)
	eval, _ := d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusIdle, eval.Status)

	d.Reset("default", "api")

	eval, _ = d.Evaluate(context.Background(), "default", "api", []signal.Checker{idleSignal}, timeout)
	assert.Equal(t, IdleStatusActive, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")
}
