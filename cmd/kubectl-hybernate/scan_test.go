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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
)

func sampleCluster() scanResult {
	workloads := []discovery.Workload{
		{Namespace: "preview-42", Kind: v1alpha1.TargetKindDeployment, Name: "checkout-api", Replicas: 2,
			State: discovery.StateIdle, CPUPercent: ptr.To(2), Reason: "CPU 2% of its request",
			MonthlyCost: 1240.4, HourlyCost: 1.6992,
			PodCPURequestMillis: 500, PodMemoryRequestBytes: 1 << 30, Clues: []string{"last deployed 23 days ago"}},
		{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Replicas: 1,
			State: discovery.StateActive, CPUPercent: ptr.To(64), MonthlyCost: 96, HourlyCost: 0.1315},
	}
	return scanResult{Context: "staging", Cluster: "staging", ClusterReport: &discovery.ClusterReport{
		Mode: discovery.ModeSnapshot, Namespaces: 1, Workloads: workloads,
		Notes: []string{"skipped namespace locked: forbidden"},
		Totals: discovery.Totals{Workloads: 2, Idle: 1, IdleCPUMillis: 1000, IdleMemoryBytes: 2 << 30,
			MonthlyCost: 1336.4, IdleMonthlyCost: 1240.4, IdleHourlyCost: 1.6992},
	}}
}

func TestWriteTable(t *testing.T) {
	cluster := sampleCluster()
	result := cluster
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table", limit: 25}))

	got := out.String()
	assert.Contains(t, got, "staging: 2 workloads in 1 namespace")
	assert.Contains(t, got, "CPU 2% of its request; last deployed 23 days ago", "why it's idle, then the facts")
	assert.Contains(t, got, "1 workload is idle right now, reserving 1.0 vCPU and 2.0 GiB of memory.")
	assert.Contains(t, got, "It costs $1,240/month while running, $1.70 an hour: each hour asleep frees that.")
	assert.Contains(t, got, "$1.70", "cost per hour in the table")
	assert.NotContains(t, got, "save up to", "a moment can't show savings")
	assert.Contains(t, got, "depends on how often they'd be woken")
	assert.Contains(t, got, "deployment/checkout-api")
	assert.Contains(t, got, "last deployed 23 days ago")
	assert.Contains(t, got, "statefulset/postgres")
	assert.Contains(t, got, "  - skipped namespace locked: forbidden")
	assert.Contains(t, got, "kubectl label deployment checkout-api -n preview-42 hybernate.io/managed=true",
		"next steps name a real idle workload")
	assert.Contains(t, got, "kubectl annotate deployment checkout-api -n preview-42 hybernate.io/dry-run=true")
	assert.NotContains(t, got, "Hybernate Hub", "it has no page to send anyone to yet")
}

func TestWriteTable_Limit(t *testing.T) {
	result := sampleCluster()
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table", limit: 1}))

	assert.NotContains(t, out.String(), "statefulset/postgres")
	assert.Contains(t, out.String(), "...and 1 more; --limit 0 lists them all")
}

func TestWriteTable_NothingIdle(t *testing.T) {
	cluster := sampleCluster()
	cluster.Totals = discovery.Totals{Workloads: 2}
	cluster.Workloads[0].State = discovery.StateActive
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster, scanOptions{output: "table"}))

	assert.Contains(t, out.String(), "No running workloads are idle right now.")
	assert.NotContains(t, out.String(), "Next steps:")
}

func TestWriteScan_JSON(t *testing.T) {
	cluster := sampleCluster()
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "json"}))

	var decoded struct {
		Context   string `json:"context"`
		Mode      string `json:"mode"`
		Workloads []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"workloads"`
		Totals struct {
			IdleMonthlyCost float64 `json:"idleMonthlyCost"`
		} `json:"totals"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Equal(t, "staging", decoded.Context)
	assert.Equal(t, "snapshot", decoded.Mode)
	assert.Equal(t, "checkout-api", decoded.Workloads[0].Name)
	assert.Equal(t, "idle", decoded.Workloads[0].State)
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
	tests := map[float64]string{0: "$0", 12.4: "$12", 999.5: "$1,000", 1240.4: "$1,240", 1234567.8: "$1,234,568",
		-123: "-$123", -1234.4: "-$1,234", -999999.6: "-$1,000,000", -0.2: "$0"}
	for in, want := range tests {
		assert.Equal(t, want, dollars(in))
	}
}

