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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
)

// historyResult is a cluster replayed over a week of history, with dry-run
// and dependencies to show.
func historyResult() scanResult {
	return scanResult{
		ScannedAt: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC),
		Context:   "arn:aws:eks:us-east-1:1:cluster/staging",
		Cluster:   "staging (EKS us-east-1)",
		Settings:  settings{CPUThreshold: 10, IdleAfter: "1h0m0s", Window: "7d"},
		Prices:    prices{CPUPerHour: 0.031, MemoryPerHour: 0.004, CPUAssumed: true, MemoryAssumed: true},
		ClusterReport: &discovery.ClusterReport{
			Mode:    discovery.ModeHistory,
			History: &discovery.HistorySource{Prometheus: "monitoring/prometheus-operated", Hours: 168},
			Workloads: []discovery.Workload{
				{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Replicas: 1,
					State: discovery.StateIdle, CPUPercent: ptr.To(0), Reason: "CPU under 1% of its request",
					Clues:       []string{"last deployed 23 days ago"},
					MonthlyCost: 140, HourlyCost: 0.19,
					History: &discovery.History{Hours: 168, RunningHours: 168, QuietHours: 168, SleepHours: 167,
						Freed: 32, MonthlyFreed: 139}},
				{Namespace: "preview-7", Kind: v1alpha1.TargetKindDeployment, Name: "api", Replicas: 2,
					State: discovery.StateIdle, CPUPercent: ptr.To(1), Reason: "no activity for 5h", Managed: true, DryRun: true,
					MonthlyCost: 96, HourlyCost: 0.13,
					History: &discovery.History{Hours: 168, RunningHours: 168, QuietHours: 131, SleepHours: 112,
						Wakes: 9, Freed: 14.6, MonthlyFreed: 64},
					Measured: &discovery.Measured{Since: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
						Pauses: 4, Wakes: 4, SleptHours: 41, Freed: 5.33, MonthlyFreed: 40},
					Dependencies: []discovery.Dependency{
						{Namespace: "preview-7", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "PGHOST",
							Address: "postgres-0.postgres-hl", Headless: true},
						{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats", Via: "NATS_URL",
							Address: "nats://nats.messaging:4222"},
					}},
				{Namespace: "preview-3", Kind: v1alpha1.TargetKindDeployment, Name: "web", Replicas: 2,
					State: discovery.StatePaused, Reason: "paused 3h ago", Managed: true, MonthlyCost: 48, HourlyCost: 0.07,
					SavedThisMonth: 3.5},
				{Namespace: "payments", Kind: v1alpha1.TargetKindDeployment, Name: "ledger", Replicas: 3,
					State: discovery.StateActive, CPUPercent: ptr.To(64), Reason: "CPU 64% of its request",
					MonthlyCost: 210, HourlyCost: 0.29,
					History: &discovery.History{Hours: 168, RunningHours: 168, QuietHours: 12}},
				{Namespace: "kube-tools", Kind: v1alpha1.TargetKindDeployment, Name: "agent", State: discovery.StateUnknown,
					Unmeasured: "no CPU requests"},
			},
			Notes: []string{"the history replay sees CPU and rollouts only"},
			Totals: discovery.Totals{Workloads: 5, MonthlyCost: 446, SavingsBasis: 446, Paused: 1, PausedHourlyCost: 0.07,
				Idle: 2, IdleCPUMillis: 2500,
				IdleMemoryBytes: 6 << 30, IdleHourlyCost: 0.32, IdleMonthlyCost: 236,
				Replayed: discovery.ReplayTotals{Workloads: 2, Sleepers: 1, SleepHours: 167, Freed: 32, MonthlyFreed: 139},
				Measured: discovery.Measured{Pauses: 4, SleptHours: 41, Freed: 5.33, MonthlyFreed: 40}, DryRun: 1,
				SavedThisMonth: 3.5, Live: 1},
		},
	}
}

func renderHTML(t *testing.T, result scanResult) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, writeHTML(&out, result, time.Hour))
	return out.String()
}

