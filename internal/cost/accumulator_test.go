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

package cost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHoursPrice(t *testing.T) {
	h := Hours{CPU: 100, Memory: 400, Storage: 200}

	assert.InDelta(t, 100*0.031+400*0.004+200*(0.08/730), h.Price(DefaultRates), 1e-12)
}

func TestComputeHourly(t *testing.T) {
	assert.InDelta(t, 2*0.031+8*0.004, ComputeHourly(2, 8, DefaultRates), 1e-12)
}

func TestCounted(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "a flush interval counts in full", in: 5 * time.Minute, want: 5 * time.Minute},
		{name: "up to the limit", in: MaxInterval, want: MaxInterval},
		{name: "downtime counts as the limit", in: 10 * time.Hour, want: MaxInterval},
		{name: "the clock going back counts as nothing", in: -5 * time.Minute, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Counted(tt.in))
		})
	}
}

func TestMonthOf(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)

	assert.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), MonthOf(time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)))
	assert.Equal(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), MonthOf(time.Date(2026, 3, 1, 8, 0, 0, 0, tokyo)),
		"months are UTC months wherever the operator runs")
}

// A workload created late in the month is projected from the time it was
// tracked, not from the day of the month.
func TestProject(t *testing.T) {
	march20 := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	const hourly = 0.05

	tests := []struct {
		name    string
		tracked time.Duration
		want    float64
		ok      bool
	}{
		{name: "less than a day is pending", tracked: 23 * time.Hour},
		{name: "a day tracked from the 19th", tracked: 24 * time.Hour, want: hourly * 31 * 24, ok: true},
		{name: "the month so far", tracked: (19*24 + 12) * time.Hour, want: hourly * 31 * 24, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Project(hourly*tt.tracked.Hours(), tt.tracked, march20)

			assert.Equal(t, tt.ok, ok)
			assert.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestFormatDollars(t *testing.T) {
	assert.Equal(t, "$0.00", FormatDollars(0))
	assert.Equal(t, "$1.50", FormatDollars(1.5))
	assert.Equal(t, "$123.46", FormatDollars(123.456))
}