func TestShortClusterName(t *testing.T) {
	tests := map[string]string{
		"arn:aws:eks:us-east-1:123456789012:cluster/staging": "staging (EKS us-east-1)",
		"arn:aws:eks:eu-west-2:123456789012:cluster/my-dev":  "my-dev (EKS eu-west-2)",
		"gke_myproject_europe-west1_dev":                     "dev (GKE europe-west1)",
		"gke_my-project-123_us-central1-a_dev":               "dev (GKE us-central1-a)",
		"kind-hybernate-scan":                                "kind-hybernate-scan",
		"staging-admin":                                      "staging-admin",
		"arn:aws:eks:us-east-1:123:nodegroup/x":              "arn:aws:eks:us-east-1:123:nodegroup/x",
		"gke_only_two":                                       "gke_only_two",
		"gke_a_b_c_d":                                        "gke_a_b_c_d",
	}
	for in, want := range tests {
		assert.Equal(t, want, shortClusterName(in), in)
	}
}

func TestClusterName(t *testing.T) {
	assert.Equal(t, "staging (EKS us-east-1)", clusterName("arn:aws:eks:us-east-1:111111111111:cluster/staging"))
	assert.Equal(t, "current context", clusterName(""))
}

func TestWriteTable_Prices(t *testing.T) {
	cluster := sampleCluster()
	var assumed, own bytes.Buffer

	withAssumed, withOwn := cluster, cluster
	withAssumed.Prices = prices{CPUPerHour: 0.031, MemoryPerHour: 0.004, CPUAssumed: true, MemoryAssumed: true}
	withOwn.Prices = prices{CPUPerHour: 0.05, MemoryPerHour: 0.006}
	require.NoError(t, writeScan(&assumed, withAssumed, scanOptions{output: "table"}))
	require.NoError(t, writeScan(&own, withOwn, scanOptions{output: "table"}))

	assert.Contains(t, assumed.String(), "Costs use assumed list prices: $0.031 per vCPU-hour and $0.004 per GiB-hour")
	assert.NotContains(t, own.String(), "assumed", "the user's own prices need no caveat")
}

func TestPricesSentence(t *testing.T) {
	tests := []struct {
		prices prices
		want   string
	}{
		{prices{CPUPerHour: 0.031, MemoryPerHour: 0.004, CPUAssumed: true, MemoryAssumed: true},
			"Assumed list prices: $0.031 per vCPU-hour and $0.004 per GiB-hour of memory, from AWS on-demand in us-east-1."},
		{prices{CPUPerHour: 0.045, MemoryPerHour: 0.006},
			"Your prices: $0.045 per vCPU-hour and $0.006 per GiB-hour of memory."},
		{prices{CPUPerHour: 0.045, MemoryPerHour: 0.004, MemoryAssumed: true},
			"Your CPU price, $0.045 per vCPU-hour, and the assumed memory price, $0.004 per GiB-hour of memory " +
				"(AWS on-demand, us-east-1)."},
		{prices{CPUPerHour: 0.031, MemoryPerHour: 0.006, CPUAssumed: true},
			"Your memory price, $0.006 per GiB-hour of memory, and the assumed CPU price, $0.031 per vCPU-hour " +
				"(AWS on-demand, us-east-1)."},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.prices.sentence())
	}
}

