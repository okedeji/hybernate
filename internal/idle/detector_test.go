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

package idle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSignal struct {
	active bool
	reason string
	err    error
}

func (f *fakeSignal) Check(_ context.Context, _, _ string) (Result, error) {
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Active: f.active, Reason: f.reason}, nil
}

func TestEvaluate_NoSignals(t *testing.T) {
	d := NewDetector()
	eval, err := d.Evaluate(context.Background(), "default", "api", nil, 30*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, Active, eval.Status)
	assert.Contains(t, eval.Reasons[0], "no signals configured")
}

func TestEvaluate_ActiveSignalShortCircuits(t *testing.T) {
	d := NewDetector()
	signals := []Signal{
		&fakeSignal{active: true, reason: "CPU at 200m"},
		&fakeSignal{active: false, reason: "webhook reports idle"},
	}

	eval, err := d.Evaluate(context.Background(), "default", "api", signals, 30*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, Active, eval.Status)
	assert.Equal(t, []string{"CPU at 200m"}, eval.Reasons)
}

func TestEvaluate_IdleTimerStartsThenExpires(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewDetector()
	d.Clock = func() time.Time { return fakeTime }

	signals := []Signal{
		&fakeSignal{active: false, reason: "CPU at 3m"},
	}
	timeout := 30 * time.Minute

	// First call: starts the idle timer, returns Active
	eval, err := d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, Active, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")

	// 10 minutes later: idle but not long enough
	fakeTime = fakeTime.Add(10 * time.Minute)
	eval, err = d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, Active, eval.Status)
	assert.Equal(t, 10*time.Minute, eval.IdleFor)
	assert.Contains(t, eval.Reasons[0], "waiting for timeout")

	// 30 minutes after start: threshold crossed
	fakeTime = fakeTime.Add(20 * time.Minute)
	eval, err = d.Evaluate(context.Background(), "default", "api", signals, timeout)
	require.NoError(t, err)
	assert.Equal(t, Idle, eval.Status)
	assert.Equal(t, 30*time.Minute, eval.IdleFor)
	assert.Contains(t, eval.Reasons[0], "exceeds timeout")
}

func TestEvaluate_ActiveSignalResetsTimer(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeSignal{active: false, reason: "CPU at 3m"}
	timeout := 30 * time.Minute

	// Start idle timer
	d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)

	// 15 minutes later: still idle
	fakeTime = fakeTime.Add(15 * time.Minute)
	eval, _ := d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Active, eval.Status)
	assert.Equal(t, 15*time.Minute, eval.IdleFor)

	// Activity spike: reset
	activeSignal := &fakeSignal{active: true, reason: "CPU at 500m"}
	eval, _ = d.Evaluate(context.Background(), "default", "api", []Signal{activeSignal}, timeout)
	assert.Equal(t, Active, eval.Status)
	assert.Equal(t, time.Duration(0), eval.IdleFor)

	// Goes idle again: timer restarts from zero
	fakeTime = fakeTime.Add(1 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Active, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")

	// 29 minutes later: not enough (timer restarted)
	fakeTime = fakeTime.Add(29 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Active, eval.Status)

	// 1 more minute: now it's 30 minutes since restart
	fakeTime = fakeTime.Add(1 * time.Minute)
	eval, _ = d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Idle, eval.Status)
}

func TestEvaluate_SignalError(t *testing.T) {
	d := NewDetector()
	signals := []Signal{
		&fakeSignal{err: fmt.Errorf("prometheus unreachable")},
	}

	_, err := d.Evaluate(context.Background(), "default", "api", signals, 30*time.Minute)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus unreachable")
	assert.Contains(t, err.Error(), "default/api")
}

func TestEvaluate_MultipleWorkloadsIndependent(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeSignal{active: false, reason: "idle"}
	timeout := 10 * time.Minute

	// Both workloads start idle
	d.Evaluate(context.Background(), "staging", "api", []Signal{idleSignal}, timeout)
	d.Evaluate(context.Background(), "staging", "db", []Signal{idleSignal}, timeout)

	// 10 minutes later: both should be Idle
	fakeTime = fakeTime.Add(10 * time.Minute)

	evalAPI, _ := d.Evaluate(context.Background(), "staging", "api", []Signal{idleSignal}, timeout)
	evalDB, _ := d.Evaluate(context.Background(), "staging", "db", []Signal{idleSignal}, timeout)

	assert.Equal(t, Idle, evalAPI.Status)
	assert.Equal(t, Idle, evalDB.Status)

	// Reset only api
	d.Reset("staging", "api")

	// api should be active (timer reset), db should still be idle
	fakeTime = fakeTime.Add(1 * time.Minute)
	evalAPI, _ = d.Evaluate(context.Background(), "staging", "api", []Signal{idleSignal}, timeout)
	evalDB, _ = d.Evaluate(context.Background(), "staging", "db", []Signal{idleSignal}, timeout)

	assert.Equal(t, Active, evalAPI.Status)
	assert.Contains(t, evalAPI.Reasons[0], "starting idle timer")
	assert.Equal(t, Idle, evalDB.Status)
}

func TestReset(t *testing.T) {
	fakeTime := time.Date(2026, 3, 13, 14, 0, 0, 0, time.UTC)
	d := NewDetector()
	d.Clock = func() time.Time { return fakeTime }

	idleSignal := &fakeSignal{active: false, reason: "idle"}
	timeout := 5 * time.Minute

	// Start timer
	d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)

	// 5 minutes: idle
	fakeTime = fakeTime.Add(5 * time.Minute)
	eval, _ := d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Idle, eval.Status)

	// Reset
	d.Reset("default", "api")

	// Same time: should restart timer, not be idle
	eval, _ = d.Evaluate(context.Background(), "default", "api", []Signal{idleSignal}, timeout)
	assert.Equal(t, Active, eval.Status)
	assert.Contains(t, eval.Reasons[0], "starting idle timer")
}
