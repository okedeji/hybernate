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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/discovery"
)

const hubURL = "https://okedeji.io/hybernate/hub"

// defaultCPUThreshold matches the activity clock's default, so the scan
// calls idle what Hybernate would pause.
const defaultCPUThreshold = 10

type scanOptions struct {
	contexts     []string
	allContexts  bool
	namespaces   []string
	exclude      []string
	output       string
	limit        int
	cpuThreshold int
	cpuPrice     float64
	memoryPrice  float64
	window       string
	idleAfter    time.Duration
	promURL      string
}

// clusterScan is one context's scan, or why it couldn't be scanned.
type clusterScan struct {
	// Context is the kubeconfig context; Cluster is how it's shown.
	Context string `json:"context"`
	Cluster string `json:"cluster"`
	*discovery.ClusterReport
	// HistoryAccess is what an admin can run to let the user read the
	// cluster's Prometheus, when the scan found one it wasn't allowed to.
	HistoryAccess []string `json:"historyAccess,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// prices are what costs were calculated with, and whether they're the
// built-in assumption rather than the user's own.
type prices struct {
	CPUPerHour    float64 `json:"cpuPerHour"`
	MemoryPerHour float64 `json:"memoryPerHour"`
	Assumed       bool    `json:"assumed"`
}

type scanResult struct {
	Clusters []clusterScan    `json:"clusters"`
	Prices   prices           `json:"prices"`
	Totals   discovery.Totals `json:"totals"`
}

func scanCmd() *cobra.Command {
	opts := scanOptions{
		cpuThreshold: defaultCPUThreshold,
		cpuPrice:     cost.DefaultRates.CPUPerHour,
		memoryPrice:  cost.DefaultRates.MemoryPerHour,
		window:       "7d",
		idleAfter:    time.Hour,
	}
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Find idle workloads and what they cost",
		Long: `Scan reads your clusters and shows which Deployments and StatefulSets look
idle, what they reserve, and what pausing them could save. It only reads,
with your own kubeconfig, and needs nothing installed in the cluster.

With a Prometheus in the cluster, found automatically and queried through
the API server, it replays Hybernate's activity clock over the last week of
CPU and rollouts: how long each workload would have slept, how often it
would have been woken, and what that would have freed. Without one, it
judges from CPU right now.

Examples:
  # Scan the current context
  kubectl hybernate scan

  # Scan several clusters, with a combined total
  kubectl hybernate scan --context staging --context sandboxes
  kubectl hybernate scan --all-contexts

  # One namespace, as JSON
  kubectl hybernate scan -n sandbox-42 -o json

  # A month of history from a Prometheus outside the cluster
  kubectl hybernate scan --window 30d --prometheus-url https://thanos.example.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.output != "table" && opts.output != "json" && opts.output != "yaml" {
				return fmt.Errorf("unknown output %q: use table, json, or yaml", opts.output)
			}
			window, err := parseWindow(opts.window)
			if err != nil {
				return err
			}
			if opts.idleAfter <= 0 {
				return errors.New("--idle-after must be more than zero")
			}
			contexts, err := scanContexts(opts)
			if err != nil {
				return err
			}
			result := scanResult{Prices: pricesFor(cmd, opts)}
			for _, name := range contexts {
				result.Clusters = append(result.Clusters, scanContext(cmd.Context(), name, window, opts))
			}
			nameClusters(result.Clusters)
			result.Totals = combinedTotals(result.Clusters)
			return writeScan(cmd.OutOrStdout(), result, opts)
		},
	}
	cmd.Flags().StringSliceVar(&opts.contexts, "context", nil,
		"Kubeconfig context to scan; repeat for several (defaults to the current one)")
	cmd.Flags().BoolVar(&opts.allContexts, "all-contexts", false, "Scan every context in the kubeconfig")
	cmd.Flags().StringSliceVarP(&opts.namespaces, "namespace", "n", nil,
		"Namespace to scan; repeat for several (defaults to all you can read)")
	cmd.Flags().StringSliceVar(&opts.exclude, "exclude-namespaces", discovery.SystemNamespaces, "Namespaces to skip")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "table", "Output format: table, json, or yaml")
	cmd.Flags().IntVar(&opts.limit, "limit", 25,
		"Workloads to list per cluster in the table, most savings first (0 for all)")
	cmd.Flags().IntVar(&opts.cpuThreshold, "cpu-threshold", defaultCPUThreshold,
		"CPU use, as a percentage of requests, below which a workload counts as idle")
	cmd.Flags().Float64Var(&opts.cpuPrice, "cpu-price", opts.cpuPrice, "Your price per vCPU-hour, in dollars")
	cmd.Flags().Float64Var(&opts.memoryPrice, "memory-price", opts.memoryPrice,
		"Your price per GiB-hour of memory, in dollars")
	cmd.Flags().StringVar(&opts.window, "window", opts.window,
		"How much Prometheus history to replay, such as 7d or 36h; 0 judges from CPU right now only")
	cmd.Flags().DurationVar(&opts.idleAfter, "idle-after", opts.idleAfter,
		"How long without activity before the replay pauses a workload, as Hybernate's idleAfter")
	cmd.Flags().StringVar(&opts.promURL, "prometheus-url", "",
		"Prometheus API to read history from, such as Thanos or Mimir (defaults to one found in the cluster)")
	return cmd
}