func TestWriteHTML_History(t *testing.T) {
	got := renderHTML(t, historyResult())

	assert.Contains(t, got, "<title>Workload scan: staging (EKS us-east-1)</title>")
	assert.Contains(t, got, "3 October 2026, 09:00 UTC")
	assert.Contains(t, got, "Based on the last 7 days of Prometheus history, and CPU at the time of the scan.")
	assert.Contains(t, got, "Assumed list prices: $0.031 per vCPU-hour and $0.004 per GiB-hour of memory")
	assert.Contains(t, got, `<div class="figure"><b>$4</b><span>saved by Hybernate this month</span>`+
		`<small>so far, pausing 1 live workload</small></div>`)
	assert.Contains(t, got, `<div class="figure"><b>$179</b><span>could be saved a month</span>`+
		`<small>40% of what these workloads cost over the same time</small></div>`,
		"what history shows for unmanaged workloads, plus what dry-run measured, against what they cost")
	assert.NotContains(t, got, "Had Hybernate been pausing them", "the figures and facts say it")
	assert.Contains(t, got, "It&#39;s idle once it has had no activity for 1h")
	assert.Contains(t, got, "Try Hybernate Hub")
	tip := "Hours Hybernate would have had it paused, measured since dry-run started for a dry-run workload, or " +
		"from history over the last 7 days for an unmanaged one. Under it, how many times activity would have " +
		"woken it, each a wait for someone."
	assert.Contains(t, got, `class="tip" title="`+tip+`">Could sleep</button>`, "the definition is on the header")
	assert.Contains(t, got, "<dt>Could sleep</dt><dd>"+tip+"</dd>", "and kept in the method section for print")
	assert.NotContains(t, got, "Quiet")
	assert.Contains(t, got, "<div>$446<span>a month, what these workloads cost while running</span></div>")
	assert.Contains(t, got, "<div>1 of 2<span>unmanaged workloads would have slept over the last 7 days</span></div>")
	assert.NotContains(t, got, "wakes over", "wakes are per workload, in the table")
	assert.Contains(t, got, "<div>2 of 5<span>managed by Hybernate: 1 live, 1 in dry-run</span></div>")
	assert.Contains(t, got, "This report covers one cluster, at list prices, from one scan. Hybernate Hub shows "+
		"every cluster together")
	assert.Contains(t, got, "Every cluster together, with verified savings")
	assert.Contains(t, got, `data-filter="namespaces"`, "namespaces can be filtered like workloads")
	assert.Contains(t, got, `<td class="ref">statefulset/postgres<span class="sub">preview-42</span></td>`,
		"the namespace under the workload, not a column of its own")
	assert.Contains(t, got, `167h<span class="sub">0 wakes</span>`, "wakes under asleep")
	assert.NotContains(t, got, ">Replicas<")
	assert.NotContains(t, got, ">CPU<")
	assert.Contains(t, got, `<table data-sortable id="namespaces">`)
	assert.Contains(t, got, "Saved this month")
	assert.Contains(t, got, "Could save / month")
	assert.Contains(t, got, `<span class="state active"><span class="dot"></span>active (unmanaged)</span>`+
		`<span class="because">CPU 64% of its request</span>`, "the evidence sits under the state it explains")
	assert.NotContains(t, got, "deployment/agent", "workloads that couldn't be judged go in the notes")
	assert.Contains(t, got, "1 workload set no CPU requests")
	assert.Contains(t, got, `41h<span class="sub">4 wakes<br>since Oct 1</span>`,
		"what dry-run measured is in its columns, with the period it covers")
	assert.Contains(t, got, `<span class="because">no activity for 5h</span>`,
		"the line under a dry-run workload's state explains the state again")
	assert.NotContains(t, got, "Measured in dry-run", "no separate section")
	assert.Contains(t, got, "2 found; select a column to sort")
	assert.Contains(t, got, "<td>PGHOST</td>\n        <td>not connected yet</td>",
		"a dry-run workload's dependency, until Hybernate applies the dependencies it finds")
	assert.NotContains(t, got, "hybernate.io/depends-on=", "Hybernate connects dependencies itself")
	assert.NotContains(t, got, "headless")
	assert.Contains(t, got, "messaging/statefulset/nats")
	assert.NotContains(t, got, "postgres-0.postgres-hl", "addresses stay out of a report that gets forwarded")
	assert.NotContains(t, got, "nats://", "addresses stay out of a report that gets forwarded")
	assert.Contains(t, got, "after its idle-after with no activity (1h, or a managed workload&#39;s own)")
	assert.Contains(t, got, "<p>Every Deployment and StatefulSet in the cluster: what each is doing right now, "+
		"what Hybernate saves pausing the live ones, and what pausing the rest could save.</p>")
	assert.Contains(t, got, "with their own CPU threshold and idle-after", "the details are in the method section")
	assert.Contains(t, got, `<span class="line"><span class="sh-cmd">kubectl</span> label statefulset postgres `+
		`<span class="sh-flag">-n</span> preview-42 <span class="sh-key">hybernate.io/managed</span>=`+
		`<span class="sh-val">true</span></span>`, "coloured like a shell")
	assert.Contains(t, got, `hybernate enable deployment/api <span class="sh-flag">-n</span> preview-7`,
		"enable the one dry-run measured")
	assert.NotContains(t, got, "<script src", "nothing loads from elsewhere")
	assert.NotContains(t, got, "<link", "nothing loads from elsewhere")
}