func TestWriteTable_PausedAndUnjudged(t *testing.T) {
	cluster := sampleCluster()
	cluster.Workloads = append(cluster.Workloads,
		discovery.Workload{Namespace: "preview-42", Kind: v1alpha1.TargetKindDeployment, Name: "asleep",
			State: discovery.StatePaused, Reason: "paused 5h ago", Managed: true, Replicas: 3, HourlyCost: 0.3},
		discovery.Workload{Namespace: "kube-tools", Kind: v1alpha1.TargetKindDeployment, Name: "agent",
			State: discovery.StateUnknown, Unmeasured: "no CPU requests"})
	cluster.Totals.Paused, cluster.Totals.PausedHourlyCost = 1, 0.3
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster, scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "Hybernate has 1 workload paused right now, freeing $0.30 an hour.")
	assert.Contains(t, got, "paused (live)")
	assert.Contains(t, got, "paused 5h ago")
	assert.NotContains(t, got, "deployment/agent", "unjudged workloads leave the table")
	assert.Contains(t, got, "1 workload set no CPU requests, so their use can't be measured: kube-tools/agent")
}

func TestWriteTable_MeasuredInDryRun(t *testing.T) {
	cluster := sampleCluster()
	cluster.Workloads = append(cluster.Workloads, discovery.Workload{
		Namespace: "preview-7", Kind: v1alpha1.TargetKindDeployment, Name: "api", Replicas: 2,
		State: discovery.StateActive, Managed: true, DryRun: true, HourlyCost: 0.13,
		Measured: &discovery.Measured{Since: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
			Pauses: 4, Wakes: 4, SleptHours: 96.5, Freed: 12.48, MonthlyFreed: 81},
	})
	cluster.Totals.DryRun = 1
	cluster.Totals.Measured = *cluster.Workloads[2].Measured
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table"}))

	got := out.String()
	assert.Regexp(t, `preview-7\s+deployment/api\s+active \(dry-run\)\s+measuring since Oct 1\s+\$0\s+\$0.13\s+`+
		`97h\s+4\s+.*\$81`, got, "what Hybernate measured is in its columns, with the period")
	assert.Contains(t, got, "1 workload is in dry-run: measured by Hybernate since starting, it would have slept "+
		"97 hours, freeing $12.48,\n  about $81 a month.")
	assert.Contains(t, got, "COULD SAVE/MO")
	assert.NotContains(t, got, "Measured in dry-run:", "no separate section")
	assert.Contains(t, got, "start pausing:\n       kubectl hybernate enable deployment/api -n preview-7",
		"enable names the workload dry-run measured")
	assert.NotContains(t, got, "helm install", "a managed workload shows Hybernate is installed")
	assert.Contains(t, got, "kubectl label deployment checkout-api", "and an unmanaged idle one can still be measured")
}

func TestWriteTable_OnlyMeasuring(t *testing.T) {
	report := &discovery.ClusterReport{Mode: discovery.ModeSnapshot, Workloads: []discovery.Workload{{
		Namespace: "preview-7", Kind: v1alpha1.TargetKindStatefulSet, Name: "db", Replicas: 1,
		State: discovery.StateActive, Managed: true, DryRun: true,
		Measured: &discovery.Measured{Since: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Pauses: 1, SleptHours: 0.5},
	}}, Totals: discovery.Totals{Workloads: 1}}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, scanResult{Cluster: "staging", ClusterReport: report},
		scanOptions{output: "table"}))

	got := out.String()
	assert.Regexp(t, `statefulset/db\s+active \(dry-run\)\s+measuring since Oct 1\s+\$0\s+\$0.00\s+30m\s+0`, got,
		"dry-run's measurement shows even without history")
	assert.Contains(t, got, "1. When you're happy with what dry-run measured, start pausing:")
	assert.Contains(t, got, "kubectl hybernate enable statefulset/db -n preview-7")
	assert.NotContains(t, got, "kubectl label", "nothing unmanaged is idle")
}

