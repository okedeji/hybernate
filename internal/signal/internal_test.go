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
	"k8s.io/apimachinery/pkg/api/resource"
)

type fakeMetrics struct {
	cpu resource.Quantity
	err error
}

func (f *fakeMetrics) CPUUsage(_ context.Context, _, _ string) (resource.Quantity, error) {
	if f.err != nil {
		return resource.Quantity{}, f.err
	}
	return f.cpu, nil
}

func TestInternal_BelowThresholdConfirms(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("3m")}
	s := NewInternal(m, resource.MustParse("50m"), Below)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.True(t, res.Confirm)
	assert.Contains(t, res.Reason, "3m")
	assert.Contains(t, res.Reason, "50m")
}

func TestInternal_AtThresholdConfirmsBelow(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("50m")}
	s := NewInternal(m, resource.MustParse("50m"), Below)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.True(t, res.Confirm)
}

func TestInternal_AboveThresholdDeniesBelow(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("200m")}
	s := NewInternal(m, resource.MustParse("50m"), Below)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Confirm)
	assert.Contains(t, res.Reason, "200m")
}

func TestInternal_AboveThresholdConfirmsAbove(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("900m")}
	s := NewInternal(m, resource.MustParse("800m"), Above)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.True(t, res.Confirm)
	assert.Contains(t, res.Reason, "900m")
}

func TestInternal_AtThresholdDeniesAbove(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("800m")}
	s := NewInternal(m, resource.MustParse("800m"), Above)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Confirm)
}

func TestInternal_BelowThresholdDeniesAbove(t *testing.T) {
	m := &fakeMetrics{cpu: resource.MustParse("100m")}
	s := NewInternal(m, resource.MustParse("800m"), Above)

	res, err := s.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Confirm)
}

func TestInternal_MetricsErrorPropagates(t *testing.T) {
	m := &fakeMetrics{err: fmt.Errorf("metrics-server unavailable")}
	s := NewInternal(m, resource.MustParse("50m"), Below)

	_, err := s.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics-server unavailable")
	assert.Contains(t, err.Error(), "staging/api")
}