// A workload that ran four pods all week and runs one now could save more
// a month than it costs now. The share is of what it cost over the same
// history, so it's never over 100%.
func TestHeadlineFor_SavingsShareIsOfTheirOwnCost(t *testing.T) {
	totals := discovery.Totals{Workloads: 1, MonthlyCost: 36.5, SavingsBasis: 146,
		Replayed: discovery.ReplayTotals{Workloads: 1, Sleepers: 1, MonthlyFreed: 145}}

	h, _ := headlineFor(totals, "the last 7 days")

	require.Len(t, h.Figures, 2)
	assert.Equal(t, "$145", h.Figures[1].Value)
	assert.Equal(t, "99% of what these workloads cost over the same time", h.Figures[1].Detail)
}

func TestWriteHTML_ByNamespace(t *testing.T) {
	page := buildReport(historyResult(), time.Hour)

	require.NotEmpty(t, page.Namespaces)
	assert.Equal(t, "preview-42", page.Namespaces[0].Namespace, "most freed first")
	assert.Equal(t, "$139", page.Namespaces[0].CouldSave)
	for _, ns := range page.Namespaces {
		if ns.Namespace == "preview-3" {
			assert.Equal(t, "$0", ns.Cost, "a paused workload isn't costing anything")
		}
		if ns.Namespace == "preview-7" {
			assert.Equal(t, "$40", ns.CouldSave, "dry-run's could-save is what Hybernate measured")
		}
	}
}

// A workload Hybernate already pauses isn't counted in what pausing would
// free, by namespace as in the headline.
func TestWriteHTML_ByNamespaceLeavesOutManaged(t *testing.T) {
	result := historyResult()
	web := &result.Workloads[2]
	require.Equal(t, "web", web.Name)
	web.History = &discovery.History{Hours: 168, RunningHours: 40, SleepHours: 30, MonthlyFreed: 99}

	page := buildReport(result, time.Hour)

	for _, ns := range page.Namespaces {
		if ns.Namespace == "preview-3" {
			assert.Equal(t, "$0", ns.CouldSave)
		}
	}
}

// Each money cell has one source: what Hybernate saved, for a live
// workload; what dry-run measured, for one in dry-run; what history shows,
// for an unmanaged one.
func TestWriteHTML_OneSourcePerCell(t *testing.T) {
	page := buildReport(historyResult(), time.Hour)

	byName := map[string]workloadRow{}
	for _, row := range page.Workloads {
		byName[row.Workload] = row
	}
	postgres, api, web := byName["statefulset/postgres"], byName["deployment/api"], byName["deployment/web"]
	assert.Equal(t, []string{"167h", "0", "-", "$139"}, []string{postgres.CouldSleep, postgres.Wakes, postgres.Saved,
		postgres.CouldSave}, "unmanaged: history")
	assert.Equal(t, []string{"41h", "4", "-", "$40"}, []string{api.CouldSleep, api.Wakes, api.Saved, api.CouldSave},
		"dry-run: measured, not history")
	assert.Equal(t, []string{"-", "-", "$4", "-"}, []string{web.CouldSleep, web.Wakes, web.Saved, web.CouldSave},
		"live: what it saved")
	assert.Less(t, web.CouldSleepSort, byName["deployment/ledger"].CouldSleepSort, "a dash sorts below 0h")
}

func TestWriteHTML_Snapshot(t *testing.T) {
	result := sampleCluster()
	result.ScannedAt = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	result.Settings = settings{CPUThreshold: 10}
	result.Prices = prices{CPUPerHour: 0.045, MemoryPerHour: 0.006}

	got := renderHTML(t, result)

	assert.Contains(t, got, "A snapshot of CPU at the time of the scan, without history.")
	assert.Contains(t, got, "Your prices: $0.045 per vCPU-hour and $0.006 per GiB-hour of memory.")
	assert.Contains(t, got, `<div class="figure"><b>$0</b><span>saved by Hybernate this month</span>`+
		`<small>nothing is live yet</small></div>`)
	assert.Contains(t, got,
		`<div class="figure"><b>$1,240</b><span>a month, what idle workloads cost while running</span>`)
	assert.Contains(t, got, "<div>$1.70<span>an hour, what idle workloads cost while running</span></div>",
		"no history, so no wakes: the hourly cost of idle in its place")
	assert.Contains(t, got, "this report doesn&#39;t estimate a saving", "one moment can't show a saving")
	assert.NotContains(t, got, "Could save / month", "no history, no dry-run: nothing to say it could save")
	assert.NotContains(t, got, "Saved this month", "nothing live")
}

func TestWriteHTMLFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.html")

	written, err := writeHTMLFile(path, historyResult(), time.Hour)

	require.NoError(t, err)
	assert.Equal(t, path, written)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "Workload scan")
	_, err = writeHTMLFile(filepath.Join(t.TempDir(), "missing", "report.html"), historyResult(), time.Hour)
	assert.Error(t, err)
}

// The report names the cluster's workloads, so a temporary one is a new
// file only the user can read, never one someone else made or linked.
func TestWriteHTMLFile_Temporary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	predictable := filepath.Join(dir, "hybernate-scan-20261003-090000.html")
	target := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.Symlink(target, predictable))

	first, err := writeHTMLFile("", historyResult(), time.Hour)
	require.NoError(t, err)
	second, err := writeHTMLFile("", historyResult(), time.Hour)
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "each scan its own file")
	assert.NotEqual(t, predictable, first)
	assert.NoFileExists(t, target, "a planted link isn't followed")
	info, err := os.Stat(first)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Regexp(t, `^hybernate-scan-\d+\.html$`, filepath.Base(first))
}

func TestWriteReport(t *testing.T) {
	result := historyResult()
	tests := []struct {
		name        string
		opts        scanOptions
		terminal    bool
		openFails   bool
		wantFile    bool
		wantOpened  bool
		wantMessage string
	}{
		{name: "a terminal opens it from a temporary file", opts: scanOptions{output: "table", open: true},
			terminal: true, wantFile: true, wantOpened: true, wantMessage: "opened in your browser; --open=false to skip"},
		{name: "--open=false doesn't", opts: scanOptions{output: "table"}, terminal: true},
		{name: "piped output doesn't", opts: scanOptions{output: "table", open: true}},
		{name: "JSON doesn't", opts: scanOptions{output: "json", open: true}, terminal: true},
		{name: "--html saves it anyway", opts: scanOptions{output: "json", html: "report.html"},
			wantFile: true, wantMessage: "Report: "},
		{name: "a browser that won't open is reported", opts: scanOptions{output: "table", open: true},
			terminal: true, openFails: true, wantFile: true, wantOpened: true, wantMessage: "couldn't open a browser"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			if tt.opts.html != "" {
				tt.opts.html = filepath.Join(dir, tt.opts.html)
			}
			var opened string
			restoreBrowse, restoreInteractive := browse, interactive
			t.Cleanup(func() { browse, interactive = restoreBrowse, restoreInteractive })
			interactive = func(io.Writer) bool { return tt.terminal }
			browse = func(path string) error {
				opened = path
				if tt.openFails {
					return errors.New("no browser")
				}
				return nil
			}
			var stderr bytes.Buffer

			require.NoError(t, writeReport(&bytes.Buffer{}, &stderr, result, tt.opts))

			files, err := filepath.Glob(filepath.Join(dir, "*.html"))
			require.NoError(t, err)
			if !tt.wantFile {
				assert.Empty(t, files)
				assert.Empty(t, stderr.String())
				return
			}
			require.Len(t, files, 1)
			assert.Contains(t, stderr.String(), tt.wantMessage)
			assert.Contains(t, stderr.String(), files[0])
			if tt.wantOpened {
				assert.Equal(t, files[0], opened)
				assert.Regexp(t, `^hybernate-scan-\d+\.html$`, filepath.Base(files[0]))
			} else {
				assert.Empty(t, opened)
			}
		})
	}
}

// Hybernate doesn't record how long it has had live workloads paused yet,
// so the column only appears once it does.
func TestWriteHTML_SleptThisMonth(t *testing.T) {
	assert.NotContains(t, renderHTML(t, historyResult()), "Slept this month", "no data, no column")

	result := historyResult()
	result.Workloads[2].Slept = &discovery.Slept{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Hours: 61, Wakes: 3}

	got := renderHTML(t, result)

	assert.Contains(t, got, "Slept this month")
	assert.Contains(t, got, `61h<span class="sub">3 wakes<br>since Oct 1</span>`)
	assert.Contains(t, got, "<dt>Slept</dt>")
}

func TestRoundedDuration(t *testing.T) {
	tests := map[time.Duration]string{
		0:                                     "0s",
		10 * time.Second:                      "10s",
		30 * time.Second:                      "30s",
		time.Minute + 10*time.Second:          "1m10s",
		time.Hour:                             "1h",
		90 * time.Minute:                      "1h30m",
		2*time.Hour + 5*time.Second:           "2h5s",
		20 * time.Minute:                      "20m",
		48 * time.Hour:                        "48h",
		1500 * time.Millisecond:               "2s",
		-time.Hour:                            "-1h",
		time.Hour + time.Minute + time.Second: "1h1m1s",
	}
	for in, want := range tests {
		assert.Equal(t, want, roundedDuration(in), in.String())
	}
}