// pricesFor says what costs are calculated with, and whether those are the
// built-in assumption because the user gave no prices of their own.
func pricesFor(cmd *cobra.Command, opts scanOptions) prices {
	return prices{
		CPUPerHour:    opts.cpuPrice,
		MemoryPerHour: opts.memoryPrice,
		Assumed:       !cmd.Flags().Changed("cpu-price") && !cmd.Flags().Changed("memory-price"),
	}
}

func scanContexts(opts scanOptions) ([]string, error) {
	if !opts.allContexts {
		if len(opts.contexts) > 0 {
			return opts.contexts, nil
		}
		return []string{""}, nil
	}
	config, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	var names []string
	for name := range config.Contexts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// parseWindow reads a duration that may be in days, which
// time.ParseDuration doesn't know.
func parseWindow(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("--window %q isn't a number of days, such as 7d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--window %q isn't a duration, such as 7d or 36h", s)
	}
	return d, nil
}

func scanContext(ctx context.Context, contextName string, window time.Duration, opts scanOptions) clusterScan {
	config, _, current, err := kubeConfigFor(contextName)
	scan := clusterScan{Context: current}
	if err != nil {
		scan.Error = err.Error()
		return scan
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		scan.Error = fmt.Sprintf("creating client: %v", err)
		return scan
	}
	namespaces, err := discovery.Namespaces(ctx, c, opts.namespaces, opts.exclude)
	if errors.Is(err, discovery.ErrCantListNamespaces) {
		scan.Error = "you can't list namespaces in this cluster; name the ones to scan with -n"
		return scan
	}
	if err != nil {
		scan.Error = err.Error()
		return scan
	}
	var history *discovery.Prometheus
	var historyNote string
	if window > 0 {
		var err error
		history, err = historySource(ctx, c, config, namespaces, opts.promURL)
		var forbidden *discovery.ProxyForbiddenError
		if errors.As(err, &forbidden) {
			scan.HistoryAccess = historyAccess(ctx, c, forbidden)
		}
		if err != nil {
			historyNote = err.Error() + "; judging from CPU right now"
		}
	}
	scanner := discovery.NewScanner(c, c)
	report, err := scanner.ScanCluster(ctx, discovery.ClusterOptions{
		Namespaces:   namespaces,
		CPUThreshold: opts.cpuThreshold,
		Rates:        cost.Rates{CPUPerHour: opts.cpuPrice, MemoryPerHour: opts.memoryPrice},
		Now:          time.Now,
		History:      history,
		Window:       window,
		IdleAfter:    opts.idleAfter,
	})
	if err != nil {
		scan.Error = err.Error()
		return scan
	}
	if historyNote != "" {
		report.Notes = append([]string{historyNote}, report.Notes...)
	}
	if len(namespaces) > 0 {
		report.Notes = append(discovery.AccessNotes(ctx, c, namespaces[0]), report.Notes...)
	}
	scan.ClusterReport = report
	return scan
}