func TestWriteTable_History(t *testing.T) {
	cluster := sampleCluster()
	cluster.Mode = discovery.ModeHistory
	cluster.History = &discovery.HistorySource{Prometheus: "monitoring/prometheus-operated", Hours: 168}
	cluster.Workloads[0].History = &discovery.History{Hours: 168, RunningHours: 168, QuietHours: 141,
		SleepHours: 120, Wakes: 9, Freed: 204.3, MonthlyFreed: 887.6}
	cluster.Totals.Replayed = discovery.ReplayTotals{Workloads: 2, Sleepers: 1, SleepHours: 1204, Wakes: 9,
		Freed: 204.3, MonthlyFreed: 887.6}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got,
		"Replaying the last 7 days, Hybernate would have paused 1 of 2 unmanaged workloads for 1,204 hours in all,")
	assert.Contains(t, got, "and freed $204: about $888/month.")
	assert.Regexp(t, `STATE\s+BECAUSE\s+COST/MO\s+COULD SLEEP\s+WAKES\s+COULD SAVE/MO`, got)
	assert.NotContains(t, got, "REPLICAS", "what the state's reason and the cost already say")
	assert.Regexp(t,
		`deployment/checkout-api\s+idle \(unmanaged\)\s+CPU 2% of its request; last deployed 23 days ago\s+`+
			`\$1,240\s+120h\s+9\s+\$888`,
		got, "the evidence comes right after the state it explains")
	assert.Regexp(t, `statefulset/postgres\s+active \(unmanaged\)\s+\$96\s+-\s+-\s+-`, got,
		"no history, no numbers")
	assert.NotContains(t, got, "COST/HOUR")
	assert.NotContains(t, got, "QUIET")
	assert.NotContains(t, got, "which one moment can't show", "history shows it")
}

func TestWriteTable_HistoryWithNothingToSleep(t *testing.T) {
	cluster := sampleCluster()
	cluster.Mode = discovery.ModeHistory
	cluster.History = &discovery.HistorySource{Hours: 30}
	cluster.Totals.Replayed = discovery.ReplayTotals{Workloads: 2}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table"}))

	assert.Contains(t, out.String(),
		"Replaying the last 30 hours, no unmanaged workload would have slept.")
}

func TestHistoryAccess(t *testing.T) {
	forbidden := &discovery.ProxyForbiddenError{Namespace: "monitoring", Service: "prometheus-operated", Port: "web"}
	tests := []struct {
		name     string
		username string
		reviewOK bool
		want     string
	}{
		{name: "a user", username: "jane@example.com", reviewOK: true, want: "--user=jane@example.com"},
		{name: "a service account", username: "system:serviceaccount:ci:scanner", reviewOK: true,
			want: "--serviceaccount=ci:scanner"},
		{name: "a cluster that can't say who you are", want: "--user=<you>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
					if !tt.reviewOK {
						return errors.New("selfsubjectreviews not served")
					}
					obj.(*authenticationv1.SelfSubjectReview).Status.UserInfo.Username = tt.username
					return nil
				},
			}).Build()

			got := historyAccess(context.Background(), c, forbidden)

			require.Len(t, got, 2)
			assert.Equal(t, "kubectl create role hybernate-scan -n monitoring --verb=get --resource=services/proxy "+
				"--resource-name=prometheus-operated:web", got[0])
			assert.Equal(t, "kubectl create rolebinding hybernate-scan -n monitoring --role=hybernate-scan "+tt.want, got[1])
		})
	}
}

// A Prometheus served over TLS is reached at the proxy's https: name, which
// is the name a Role must grant.
func TestHistoryAccess_HTTPS(t *testing.T) {
	forbidden := &discovery.ProxyForbiddenError{Namespace: "monitoring", Service: "prometheus", Port: "https",
		Scheme: "https"}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	got := historyAccess(context.Background(), c, forbidden)

	assert.Contains(t, got[0], "--resource-name=https:prometheus:https")
}

func TestWriteTable_HistoryAccess(t *testing.T) {
	cluster := sampleCluster()
	cluster.HistoryAccess = []string{"kubectl create role x", "kubectl create rolebinding x"}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table"}))

	assert.Contains(t, out.String(), "To replay history, an admin can let you read Prometheus, and nothing else, with:\n"+
		"  kubectl create role x\n  kubectl create rolebinding x\nOr pass --prometheus-url")
}

