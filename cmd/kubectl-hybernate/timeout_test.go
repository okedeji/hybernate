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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangingCluster is a kubeconfig for an API server that never answers.
func hangingCluster(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	kubeconfig := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kubeconfig, fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters: [{name: hanging, cluster: {server: %s}}]
users: [{name: me, user: {token: t}}]
contexts: [{name: hanging, context: {cluster: hanging, user: me}}]
current-context: hanging
`, server.URL), 0o600))
	t.Setenv("KUBECONFIG", kubeconfig)
}

// A cluster that stops answering ends the scan at --timeout, saying so,
// rather than hanging it.
func TestScan_GivesUpAtTimeout(t *testing.T) {
	hangingCluster(t)
	cmd := scanCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"-n", "preview-42", "--timeout", "300ms", "--window", "0", "-o", "json"})

	start := time.Now()
	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the scan didn't finish within --timeout 300ms")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestScan_RejectsNoTimeout(t *testing.T) {
	cmd := scanCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--timeout", "0"})

	assert.EqualError(t, cmd.Execute(), "--timeout must be more than zero")
}