// historySource finds the Prometheus to replay history from, or says why
// there's none to use.
func historySource(ctx context.Context, c client.Client, config *rest.Config, namespaces []string, promURL string) (
	*discovery.Prometheus, error) {
	var prom *discovery.Prometheus
	if promURL != "" {
		p, err := discovery.NewPrometheusURL(promURL, &http.Client{})
		if err != nil {
			return nil, err
		}
		prom = p
	} else {
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return nil, fmt.Errorf("can't look for Prometheus: %w", err)
		}
		p, err := discovery.FindPrometheus(ctx, c, clientset.CoreV1().RESTClient(), namespaces)
		if errors.Is(err, discovery.ErrNoPrometheus) {
			return nil, errors.New("no Prometheus found in the cluster, so there's no history to replay; " +
				"pass --prometheus-url for one elsewhere")
		}
		if err != nil {
			return nil, fmt.Errorf("can't look for Prometheus: %w", err)
		}
		prom = p
	}
	if err := prom.Check(ctx); err != nil {
		return nil, err
	}
	return prom, nil
}

// historyAccess is what an admin can run to let the user read Prometheus
// through the API server: get on services/proxy for its one Service and
// port, in its namespace. The proxy's resource name is the Service and
// port together, so the Role names both.
func historyAccess(ctx context.Context, c client.Client, f *discovery.ProxyForbiddenError) []string {
	subject := "--user=<you>"
	review := &authenticationv1.SelfSubjectReview{}
	if err := c.Create(ctx, review); err == nil && review.Status.UserInfo.Username != "" {
		subject = bindingSubject(review.Status.UserInfo.Username)
	}
	const role = "hybernate-scan"
	return []string{
		fmt.Sprintf("kubectl create role %s -n %s --verb=get --resource=services/proxy --resource-name=%s:%s",
			role, f.Namespace, f.Service, f.Port),
		fmt.Sprintf("kubectl create rolebinding %s -n %s --role=%s %s", role, f.Namespace, role, subject),
	}
}

// bindingSubject is how kubectl create rolebinding names a user, or a
// service account by its namespace and name.
func bindingSubject(username string) string {
	if sa, ok := strings.CutPrefix(username, "system:serviceaccount:"); ok {
		return "--serviceaccount=" + sa
	}
	return "--user=" + username
}

func combinedTotals(clusters []clusterScan) discovery.Totals {
	var t discovery.Totals
	for _, c := range clusters {
		if c.ClusterReport == nil {
			continue
		}
		t.Workloads += c.Totals.Workloads
		t.MonthlyCost += c.Totals.MonthlyCost
		t.Paused += c.Totals.Paused
		t.PausedHourlyCost += c.Totals.PausedHourlyCost
		t.Idle += c.Totals.Idle
		t.IdleCPUMillis += c.Totals.IdleCPUMillis
		t.IdleMemoryBytes += c.Totals.IdleMemoryBytes
		t.IdleHourlyCost += c.Totals.IdleHourlyCost
		t.IdleMonthlyCost += c.Totals.IdleMonthlyCost
		t.Replayed.Workloads += c.Totals.Replayed.Workloads
		t.Replayed.Sleepers += c.Totals.Replayed.Sleepers
		t.Replayed.SleepHours += c.Totals.Replayed.SleepHours
		t.Replayed.Wakes += c.Totals.Replayed.Wakes
		t.Replayed.Freed += c.Totals.Replayed.Freed
		t.Replayed.MonthlyFreed += c.Totals.Replayed.MonthlyFreed
	}
	return t
}

func writeScan(w io.Writer, result scanResult, opts scanOptions) error {
	switch opts.output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	case "yaml":
		out, err := yaml.Marshal(result)
		if err != nil {
			return fmt.Errorf("encoding yaml: %w", err)
		}
		_, err = w.Write(out)
		return err
	default:
		return writeTable(w, result, opts.limit)
	}
}

