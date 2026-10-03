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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
)

func sampleCluster(context string) clusterScan {
	workloads := []discovery.Workload{
		{Namespace: "sandbox-42", Kind: v1alpha1.TargetKindDeployment, Name: "checkout-api", Replicas: 2,
			State: discovery.StateIdle, CPUPercent: ptr.To(2), Reason: "CPU 2%", MonthlyCost: 1240.4, HourlyCost: 1.6992,
			PodCPURequestMillis: 500, PodMemoryRequestBytes: 1 << 30, Clues: []string{"not deployed in 23 days"}},
		{Namespace: "sandbox-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Replicas: 1,
			State: discovery.StateActive, CPUPercent: ptr.To(64), MonthlyCost: 96, HourlyCost: 0.1315},
	}
	return clusterScan{Context: context, Cluster: context, ClusterReport: &discovery.ClusterReport{
		Mode: discovery.ModeSnapshot, Workloads: workloads, Notes: []string{"skipped namespace locked: forbidden"},
		Totals: discovery.Totals{Workloads: 2, Idle: 1, IdleCPUMillis: 1000, IdleMemoryBytes: 2 << 30,
			MonthlyCost: 1336.4, IdleMonthlyCost: 1240.4, IdleHourlyCost: 1.6992},
	}}
}

func TestWriteTable(t *testing.T) {
	cluster := sampleCluster("staging")
	result := scanResult{Clusters: []clusterScan{cluster}, Totals: cluster.Totals}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table", limit: 25}))

	got := out.String()
	assert.Contains(t, got, "staging: 2 workloads in 1 namespace")
	assert.Contains(t, got, "CPU 2%; not deployed in 23 days", "why it's idle, then the facts")
	assert.Contains(t, got, "1 workload is idle right now, reserving 1.0 vCPU and 2.0 GiB of memory.")
	assert.Contains(t, got, "It costs $1,240/month while running, $1.70 an hour: each hour asleep frees that.")
	assert.Contains(t, got, "$1.70", "cost per hour in the table")
	assert.NotContains(t, got, "save up to", "a moment can't show savings")
	assert.Contains(t, got, "depends on how often they'd be woken")
	assert.Contains(t, got, "deployment/checkout-api")
	assert.Contains(t, got, "not deployed in 23 days")
	assert.Contains(t, got, "statefulset/postgres")
	assert.Contains(t, got, "  - skipped namespace locked: forbidden")
	assert.Contains(t, got, "kubectl label deployment checkout-api -n sandbox-42 hybernate.io/managed=true",
		"next steps name a real idle workload")
	assert.Contains(t, got, "kubectl annotate deployment checkout-api -n sandbox-42 hybernate.io/dry-run=true")
	assert.NotContains(t, got, "All clusters:", "one cluster needs no combined total")
	assert.Contains(t, got, "Hybernate Hub verifies savings against your cloud bill")
}

func TestWriteTable_Limit(t *testing.T) {
	result := scanResult{Clusters: []clusterScan{sampleCluster("staging")}}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table", limit: 1}))

	assert.NotContains(t, out.String(), "statefulset/postgres")
	assert.Contains(t, out.String(), "...and 1 more; --limit 0 lists them all")
}

func TestWriteTable_SeveralClusters(t *testing.T) {
	a, b := sampleCluster("staging"), sampleCluster("sandboxes")
	failed := clusterScan{Context: "prod", Cluster: "prod",
		Error: "you can't list namespaces in this cluster; name the ones to scan with -n"}
	result := scanResult{Clusters: []clusterScan{a, b, failed}}
	result.Totals = combinedTotals(result.Clusters)
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table", limit: 25}))

	assert.Contains(t, out.String(), "prod: not scanned: you can't list namespaces")
	assert.Contains(t, out.String(), "All clusters:")
	assert.Contains(t, out.String(), "2 workloads are idle right now, reserving 2.0 vCPU and 4.0 GiB of memory.")
	assert.Contains(t, out.String(), "They cost $2,481/month")
	assert.Equal(t, 1, strings.Count(out.String(), "Next steps:"))
}

func TestWriteTable_NothingIdle(t *testing.T) {
	cluster := sampleCluster("staging")
	cluster.Totals = discovery.Totals{Workloads: 2}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Clusters: []clusterScan{cluster}}, scanOptions{output: "table"}))

	assert.Contains(t, out.String(), "No running workloads are idle right now.")
	assert.NotContains(t, out.String(), "Next steps:")
}

