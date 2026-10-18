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

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCounts(t *testing.T) {
	healthy := map[string]int{"aws": 1400, "azure": 1300, "gcp": 550}
	tests := []struct {
		name     string
		previous map[string]int
		current  map[string]int
		wantErr  string
	}{
		{name: "as before", previous: healthy, current: healthy},
		{name: "first table", current: healthy},
		{
			name:     "a few gone",
			previous: healthy,
			current:  map[string]int{"aws": 1300, "azure": 1300, "gcp": 500},
		},
		{
			name:     "emptied feed",
			previous: healthy,
			current:  map[string]int{"aws": 1400, "gcp": 550},
			wantErr:  "azure has 0 instance types",
		},
		{
			name:    "too few with nothing to compare",
			current: map[string]int{"aws": 1400, "azure": 1300, "gcp": 99},
			wantErr: "gcp has 99 instance types, fewer than 100",
		},
		{
			name:     "over a fifth gone",
			previous: healthy,
			current:  map[string]int{"aws": 1100, "azure": 1300, "gcp": 550},
			wantErr:  "aws has 1100 instance types, down from 1400",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCounts(tt.previous, tt.current)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestCountRows(t *testing.T) {
	table := "# provider,instance type,vCPUs,memory GiB,region=on-demand USD per hour...\n" +
		"aws,m5.large,2,8,us-east-1=0.096\n" +
		"aws,m5.xlarge,4,16,us-east-1=0.192\n" +
		"gcp,e2-small,2,2,us-central1=0.0167\n"
	counts, err := countRows(strings.NewReader(table))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"aws": 2, "gcp": 1}, counts)
}

func TestCountsInCommittedTable(t *testing.T) {
	counts, err := countsIn(filepath.Join("..", "..", "internal", "cost", "prices.csv.gz"))
	require.NoError(t, err)
	assert.NoError(t, checkCounts(nil, counts), "the committed table fails its own check")
}

func TestCountsInMissingTable(t *testing.T) {
	counts, err := countsIn(filepath.Join(t.TempDir(), "prices.csv.gz"))
	require.NoError(t, err)
	assert.Empty(t, counts)
}