func writeTable(w io.Writer, result scanResult, limit int) error {
	p := &printer{w: w}
	for _, c := range result.Clusters {
		if c.ClusterReport == nil {
			p.line("%s: not scanned: %s", c.Cluster, c.Error)
			p.line("")
			continue
		}
		namespaces := map[string]bool{}
		for _, wl := range c.Workloads {
			namespaces[wl.Namespace] = true
		}
		p.line("%s: %s in %s", c.Cluster, countOf(c.Totals.Workloads, "workload"), countOf(len(namespaces), "namespace"))
		p.line("")
		writeHeadline(p, c.Totals, replayedOver(c.History))
		judged, unjudged := splitJudged(c.Workloads)
		if len(judged) > 0 {
			writeWorkloads(p, judged, limit, c.Mode == discovery.ModeHistory)
		}
		writeMeasured(p, c.Workloads)
		notes := append(unjudgedNotes(unjudged), c.Notes...)
		if len(notes) > 0 {
			p.line("Notes:")
			for _, n := range notes {
				p.line("  - %s", n)
			}
			p.line("")
		}
		if len(c.HistoryAccess) > 0 {
			p.line("To replay history, an admin can let you read Prometheus, and nothing else, with:")
			for _, command := range c.HistoryAccess {
				p.line("  %s", command)
			}
			p.line("Or pass --prometheus-url if Prometheus is reachable from your machine.")
			p.line("")
		}
	}
	if len(result.Clusters) > 1 {
		p.line("All clusters:")
		writeHeadline(p, result.Totals, "their history")
	}
	if result.Prices.Assumed {
		p.line("Costs use assumed list prices: $%.3f per vCPU-hour and $%.3f per GiB-hour of memory,",
			result.Prices.CPUPerHour, result.Prices.MemoryPerHour)
		p.line("from AWS on-demand in us-east-1. Pass --cpu-price and --memory-price for yours.")
		p.line("")
	}
	if result.Totals.Idle > 0 && result.Totals.Replayed.Workloads == 0 {
		p.line("What pausing would save depends on how often they'd be woken, which one moment can't show.")
		p.line("Labelling them dry-run measures it, below; scanning with Prometheus history estimates it.")
		p.line("")
	}
	writeNextSteps(p, result)
	p.line("Hybernate Hub verifies savings against your cloud bill, with history across all your")
	p.line("clusters. Free for up to 2 clusters: %s", hubURL)
	return p.err
}

func writeHeadline(p *printer, t discovery.Totals, replayed string) {
	if t.Paused > 0 {
		p.line("  Hybernate has %s paused right now, freeing %s an hour.",
			countOf(t.Paused, "workload"), cents(t.PausedHourlyCost))
	}
	switch t.Idle {
	case 0:
		p.line("  No running workloads are idle right now.")
	default:
		verb, they, costs := "are", "They", "cost"
		if t.Idle == 1 {
			verb, they, costs = "is", "It", "costs"
		}
		p.line("  %s %s idle right now, reserving %s and %s of memory.",
			countOf(t.Idle, "workload"), verb, vcpu(t.IdleCPUMillis), gib(t.IdleMemoryBytes))
		p.line("  %s %s %s/month while running, %s an hour: each hour asleep frees that.",
			they, costs, dollars(t.IdleMonthlyCost), cents(t.IdleHourlyCost))
	}
	if r := t.Replayed; r.Workloads > 0 && replayed != "" {
		if r.Sleepers == 0 {
			p.line("  Replaying %s, nothing Hybernate doesn't already pause would have slept.", replayed)
		} else {
			p.line("  Replaying %s, Hybernate would have paused %s for %s in all, waking them %s,",
				replayed, countOf(r.Sleepers, "workload"), hoursTotal(r.SleepHours), countOf(r.Wakes, "time"))
			p.line("  and freed %s: about %s/month.", dollars(r.Freed), dollars(r.MonthlyFreed))
		}
	}
	p.line("")
}

// replayedOver says what a cluster's history replay covered, or "" when
// it had none.
func replayedOver(h *discovery.HistorySource) string {
	if h == nil || h.Hours == 0 {
		return ""
	}
	if h.Hours >= 48 {
		return fmt.Sprintf("the last %d days", int(h.Hours/24+0.5))
	}
	return fmt.Sprintf("the last %d hours", int(h.Hours+0.5))
}

// hoursTotal writes a number of hours with thousands separated.
func hoursTotal(h float64) string {
	return strings.TrimPrefix(dollars(h), "$") + " hours"
}

// splitJudged separates the workloads the scan could judge from those it
// couldn't, which are summed up in a note instead of filling the table.
func splitJudged(workloads []Workload) (judged, unjudged []Workload) {
	for _, wl := range workloads {
		if wl.State == discovery.StateUnknown {
			unjudged = append(unjudged, wl)
		} else {
			judged = append(judged, wl)
		}
	}
	return judged, unjudged
}