func TestWriteScan_JSON(t *testing.T) {
	cluster := sampleCluster("staging")
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Clusters: []clusterScan{cluster}, Totals: cluster.Totals},
		scanOptions{output: "json"}))

	var decoded struct {
		Clusters []struct {
			Context   string `json:"context"`
			Mode      string `json:"mode"`
			Workloads []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"workloads"`
		} `json:"clusters"`
		Totals struct {
			IdleMonthlyCost float64 `json:"idleMonthlyCost"`
		} `json:"totals"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Equal(t, "staging", decoded.Clusters[0].Context)
	assert.Equal(t, "snapshot", decoded.Clusters[0].Mode)
	assert.Equal(t, "checkout-api", decoded.Clusters[0].Workloads[0].Name)
	assert.Equal(t, "idle", decoded.Clusters[0].Workloads[0].State)
	assert.InDelta(t, 1240.4, decoded.Totals.IdleMonthlyCost, 0.001)
	assert.NotContains(t, out.String(), "potentialSavings")
	assert.NotContains(t, out.String(), "Hybernate Hub", "machine-readable output carries no promotion")
}

func TestCents(t *testing.T) {
	tests := map[float64]string{0: "$0.00", 0.0036: "<$0.01", 0.005: "$0.01", 1.6992: "$1.70"}
	for in, want := range tests {
		assert.Equal(t, want, cents(in))
	}
}

func TestDollars(t *testing.T) {
	tests := map[float64]string{0: "$0", 12.4: "$12", 999.5: "$1,000", 1240.4: "$1,240", 1234567.8: "$1,234,568"}
	for in, want := range tests {
		assert.Equal(t, want, dollars(in))
	}
}

func TestShortClusterName(t *testing.T) {
	tests := map[string]string{
		"arn:aws:eks:us-east-1:123456789012:cluster/staging":    "staging (EKS us-east-1)",
		"arn:aws:eks:eu-west-2:123456789012:cluster/my-sandbox": "my-sandbox (EKS eu-west-2)",
		"gke_myproject_europe-west1_sandboxes":                  "sandboxes (GKE europe-west1)",
		"gke_my-project-123_us-central1-a_dev":                  "dev (GKE us-central1-a)",
		"kind-hybernate-scan":                                   "kind-hybernate-scan",
		"staging-admin":                                         "staging-admin",
		"arn:aws:eks:us-east-1:123:nodegroup/x":                 "arn:aws:eks:us-east-1:123:nodegroup/x",
		"gke_only_two":                                          "gke_only_two",
		"gke_a_b_c_d":                                           "gke_a_b_c_d",
	}
	for in, want := range tests {
		assert.Equal(t, want, shortClusterName(in), in)
	}
}

func TestNameClusters_KeepsClashingNamesInFull(t *testing.T) {
	clusters := []clusterScan{
		{Context: "arn:aws:eks:us-east-1:111111111111:cluster/staging"},
		{Context: "arn:aws:eks:us-east-1:222222222222:cluster/staging"},
		{Context: "gke_proj_europe-west1_sandboxes"},
	}

	nameClusters(clusters)

	assert.Equal(t, "arn:aws:eks:us-east-1:111111111111:cluster/staging", clusters[0].Cluster,
		"two accounts' staging clusters mustn't look like one")
	assert.Equal(t, "arn:aws:eks:us-east-1:222222222222:cluster/staging", clusters[1].Cluster)
	assert.Equal(t, "sandboxes (GKE europe-west1)", clusters[2].Cluster)
}

func TestWriteTable_Prices(t *testing.T) {
	cluster := sampleCluster("staging")
	var assumed, own bytes.Buffer

	require.NoError(t, writeScan(&assumed, scanResult{Clusters: []clusterScan{cluster},
		Prices: prices{CPUPerHour: 0.031, MemoryPerHour: 0.004, Assumed: true}}, scanOptions{output: "table"}))
	require.NoError(t, writeScan(&own, scanResult{Clusters: []clusterScan{cluster},
		Prices: prices{CPUPerHour: 0.05, MemoryPerHour: 0.006}}, scanOptions{output: "table"}))

	assert.Contains(t, assumed.String(), "Costs use assumed list prices: $0.031 per vCPU-hour and $0.004 per GiB-hour")
	assert.NotContains(t, own.String(), "assumed list prices", "the user's own prices need no caveat")
}

func TestWriteTable_PausedAndUnjudged(t *testing.T) {
	cluster := sampleCluster("staging")
	cluster.Workloads = append(cluster.Workloads,
		discovery.Workload{Namespace: "sandbox-42", Kind: v1alpha1.TargetKindDeployment, Name: "asleep",
			State: discovery.StatePaused, Reason: "paused 5h ago", Managed: true, Replicas: 3, HourlyCost: 0.3},
		discovery.Workload{Namespace: "kube-tools", Kind: v1alpha1.TargetKindDeployment, Name: "agent",
			State: discovery.StateUnknown, Unmeasured: "no CPU requests"})
	cluster.Totals.Paused, cluster.Totals.PausedHourlyCost = 1, 0.3
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Clusters: []clusterScan{cluster}}, scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "Hybernate has 1 workload paused right now, freeing $0.30 an hour.")
	assert.Contains(t, got, "paused (managed)")
	assert.Contains(t, got, "paused 5h ago")
	assert.NotContains(t, got, "deployment/agent", "unjudged workloads leave the table")
	assert.Contains(t, got, "1 workload set no CPU requests, so their use can't be measured: kube-tools/agent")
}

