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

package signal

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeChecker struct {
	confirm bool
	reason  string
	err     error
}

func (f *fakeChecker) Check(_ context.Context, _, _ string) (Result, error) {
	if f.err != nil {
		return Result{}, f.err
	}
	return Result{Confirm: f.confirm, Reason: f.reason}, nil
}

func TestCheckAll_AllConfirm(t *testing.T) {
	signals := []Checker{
		&fakeChecker{confirm: true, reason: "CPU low"},
		&fakeChecker{confirm: true, reason: "promql confirms"},
	}

	res, err := CheckAll(context.Background(), "staging", "api", signals)

	require.NoError(t, err)
	assert.True(t, res.Confirm)
	assert.Equal(t, "all signals confirm", res.Reason)
}

func TestCheckAll_OneDenies(t *testing.T) {
	signals := []Checker{
		&fakeChecker{confirm: true, reason: "CPU low"},
		&fakeChecker{confirm: false, reason: "promql value is non-zero"},
	}

	res, err := CheckAll(context.Background(), "staging", "api", signals)

	require.NoError(t, err)
	assert.False(t, res.Confirm)
	assert.Contains(t, res.Reason, "signal denied")
	assert.Contains(t, res.Reason, "promql value is non-zero")
}

func TestCheckAll_FirstDeniesShortCircuits(t *testing.T) {
	second := &trackingChecker{result: Result{Confirm: true, Reason: "should not run"}}
	signals := []Checker{
		&fakeChecker{confirm: false, reason: "CPU high"},
		second,
	}

	res, err := CheckAll(context.Background(), "staging", "api", signals)

	require.NoError(t, err)
	assert.False(t, res.Confirm)
	assert.False(t, second.called)
}

type trackingChecker struct {
	result Result
	called bool
}

func (t *trackingChecker) Check(_ context.Context, _, _ string) (Result, error) {
	t.called = true
	return t.result, nil
}

func TestCheckAll_ErrorPropagates(t *testing.T) {
	signals := []Checker{
		&fakeChecker{err: fmt.Errorf("prometheus unreachable")},
	}

	_, err := CheckAll(context.Background(), "staging", "api", signals)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus unreachable")
	assert.Contains(t, err.Error(), "staging/api")
}

func TestCheckAll_NoSignals(t *testing.T) {
	res, err := CheckAll(context.Background(), "staging", "api", nil)

	require.NoError(t, err)
	assert.True(t, res.Confirm)
	assert.Equal(t, "all signals confirm", res.Reason)
}