var unmeasuredExplained = map[string]string{
	"no CPU requests": "set no CPU requests, so their use can't be measured",
	"no metrics yet":  "have no metrics yet, usually because their pods just started",
	"no Metrics API":  "couldn't be measured without the Metrics API",
}

func unjudgedNotes(unjudged []Workload) []string {
	names := map[string][]string{}
	var reasons []string
	for _, wl := range unjudged {
		if _, seen := names[wl.Unmeasured]; !seen {
			reasons = append(reasons, wl.Unmeasured)
		}
		names[wl.Unmeasured] = append(names[wl.Unmeasured], wl.Namespace+"/"+wl.Name)
	}
	notes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		list := names[reason]
		shown := list
		if len(shown) > 3 {
			shown = shown[:3]
		}
		listed := strings.Join(shown, ", ")
		if more := len(list) - len(shown); more > 0 {
			listed += fmt.Sprintf(" and %d more", more)
		}
		notes = append(notes, fmt.Sprintf("%s %s: %s", countOf(len(list), "workload"), unmeasuredExplained[reason], listed))
	}
	return notes
}

func writeWorkloads(p *printer, workloads []Workload, limit int, history bool) {
	shown := workloads
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
	if history {
		_, _ = fmt.Fprintln(tw, "  NAMESPACE\tWORKLOAD\tSTATE\tCPU\tREPLICAS\tCOST/MO\tIDLE\tASLEEP\tWAKES\tFREES/MO\tWHY")
	} else {
		_, _ = fmt.Fprintln(tw, "  NAMESPACE\tWORKLOAD\tSTATE\tCPU\tREPLICAS\tCOST/MO\tCOST/HOUR\tWHY")
	}
	for _, wl := range shown {
		cpu := "-"
		if wl.CPUPercent != nil {
			cpu = fmt.Sprintf("%d%%", *wl.CPUPercent)
		}
		state := string(wl.State)
		switch {
		case wl.DryRun:
			state += " (dry-run)"
		case wl.Managed:
			state += " (managed)"
		}
		why := strings.Join(append([]string{wl.Reason}, wl.Clues...), "; ")
		ref := strings.ToLower(string(wl.Kind)) + "/" + wl.Name
		if !history {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				wl.Namespace, ref, state, cpu, wl.Replicas, dollars(wl.MonthlyCost), cents(wl.HourlyCost), why)
			continue
		}
		idle, asleep, wakes, frees := "-", "-", "-", "-"
		if h := wl.History; h != nil {
			idle = fmt.Sprintf("%s of %s", hours(h.IdleHours), hours(h.RunningHours))
			asleep, wakes, frees = hours(h.SleepHours), strconv.Itoa(h.Wakes), dollars(h.MonthlyFreed)
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			wl.Namespace, ref, state, cpu, wl.Replicas, dollars(wl.MonthlyCost), idle, asleep, wakes, frees, why)
	}
	_ = tw.Flush()
	if len(shown) < len(workloads) {
		p.line("  ...and %d more; --limit 0 lists them all", len(workloads)-len(shown))
	}
	p.line("")
}

func writeNextSteps(p *printer, result scanResult) {
	var idle, measuring *Workload
	installed := false
	for _, c := range result.Clusters {
		if c.ClusterReport == nil {
			continue
		}
		for i := range c.Workloads {
			wl := &c.Workloads[i]
			installed = installed || wl.Managed
			if idle == nil && result.Totals.Idle > 0 && wl.State == discovery.StateIdle && !wl.Managed {
				idle = wl
			}
			if measuring == nil && wl.Measured != nil {
				measuring = wl
			}
		}
	}
	if idle == nil && measuring == nil {
		return
	}
	p.line("Next steps:")
	step := 0
	next := func(title string) {
		step++
		p.line("  %d. %s", step, title)
	}
	if idle != nil {
		// Managed workloads show Hybernate is installed; without any, it
		// may or may not be, so the install step is shown.
		if !installed {
			next("Install Hybernate in the cluster:")
			p.line("       helm install hybernate oci://ghcr.io/okedeji/charts/hybernate -n hybernate-system --create-namespace")
		}
		kind := strings.ToLower(string(idle.Kind))
		next("Measure a workload first; nothing is paused in dry-run:")
		p.line("       kubectl label %s %s -n %s hybernate.io/managed=true", kind, idle.Name, idle.Namespace)
		p.line("       kubectl annotate %s %s -n %s hybernate.io/dry-run=true", kind, idle.Name, idle.Namespace)
	}
	if measuring != nil {
		next("When you're happy with what dry-run measured, start pausing:")
	} else {
		next("When you're happy with what it measures, start pausing it while idle:")
		measuring = idle
	}
	p.line("       kubectl hybernate enable %s/%s -n %s",
		strings.ToLower(string(measuring.Kind)), measuring.Name, measuring.Namespace)
	p.line("")
}

