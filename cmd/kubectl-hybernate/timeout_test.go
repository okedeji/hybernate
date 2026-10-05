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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// hangingCluster is a kubeconfig for an API server that never answers.
func hangingCluster(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	writeKubeconfig(t, server.URL, "")
}

// writeKubeconfig points KUBECONFIG at a cluster with one context, named
// "hanging", whose namespace is namespace.
func writeKubeconfig(t *testing.T, server, namespace string) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kubeconfig, fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters: [{name: hanging, cluster: {server: %s}}]
users: [{name: me, user: {token: t}}]
contexts: [{name: hanging, context: {cluster: hanging, user: me, namespace: %q}}]
current-context: hanging
`, server, namespace), 0o600))
	t.Setenv("KUBECONFIG", kubeconfig)
}

// runRoot runs the plugin against the cluster the kubeconfig names.
func runRoot(args ...string) error {
	root := newRootCmd(newKubeClient)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	return root.Execute()
}

// A cluster that stops answering ends the scan at --timeout, saying so,
// rather than hanging it.
func TestScan_GivesUpAtTimeout(t *testing.T) {
	hangingCluster(t)

	start := time.Now()
	err := runRoot("scan", "-n", "preview-42", "--timeout", "300ms", "--window", "0", "-o", "json")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the scan didn't finish within --timeout 300ms")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// A Prometheus that stops answering is named as what was slow, not the
// API server, which answered.
func TestScan_TimeoutBlamesPrometheus(t *testing.T) {
	prometheus := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(prometheus.Close)

	_, err := runCLI(t, newStatusClient(t, interceptor.Funcs{}), "scan", "-n", "preview-42", "-o", "json",
		"--timeout", "300ms", "--prometheus-url", prometheus.URL)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the scan didn't finish within --timeout 300ms: Prometheus answered too slowly")
	assert.NotContains(t, err.Error(), "API server")
}

// A deadline of the scan's own, such as one Prometheus query's, isn't
// --timeout running out, so it isn't reported as that.
func TestScanFailed(t *testing.T) {
	queryTimedOut := &historyError{fmt.Errorf("reading history from --prometheus-url: %w", context.DeadlineExceeded)}

	err := scanFailed(context.Background(), queryTimedOut, time.Minute)

	assert.Equal(t, queryTimedOut, err)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	assert.ErrorContains(t, scanFailed(expired, fmt.Errorf("listing pods: %w", context.DeadlineExceeded), time.Minute),
		"the cluster's API server answered too slowly")
	assert.ErrorContains(t, scanFailed(expired, queryTimedOut, time.Minute), "Prometheus answered too slowly")
}

// Every command that talks to the cluster gives up at --timeout, saying so.
func TestCommands_GiveUpAtTimeout(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "status", args: []string{"status"}},
		{name: "status in a namespace", args: []string{"status", "-n", "preview-42"}},
		{name: "wake", args: []string{"wake", "api", "-n", "preview-42"}},
		{name: "enable", args: []string{"enable", "api", "-n", "preview-42"}},
		{name: "enable --all", args: []string{"enable", "--all", "-n", "preview-42"}},
		{name: "deps", args: []string{"deps", "api", "-n", "preview-42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hangingCluster(t)

			start := time.Now()
			err := runRoot(append(tt.args, "--timeout", "300ms")...)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "didn't answer within --timeout 300ms")
			assert.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

// A threshold of 0 would be judged as the default 10% while the report
// said 0%, and one over 100% can never be reached; both are refused.
func TestScan_RejectsACPUThresholdOutsideAPercentage(t *testing.T) {
	for _, threshold := range []string{"0", "-5", "101"} {
		t.Run(threshold, func(t *testing.T) {
			err := runRoot("scan", "--cpu-threshold", threshold)

			assert.EqualError(t, err, "--cpu-threshold must be a percentage from 1 to 100")
		})
	}
}

func TestCommands_RejectNonPositiveDurations(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"scan", "--timeout", "0"}, want: "--timeout must be more than zero"},
		{args: []string{"status", "--timeout", "-1s"}, want: "--timeout must be more than zero"},
		{args: []string{"status", "--since", "0"}, want: "--since must be more than zero"},
		{args: []string{"status", "--since", "-1h"}, want: "--since must be more than zero"},
		{args: []string{"wake", "api", "--timeout", "0"}, want: "--timeout must be more than zero"},
		{args: []string{"wake", "api", "--for", "-1h"}, want: "--for can't be negative"},
		{args: []string{"enable", "api", "--timeout", "0"}, want: "--timeout must be more than zero"},
		{args: []string{"deps", "api", "--timeout", "0"}, want: "--timeout must be more than zero"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.args), func(t *testing.T) {
			err := runRoot(tt.args...)

			assert.EqualError(t, err, tt.want)
		})
	}
}