func TestWriteTable_Dependencies(t *testing.T) {
	cluster := sampleCluster()
	cluster.Workloads[0].Dependencies = []discovery.Dependency{
		{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "PGHOST",
			Address: "postgres-0.postgres-hl", Headless: true},
		{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats", Via: "NATS_URL",
			Address: "nats://nats.messaging:4222", Headless: true},
		{Namespace: "preview-42", Kind: v1alpha1.TargetKindDeployment, Name: "cache", Via: "CACHE",
			Address: "cache:6379"},
	}
	cluster.Workloads[1].Dependencies = []discovery.Dependency{
		{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "backup", Via: "BACKUP",
			Address: "backup-0.backup", Headless: true, Declared: true},
	}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "Dependencies Hybernate wakes and holds with the workloads that need them: 4 dependencies")
	assert.Regexp(t, `deployment/checkout-api\s+->\s+statefulset/postgres\s+PGHOST\s+connected once Hybernate manages it`,
		got, "an unmanaged workload's")
	assert.Regexp(t, `->\s+messaging/statefulset/nats\s+NATS_URL\s+connected once`, got,
		"another namespace's is named with it")
	assert.Regexp(t, `statefulset/backup\s+BACKUP\s+declared`, got)
	assert.NotContains(t, got, "hybernate.io/depends-on=", "Hybernate connects dependencies itself")
}

// Many dependencies are summed up, listed most useful first, and cut at
// --limit like the workloads table.
func TestWriteTable_ManyDependencies(t *testing.T) {
	cluster := sampleCluster()
	since := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i := range 6 {
		wl := discovery.Workload{Namespace: "preview-42", Kind: v1alpha1.TargetKindDeployment,
			Name: fmt.Sprintf("svc-%d", i), State: discovery.StateActive, Managed: true, DryRun: true,
			Measured: &discovery.Measured{Since: since, Pauses: 1, SleptHours: 10, Freed: float64(i)}}
		wl.Dependencies = []discovery.Dependency{{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet,
			Name: "db", Via: "DB", Address: "db:5432", Headless: i == 4}}
		cluster.Workloads = append(cluster.Workloads, wl)
	}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster,
		scanOptions{output: "table", limit: 2}))

	got := out.String()
	assert.Contains(t, got, "  ...and 4 more; --limit 0 lists them all")
	assert.Contains(t, got, "6 dependencies")
}

func TestWriteTable_ScaledToZeroByHand(t *testing.T) {
	cluster := sampleCluster()
	cluster.Workloads = append(cluster.Workloads, discovery.Workload{Namespace: "preview-1",
		Kind: v1alpha1.TargetKindDeployment, Name: "demo", State: discovery.StatePaused, ScaledByHand: true,
		Reason: "scaled to zero, not by Hybernate"})
	cluster.Totals.ScaledToZero = 1
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, cluster, scanOptions{output: "table"}))

	got := out.String()
	assert.Regexp(t, `deployment/demo\s+paused \(unmanaged\)\s+scaled to zero, not by Hybernate`, got)
	assert.Contains(t, got, "1 workload is scaled to zero by hand; Hybernate can pause it while idle and wake it")
	assert.NotContains(t, got, "Hybernate has 1 workload paused")
}

func TestManagement(t *testing.T) {
	assert.Equal(t, "unmanaged", management(discovery.Workload{}))
	assert.Equal(t, "dry-run", management(discovery.Workload{Managed: true, DryRun: true}))
	assert.Equal(t, "live", management(discovery.Workload{Managed: true}))
}