// writeMeasured lists what dry-run has measured for workloads in dry-run.
func writeMeasured(p *printer, workloads []Workload) {
	var measured []Workload
	for _, wl := range workloads {
		if wl.Measured != nil {
			measured = append(measured, wl)
		}
	}
	if len(measured) == 0 {
		return
	}
	p.line("  Measured in dry-run, had Hybernate been pausing them:")
	tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
	for _, wl := range measured {
		m := wl.Measured
		_, _ = fmt.Fprintf(tw, "  %s\t%s/%s\tsince %s: would have paused %s, slept %s, freeing %s\n",
			wl.Namespace, strings.ToLower(string(wl.Kind)), wl.Name, m.Since.Format("Jan 2"),
			countOf(m.Pauses, "time"), hours(m.SleptHours), cents(m.Freed))
	}
	_ = tw.Flush()
	p.line("")
}

// hours is a length of time to the hour, or the minute under one.
func hours(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%dm", int(h*60))
	}
	return fmt.Sprintf("%dh", int(h))
}

// nameClusters gives each scanned cluster a readable name. EKS and GKE
// contexts are long generated identifiers, so they're shortened to the
// cluster with its provider and region. A name that would match another
// keeps its full context, so two clusters never look like one.
func nameClusters(clusters []clusterScan) {
	count := map[string]int{}
	for i := range clusters {
		clusters[i].Cluster = shortClusterName(clusters[i].Context)
		count[clusters[i].Cluster]++
	}
	for i := range clusters {
		if count[clusters[i].Cluster] > 1 || clusters[i].Cluster == "" {
			clusters[i].Cluster = clusters[i].Context
		}
		if clusters[i].Cluster == "" {
			clusters[i].Cluster = "current context"
		}
	}
}

// shortClusterName recognises the context names EKS and GKE generate and
// leaves anything else as it is.
//
//	arn:aws:eks:us-east-1:123456789012:cluster/staging -> staging (EKS us-east-1)
//	gke_myproject_europe-west1_sandboxes               -> sandboxes (GKE europe-west1)
//
// GKE's form is unambiguous because project IDs, locations, and cluster names
// can't contain underscores.
func shortClusterName(contextName string) string {
	if arn, ok := strings.CutPrefix(contextName, "arn:aws:eks:"); ok {
		parts := strings.SplitN(arn, ":", 3)
		if len(parts) == 3 {
			if name, ok := strings.CutPrefix(parts[2], "cluster/"); ok && name != "" && parts[0] != "" {
				return fmt.Sprintf("%s (EKS %s)", name, parts[0])
			}
		}
		return contextName
	}
	if gke, ok := strings.CutPrefix(contextName, "gke_"); ok {
		parts := strings.Split(gke, "_")
		if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
			return fmt.Sprintf("%s (GKE %s)", parts[2], parts[1])
		}
	}
	return contextName
}

// Workload is a scanned workload, named here for the table's helpers.
type Workload = discovery.Workload

// printer writes lines and keeps the first error, so the table code reads
// straight down.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n, err := p.w.Write(b)
	p.err = err
	return n, err
}

func (p *printer) line(format string, args ...any) {
	_, _ = fmt.Fprintf(p, format+"\n", args...)
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func vcpu(millis int64) string {
	return fmt.Sprintf("%.1f vCPU", float64(millis)/1000)
}

func gib(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
}

// cents formats an amount to the cent, for costs per hour. A cost below a
// cent isn't free, so it isn't shown as $0.00.
func cents(amount float64) string {
	if amount > 0 && amount < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", amount)
}

// dollars formats whole dollars with thousands separators.
func dollars(amount float64) string {
	n := int64(amount + 0.5)
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "$" + s
}
