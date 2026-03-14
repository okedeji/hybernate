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

func defaultConstraints() ScaleConstraints {
	return ScaleConstraints{
		MinReplicas: 1,
		MaxReplicas: 100,
	}
}

func confirmSignal() []signal.Checker {
	return []signal.Checker{&fakeChecker{confirm: true, reason: "CPU confirms"}}
}

func denySignal() []signal.Checker {
	return []signal.Checker{&fakeChecker{confirm: false, reason: "CPU does not confirm"}}
}

func TestScaleEvaluate_ScaleUp(t *testing.T) {
	s := NewScaler()

	dec, err := s.Evaluate(context.Background(), "staging", "api", 10, 5, defaultConstraints(), confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(10), dec.Target)
	assert.Equal(t, int32(5), dec.Current)
	assert.Equal(t, ScaleUp, dec.Direction)
	assert.False(t, dec.Clamped)
}

func TestScaleEvaluate_ScaleDown(t *testing.T) {
	s := NewScaler()

	dec, err := s.Evaluate(context.Background(), "staging", "api", 2, 5, defaultConstraints(), confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(2), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)
}

func TestScaleEvaluate_NoChangeWhenProposedMatchesCurrent(t *testing.T) {
	s := NewScaler()

	dec, err := s.Evaluate(context.Background(), "staging", "api", 5, 5, defaultConstraints(), confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(5), dec.Target)
	assert.Equal(t, ScaleNone, dec.Direction)
	assert.Equal(t, "proposed matches current", dec.Reason)
}

func TestScaleEvaluate_SignalDeniesHoldsScale(t *testing.T) {
	s := NewScaler()

	dec, err := s.Evaluate(context.Background(), "staging", "api", 10, 5, defaultConstraints(), denySignal())

	require.NoError(t, err)
	assert.Equal(t, int32(5), dec.Target)
	assert.Equal(t, ScaleNone, dec.Direction)
	assert.Contains(t, dec.Reason, "CPU does not confirm")
}

func TestScaleEvaluate_SignalErrorPropagates(t *testing.T) {
	s := NewScaler()
	signals := []signal.Checker{&fakeChecker{err: fmt.Errorf("prometheus down")}}

	_, err := s.Evaluate(context.Background(), "staging", "api", 10, 5, defaultConstraints(), signals)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus down")
}

func TestScaleEvaluate_ClampsToMax(t *testing.T) {
	s := NewScaler()
	c := ScaleConstraints{MinReplicas: 1, MaxReplicas: 20}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 50, 15, c, confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(20), dec.Target)
	assert.Equal(t, ScaleUp, dec.Direction)
	assert.True(t, dec.Clamped)
}

func TestScaleEvaluate_ClampsToMin(t *testing.T) {
	s := NewScaler()
	c := ScaleConstraints{MinReplicas: 3, MaxReplicas: 100}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 1, 5, c, confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(3), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)
	assert.True(t, dec.Clamped)
}

func TestScaleEvaluate_MaxStepDown(t *testing.T) {
	s := NewScaler()
	c := ScaleConstraints{MinReplicas: 1, MaxReplicas: 100, MaxStepDown: 3}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 2, 10, c, confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(7), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)
	assert.True(t, dec.Clamped)
}

func TestScaleEvaluate_MaxStepUp(t *testing.T) {
	s := NewScaler()
	c := ScaleConstraints{MinReplicas: 1, MaxReplicas: 100, MaxStepUp: 5}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 20, 5, c, confirmSignal())

	require.NoError(t, err)
	assert.Equal(t, int32(10), dec.Target)
	assert.Equal(t, ScaleUp, dec.Direction)
	assert.True(t, dec.Clamped)
}

func TestScaleEvaluate_DownStabilization(t *testing.T) {
	fakeTime := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
	s := NewScaler()
	s.Clock = func() time.Time { return fakeTime }

	c := ScaleConstraints{MinReplicas: 1, MaxReplicas: 100, DownStabilization: 15 * time.Minute}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 3, 10, c, confirmSignal())
	require.NoError(t, err)
	assert.Equal(t, int32(3), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)

	fakeTime = fakeTime.Add(5 * time.Minute)
	dec, err = s.Evaluate(context.Background(), "staging", "api", 2, 3, c, confirmSignal())
	require.NoError(t, err)
	assert.Equal(t, int32(3), dec.Target)
	assert.Equal(t, ScaleNone, dec.Direction)
	assert.Equal(t, "in stabilization window", dec.Reason)

	fakeTime = fakeTime.Add(15 * time.Minute)
	dec, err = s.Evaluate(context.Background(), "staging", "api", 2, 3, c, confirmSignal())
	require.NoError(t, err)
	assert.Equal(t, int32(2), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)
}

func TestScaleEvaluate_UpStabilizationDoesNotBlockDown(t *testing.T) {
	fakeTime := time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)
	s := NewScaler()
	s.Clock = func() time.Time { return fakeTime }

	c := ScaleConstraints{MinReplicas: 1, MaxReplicas: 100, UpStabilization: 15 * time.Minute}

	dec, err := s.Evaluate(context.Background(), "staging", "api", 10, 5, c, confirmSignal())
	require.NoError(t, err)
	assert.Equal(t, ScaleUp, dec.Direction)

	fakeTime = fakeTime.Add(1 * time.Minute)
	dec, err = s.Evaluate(context.Background(), "staging", "api", 3, 10, c, confirmSignal())
	require.NoError(t, err)
	assert.Equal(t, int32(3), dec.Target)
	assert.Equal(t, ScaleDown, dec.Direction)
}

func TestScaleEvaluate_NoSignalsConfirmsByDefault(t *testing.T) {
	s := NewScaler()

	dec, err := s.Evaluate(context.Background(), "staging", "api", 10, 5, defaultConstraints(), nil)

	require.NoError(t, err)
	assert.Equal(t, int32(10), dec.Target)
	assert.Equal(t, ScaleUp, dec.Direction)
}