func TestParseWindow(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "36h", want: 36 * time.Hour},
		{in: "0", want: 0},
		{in: "1w", wantErr: true},
		{in: "-2d", wantErr: true},
		{in: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseWindow(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPricesFor(t *testing.T) {
	tests := []struct {
		name              string
		args              []string
		wantCPU           float64
		wantCPUAssumed    bool
		wantMemoryAssumed bool
	}{
		{name: "no prices given", wantCPU: 0.031, wantCPUAssumed: true, wantMemoryAssumed: true},
		{name: "a CPU price", args: []string{"--cpu-price", "0.05"}, wantCPU: 0.05, wantMemoryAssumed: true},
		{name: "a memory price", args: []string{"--memory-price", "0.006"}, wantCPU: 0.031, wantCPUAssumed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := scanCmd(&kubeFlags{})
			require.NoError(t, cmd.ParseFlags(tt.args))
			cpu, err := cmd.Flags().GetFloat64("cpu-price")
			require.NoError(t, err)
			memory, err := cmd.Flags().GetFloat64("memory-price")
			require.NoError(t, err)

			ownCPU, ownMemory := ownPrices(cmd)

			got := pricesFor(scanOptions{cpuPrice: cpu, memoryPrice: memory, ownCPUPrice: ownCPU, ownMemoryPrice: ownMemory})

			assert.InDelta(t, tt.wantCPU, got.CPUPerHour, 1e-9)
			assert.Equal(t, tt.wantCPUAssumed, got.CPUAssumed)
			assert.Equal(t, tt.wantMemoryAssumed, got.MemoryAssumed)
		})
	}
}

func TestDependencyStatus(t *testing.T) {
	tests := []struct {
		name    string
		managed bool
		d       discovery.Dependency
		want    string
	}{
		{name: "declared wins", managed: true, d: discovery.Dependency{Declared: true, Connected: true}, want: "declared"},
		{name: "connected by Hybernate", managed: true, d: discovery.Dependency{Connected: true},
			want: "connected by Hybernate"},
		{name: "unmanaged", want: "connected once Hybernate manages it"},
		{name: "managed, not yet applied", managed: true, want: "not connected yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := dependencyLine{wl: Workload{Managed: tt.managed}, d: tt.d}
			assert.Equal(t, tt.want, dependencyStatus(l))
		})
	}
}

func TestWriteScan_SavedThisMonth(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, historyResult(), scanOptions{output: "table"}))

	got := out.String()
	assert.Contains(t, got, "Hybernate has saved $4 this month pausing 1 live workload.")
	assert.Regexp(t, `preview-3\s+deployment/web\s+paused \(live\)\s+paused 3h ago\s+\$48\s+\$4\s+-\s+-\s+-`, got,
		"a live workload's saving is in SAVED THIS MONTH, with nothing estimated")
}

func TestWriteScan_Protected(t *testing.T) {
	result := historyResult()
	for i := range result.Workloads {
		if result.Workloads[i].Name == "postgres" {
			result.Workloads[i].Protected = true
		}
	}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table"}))

	got := out.String()
	assert.Regexp(t, `statefulset/postgres\s+idle \(protected\)`, got)
	assert.NotContains(t, got, "kubectl label statefulset postgres", "never suggested")
}

// Next steps never suggest a protected workload, in the table or the
// report, which choose them the same way.
func TestNextSteps_SkipProtected(t *testing.T) {
	protected := Workload{Namespace: "prod", Kind: v1alpha1.TargetKindDeployment, Name: "payments",
		State: discovery.StateIdle, Protected: true}
	open := Workload{Namespace: "dev", Kind: v1alpha1.TargetKindDeployment, Name: "preview", State: discovery.StateIdle}
	measuringProtected := Workload{Namespace: "prod", Kind: v1alpha1.TargetKindDeployment, Name: "ledger",
		Managed: true, DryRun: true, Protected: true, Measured: &discovery.Measured{}}
	tests := []struct {
		name      string
		workloads []Workload
		want      []string
	}{
		{name: "only protected", workloads: []Workload{protected, measuringProtected}},
		{name: "the first that isn't", workloads: []Workload{protected, open}, want: []string{
			"kubectl label deployment preview -n dev hybernate.io/managed=true",
			"kubectl annotate deployment preview -n dev hybernate.io/dry-run=true",
			"kubectl hybernate enable deployment/preview -n dev",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := scanResult{Cluster: "c", ClusterReport: &discovery.ClusterReport{Workloads: tt.workloads,
				Totals: discovery.Totals{Workloads: len(tt.workloads), Idle: 2}}}
			var table bytes.Buffer
			writeNextSteps(&printer{w: &table}, result)
			page := renderHTML(t, result)

			if tt.want == nil {
				assert.Empty(t, table.String())
				assert.NotContains(t, page, "Next steps")
				return
			}
			for _, command := range tt.want {
				assert.Contains(t, table.String(), command)
			}
			steps := buildReport(result, time.Hour).NextSteps
			require.Len(t, steps, len(tt.want))
			for _, step := range steps {
				assert.NotContains(t, string(step), "payments")
			}
			assert.NotContains(t, table.String(), "payments")
		})
	}
}