func TestWriteTable_MeasuredInDryRun(t *testing.T) {
	cluster := sampleCluster("staging")
	cluster.Workloads = append(cluster.Workloads, discovery.Workload{
		Namespace: "sandbox-7", Kind: v1alpha1.TargetKindDeployment, Name: "api", Replicas: 2,
		State: discovery.StateActive, Managed: true, DryRun: true, HourlyCost: 0.13,
		Measured: &discovery.Measured{Since: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
			Pauses: 4, SleptHours: 96.5, Freed: 12.48},
	})
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Clusters: []clusterScan{cluster}, Totals: cluster.Totals},
		scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "Measured in dry-run")
	assert.Regexp(t,
		`sandbox-7\s+deployment/api\s+since Oct 1: would have paused 4 times, slept 96h, freeing \$12.48`, got)
	assert.Contains(t, got, "start pausing:\n       kubectl hybernate enable deployment/api -n sandbox-7",
		"enable names the workload dry-run measured")
	assert.NotContains(t, got, "helm install", "a managed workload shows Hybernate is installed")
	assert.Contains(t, got, "kubectl label deployment checkout-api", "and an unmanaged idle one can still be measured")
}

func TestWriteTable_OnlyMeasuring(t *testing.T) {
	report := &discovery.ClusterReport{Mode: discovery.ModeSnapshot, Workloads: []discovery.Workload{{
		Namespace: "sandbox-7", Kind: v1alpha1.TargetKindStatefulSet, Name: "db", Replicas: 1,
		State: discovery.StateActive, Managed: true, DryRun: true,
		Measured: &discovery.Measured{Since: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Pauses: 1, SleptHours: 0.5},
	}}, Totals: discovery.Totals{Workloads: 1}}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Clusters: []clusterScan{{Cluster: "staging", ClusterReport: report}}},
		scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "would have paused 1 time, slept 30m, freeing $0.00")
	assert.Contains(t, got, "1. When you're happy with what dry-run measured, start pausing:")
	assert.Contains(t, got, "kubectl hybernate enable statefulset/db -n sandbox-7")
	assert.NotContains(t, got, "kubectl label", "nothing unmanaged is idle")
}

func TestPricesFor(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCPU     float64
		wantAssumed bool
	}{
		{name: "no prices given", wantCPU: 0.031, wantAssumed: true},
		{name: "a CPU price", args: []string{"--cpu-price", "0.05"}, wantCPU: 0.05},
		{name: "a memory price", args: []string{"--memory-price", "0.006"}, wantCPU: 0.031},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := scanCmd()
			require.NoError(t, cmd.ParseFlags(tt.args))
			cpu, err := cmd.Flags().GetFloat64("cpu-price")
			require.NoError(t, err)
			memory, err := cmd.Flags().GetFloat64("memory-price")
			require.NoError(t, err)

			got := pricesFor(cmd, scanOptions{cpuPrice: cpu, memoryPrice: memory})

			assert.InDelta(t, tt.wantCPU, got.CPUPerHour, 1e-9)
			assert.Equal(t, tt.wantAssumed, got.Assumed)
		})
	}
}