func TestParseHeaders(t *testing.T) {
	got, err := parseHeaders([]string{"X-Scope-OrgID: team-a", "x-extra:  two words  "})
	require.NoError(t, err)
	assert.Equal(t, "team-a", got.Get("X-Scope-OrgID"))
	assert.Equal(t, "two words", got.Get("X-Extra"))

	for _, bad := range []string{"no colon", ": no name", "bad name: x", "X-A: one\rtwo"} {
		_, err := parseHeaders([]string{bad})
		assert.Error(t, err, bad)
	}
}

func TestPrometheusFrom(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600))

	setup, err := prometheusFrom(prometheusOptions{url: "https://mimir.example.com", selector: `cluster="prod"`,
		headers: []string{"X-Scope-OrgID: team-a"}, bearerTokenFile: tokenFile})
	require.NoError(t, err)
	assert.Equal(t, discovery.Selector{`cluster="prod"`}, setup.selector)
	assert.Equal(t, "Bearer s3cret", setup.header.Get("Authorization"))
	assert.Equal(t, "team-a", setup.header.Get("X-Scope-OrgID"))
	require.NotNil(t, setup.client)
	assert.NotZero(t, setup.client.Timeout)

	tests := []struct {
		name string
		opts prometheusOptions
		want string
	}{
		{"a bad selector", prometheusOptions{selector: `cluster="prod"} or vector(1)`}, "--prometheus-selector"},
		{"auth without a URL", prometheusOptions{headers: []string{"X-A: b"}}, "are for --prometheus-url"},
		{"two Authorizations", prometheusOptions{url: "https://p", headers: []string{"Authorization: Basic x"},
			bearerTokenFile: tokenFile}, "can't both be given"},
		{"a missing token file", prometheusOptions{url: "https://p", bearerTokenFile: tokenFile + "-missing"},
			"--prometheus-bearer-token-file"},
		{"a missing CA file", prometheusOptions{url: "https://p", caFile: tokenFile + "-missing"}, "--prometheus-ca-file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := prometheusFrom(tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A scan that couldn't read all it should have exits non-zero, after its
// report is written.
func TestIncompleteError(t *testing.T) {
	result := sampleCluster()
	assert.NoError(t, incompleteError(result))

	result.Incomplete = []string{"payments", "orders"}
	err := incompleteError(result)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the scan of staging is incomplete: 2 namespaces couldn't be read in full")
}

// An empty cluster says so, rather than pointing at notes it doesn't have.
func TestEmptyCluster(t *testing.T) {
	result := scanResult{Cluster: "c", ClusterReport: &discovery.ClusterReport{Namespaces: 3, Workloads: []Workload{}}}
	var out bytes.Buffer

	require.NoError(t, writeScan(&out, result, scanOptions{output: "table"}))

	assert.Contains(t, out.String(), "c: 0 workloads in 3 namespaces")
	assert.Contains(t, out.String(), "No Deployments or StatefulSets were found.")
	assert.NotContains(t, out.String(), "No running workloads are idle")
	page := renderHTML(t, result)
	assert.Contains(t, page, "No Deployments or StatefulSets were found.")
	assert.NotContains(t, page, "the notes say why")

	result.Notes = []string{"your access doesn't allow reading workloads in 3 namespaces"}
	assert.Contains(t, renderHTML(t, result), "the notes say what it couldn&#39;t")
}
