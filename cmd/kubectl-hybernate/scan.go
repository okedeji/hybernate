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
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/discovery"
)

const hubURL = "https://okedeji.io/hybernate/hub"

// defaultCPUThreshold matches the activity clock's default, so the scan
// calls idle what Hybernate would pause.
const defaultCPUThreshold = 10

type scanOptions struct {
	kube         *kubeFlags
	namespaces   namespaceFlags
	exclude      []string
	output       string
	limit        int
	cpuThreshold int
	cpuPrice     float64
	memoryPrice  float64
	// ownCPUPrice and ownMemoryPrice say the user gave the price, which
	// then prices every workload in place of its nodes' list prices.
	ownCPUPrice    bool
	ownMemoryPrice bool
	window         string
	idleAfter      time.Duration
	timeout        time.Duration
	html           string
	open           bool
	prometheus     prometheusOptions
}

// prometheusOptions say where to read history from and how to reach it.
type prometheusOptions struct {
	url                string
	selector           string
	headers            []string
	bearerTokenFile    string
	caFile             string
	insecureSkipVerify bool
}

// scanResult is a scan of one cluster, with the rules and prices it was
// judged and priced with.
type scanResult struct {
	ScannedAt time.Time `json:"scannedAt"`
	// Context is the kubeconfig context; Cluster is how it's shown.
	Context  string   `json:"context"`
	Cluster  string   `json:"cluster"`
	Settings settings `json:"settings"`
	Prices   prices   `json:"prices"`
	*discovery.ClusterReport
	// HistoryAccess is what an admin can run to let the user read the
	// cluster's Prometheus, when the scan found one it wasn't allowed to.
	HistoryAccess []string `json:"historyAccess,omitempty"`
}

// settings are the rules the scan judged with, so a report can state them.
type settings struct {
	CPUThreshold int    `json:"cpuThreshold"`
	IdleAfter    string `json:"idleAfter"`
	Window       string `json:"window"`
}

func scanCmd(kube *kubeFlags) *cobra.Command {
	opts := scanOptions{
		kube:         kube,
		cpuThreshold: defaultCPUThreshold,
		cpuPrice:     cost.DefaultRates.CPUPerHour,
		memoryPrice:  cost.DefaultRates.MemoryPerHour,
		window:       "7d",
		idleAfter:    time.Hour,
		timeout:      5 * time.Minute,
		open:         true,
	}
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Find idle workloads and what they cost",
		Long: `Scan reads a cluster and shows which Deployments and StatefulSets look idle,
what they reserve, and what pausing them could save. It only reads, with
your own kubeconfig, and needs nothing installed in the cluster.

With a Prometheus in the cluster, found automatically and queried through
the API server, it replays Hybernate's activity clock over the last week of
CPU and rollouts: how long each workload would have slept, how often it
would have been woken, and what that would have freed. Without one, it
judges from CPU right now.

Run in a terminal, it also opens the report as a web page, to share.

Examples:
  # Scan the current context
  kubectl hybernate scan

  # Another cluster in your kubeconfig
  kubectl hybernate scan --context staging

  # One namespace, as JSON
  kubectl hybernate scan -n preview-42 -o json

  # A month of history from a Thanos that holds many clusters
  kubectl hybernate scan --window 30d --prometheus-url https://thanos.example.com \
    --prometheus-selector 'cluster="staging"'

  # History from Grafana Mimir, for one tenant
  kubectl hybernate scan --prometheus-url https://mimir.example.com/prometheus \
    --prometheus-header 'X-Scope-OrgID: team-a'

  # Save the report to send around
  kubectl hybernate scan --html workload-scan.html

  # The terminal only
  kubectl hybernate scan --open=false`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(opts.output); err != nil {
				return err
			}
			window, err := parseWindow(opts.window)
			if err != nil {
				return err
			}
			if opts.idleAfter <= 0 {
				return errors.New("--idle-after must be more than zero")
			}
			if opts.timeout <= 0 {
				return errors.New("--timeout must be more than zero")
			}
			prom, err := prometheusFrom(opts.prometheus)
			if err != nil {
				return err
			}
			opts.ownCPUPrice, opts.ownMemoryPrice = ownPrices(cmd)
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()
			result, err := scanCluster(ctx, window, prom, opts)
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("the scan didn't finish within --timeout %s: the cluster's API server answered too "+
					"slowly; scan fewer namespaces with -n, or pass a longer --timeout", opts.timeout)
			}
			if err != nil {
				return err
			}
			result.ScannedAt = time.Now().UTC().Truncate(time.Second)
			result.Settings = settings{CPUThreshold: opts.cpuThreshold, IdleAfter: roundedDuration(opts.idleAfter),
				Window: opts.window}
			result.Prices = pricesFor(opts)
			if err := writeScan(cmd.OutOrStdout(), result, opts); err != nil {
				return err
			}
			if err := writeReport(cmd.OutOrStdout(), cmd.ErrOrStderr(), result, opts); err != nil {
				return err
			}
			return incompleteError(result)
		},
	}
	addNamespaceFlags(cmd, &opts.namespaces,
		"Namespace to scan; repeat for several (defaults to all you can read)")
	cmd.Flags().StringSliceVar(&opts.exclude, "exclude-namespaces", discovery.SystemNamespaces, "Namespaces to skip")
	addOutputFlag(cmd, &opts.output)
	cmd.Flags().IntVar(&opts.limit, "limit", 25,
		"Workloads to list in the table, most savings first (0 for all)")
	cmd.Flags().IntVar(&opts.cpuThreshold, "cpu-threshold", defaultCPUThreshold,
		"CPU use, as a percentage of requests, at which a workload counts as active; managed workloads use their own")
	cmd.Flags().Float64Var(&opts.cpuPrice, "cpu-price", opts.cpuPrice, "Your price per vCPU-hour, in dollars")
	cmd.Flags().Float64Var(&opts.memoryPrice, "memory-price", opts.memoryPrice,
		"Your price per GiB-hour of memory, in dollars")
	cmd.Flags().StringVar(&opts.window, "window", opts.window,
		"How much Prometheus history to replay, such as 7d or 36h; 0 judges from CPU right now only")
	cmd.Flags().DurationVar(&opts.idleAfter, "idle-after", opts.idleAfter,
		"How long without activity makes a workload idle, as Hybernate's idleAfter; managed workloads use their own")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout,
		"How long the scan may take before it gives up")
	addPrometheusFlags(cmd, &opts.prometheus)
	cmd.Flags().StringVar(&opts.html, "html", "",
		"Save the HTML report to this file, to share (defaults to a temporary file when it opens in a browser)")
	cmd.Flags().BoolVar(&opts.open, "open", opts.open,
		"Open the HTML report in your browser, when the table is shown in a terminal")
	return cmd
}

func addPrometheusFlags(cmd *cobra.Command, o *prometheusOptions) {
	cmd.Flags().StringVar(&o.url, "prometheus-url", "",
		"Prometheus API to read history from, such as Thanos or Mimir (defaults to one found in the cluster). "+
			"Amazon and Google Managed Prometheus need requests signed with SigV4 or OAuth, which the scan "+
			"doesn't do; point this at a signing proxy in front of them")
	cmd.Flags().StringVar(&o.selector, "prometheus-selector", "",
		`Label matchers added to every history query, such as 'cluster="prod"', for a Prometheus that holds `+
			`more than one cluster`)
	cmd.Flags().StringArrayVar(&o.headers, "prometheus-header", nil,
		`Header to send to --prometheus-url, as "Name: value", such as "X-Scope-OrgID: tenant" for Mimir; repeat for several`)
	cmd.Flags().StringVar(&o.bearerTokenFile, "prometheus-bearer-token-file", "",
		"File holding a bearer token to send to --prometheus-url")
	cmd.Flags().StringVar(&o.caFile, "prometheus-ca-file", "",
		"PEM file of CA certificates to trust for --prometheus-url, besides the system's")
	cmd.Flags().BoolVar(&o.insecureSkipVerify, "prometheus-insecure-skip-verify", false,
		"Don't verify --prometheus-url's certificate")
}

// prometheusSetup is how the scan reaches Prometheus, checked before the
// scan starts so a mistyped flag fails at once.
type prometheusSetup struct {
	url      string
	selector discovery.Selector
	client   *http.Client
	header   http.Header
}

func prometheusFrom(o prometheusOptions) (prometheusSetup, error) {
	selector, err := discovery.ParseSelector(o.selector)
	if err != nil {
		return prometheusSetup{}, fmt.Errorf("--prometheus-selector: %w", err)
	}
	setup := prometheusSetup{url: o.url, selector: selector}
	if o.url == "" {
		if len(o.headers) > 0 || o.bearerTokenFile != "" || o.caFile != "" || o.insecureSkipVerify {
			return prometheusSetup{}, errors.New("--prometheus-header, --prometheus-bearer-token-file, " +
				"--prometheus-ca-file and --prometheus-insecure-skip-verify are for --prometheus-url; a Prometheus " +
				"found in the cluster is reached through the API server with your kubeconfig")
		}
		return setup, nil
	}
	setup.header, err = parseHeaders(o.headers)
	if err != nil {
		return prometheusSetup{}, err
	}
	if o.bearerTokenFile != "" {
		if setup.header.Get("Authorization") != "" {
			return prometheusSetup{}, errors.New("--prometheus-bearer-token-file and an Authorization " +
				"--prometheus-header can't both be given")
		}
		token, err := os.ReadFile(o.bearerTokenFile)
		if err != nil {
			return prometheusSetup{}, fmt.Errorf("--prometheus-bearer-token-file: %w", err)
		}
		setup.header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	setup.client, err = discovery.NewHTTPClient(discovery.HTTPOptions{CAFile: o.caFile,
		InsecureSkipVerify: o.insecureSkipVerify})
	if err != nil {
		return prometheusSetup{}, fmt.Errorf("--prometheus-ca-file: %w", err)
	}
	return setup, nil
}

// parseHeaders reads "Name: value" headers.
func parseHeaders(raw []string) (http.Header, error) {
	header := http.Header{}
	for _, h := range raw {
		name, value, ok := strings.Cut(h, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf(`--prometheus-header %q isn't "Name: value"`, h)
		}
		header.Add(name, value)
	}
	return header, nil
}

// validHeaderName says name is an HTTP token, as a header name must be.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alphanumeric && !strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			return false
		}
	}
	return true
}

// incompleteError fails a scan that couldn't read all it should have, once
// its report is written, so a script sees the exit code and a person still
// gets what it did read.
func incompleteError(result scanResult) error {
	if result.ClusterReport == nil || len(result.Incomplete) == 0 {
		return nil
	}
	return fmt.Errorf("the scan of %s is incomplete: %s couldn't be read in full, as the notes say; run it again, "+
		"scan fewer namespaces with -n, or pass a longer --timeout", result.Cluster,
		plural(len(result.Incomplete), "namespace", "namespaces"))
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

func scanCluster(ctx context.Context, window time.Duration, prom prometheusSetup, opts scanOptions) (
	scanResult, error) {
	config, at, err := opts.kube.restConfig(ctx)
	if err != nil {
		return scanResult{}, err
	}
	scan := scanResult{Context: at.context, Cluster: clusterName(at.context)}
	c, err := opts.kube.newClient(config)
	if err != nil {
		return scanResult{}, fmt.Errorf("creating client: %w", err)
	}
	namespaces, err := discovery.Namespaces(ctx, c, opts.namespaces.list(), opts.exclude)
	var namespaceNote string
	if errors.Is(err, discovery.ErrCantListNamespaces) && !opts.namespaces.all {
		namespaces, err = []string{at.namespace}, nil
		namespaceNote = fmt.Sprintf("your access doesn't allow listing namespaces, so only %s, the context's "+
			"namespace, was scanned; name others with -n", at.namespace)
	}
	if errors.Is(err, discovery.ErrCantListNamespaces) {
		return scanResult{}, fmt.Errorf("can't scan %s: to find its workloads, the scan first lists the cluster's "+
			"namespaces, and your access there doesn't allow that; name the namespaces to scan with -n, or ask an "+
			"admin to let you list namespaces", scan.Cluster)
	}
	if err != nil {
		return scanResult{}, fmt.Errorf("listing namespaces in %s: %w", scan.Cluster, err)
	}
	var history *discovery.Prometheus
	var historyNote string
	if window > 0 {
		var err error
		history, err = historySource(ctx, c, config, namespaces, prom)
		if prom.url != "" && err != nil {
			return scanResult{}, fmt.Errorf("reading history from --prometheus-url: %w", err)
		}
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
		Rates: cost.Rates{CPUPerHour: opts.cpuPrice, MemoryPerHour: opts.memoryPrice,
			StoragePerMonth: cost.DefaultRates.StoragePerMonth},
		OwnCPUPrice:     opts.ownCPUPrice,
		OwnMemoryPrice:  opts.ownMemoryPrice,
		Now:             time.Now,
		History:         history,
		Window:          window,
		IdleAfter:       opts.idleAfter,
		NamedNamespaces: len(opts.namespaces.list()) > 0,
	})
	if err != nil {
		return scanResult{}, fmt.Errorf("scanning %s: %w", scan.Cluster, err)
	}
	if historyNote != "" {
		report.Notes = append([]string{historyNote}, report.Notes...)
	}
	if namespaceNote != "" {
		report.Notes = append([]string{namespaceNote}, report.Notes...)
	}
	// The scan carries on past calls that fail, noting what it couldn't
	// read; once the deadline passes they all fail, and what it has isn't
	// the cluster.
	if err := ctx.Err(); err != nil {
		return scanResult{}, err
	}
	scan.ClusterReport = report
	return scan, nil
}

// historySource finds the Prometheus to replay history from, or says why
// there's none to use.
func historySource(ctx context.Context, c client.Client, config *rest.Config, namespaces []string,
	setup prometheusSetup) (*discovery.Prometheus, error) {
	var prom *discovery.Prometheus
	if setup.url != "" {
		p, err := discovery.NewPrometheusURL(setup.url, setup.client, setup.header)
		if err != nil {
			return nil, err
		}
		if err := p.Check(ctx); err != nil {
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
			return nil, err
		}
		prom = p
	}
	prom.Selector = setup.selector
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
		fmt.Sprintf("kubectl create role %s -n %s --verb=get --resource=services/proxy --resource-name=%s",
			role, f.Namespace, f.ResourceName()),
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

func writeScan(w io.Writer, result scanResult, opts scanOptions) error {
	return writeOutput(w, opts.output, result, func() error { return writeTable(w, result, opts.limit) })
}

func writeTable(w io.Writer, result scanResult, limit int) error {
	p := &printer{w: w}
	p.line("%s: %s in %s", result.Cluster, countOf(result.Totals.Workloads, "workload"),
		countOf(result.Namespaces, "namespace"))
	p.line("")
	if result.Totals.Workloads == 0 {
		p.line("  %s", emptySentence(result))
		p.line("")
	} else {
		writeHeadline(p, result.Totals, replayedOver(result.History))
	}
	judged, unjudged := splitJudged(result.Workloads)
	if len(judged) > 0 {
		writeWorkloads(p, judged, limit, result.Mode == discovery.ModeHistory)
	}
	writeDependencies(p, result.Workloads, limit)
	notes := append(unjudgedNotes(unjudged), result.Notes...)
	if len(notes) > 0 {
		p.line("Notes:")
		for _, n := range notes {
			p.line("  - %s", n)
		}
		p.line("")
	}
	if len(result.HistoryAccess) > 0 {
		p.line("To replay history, an admin can let you read Prometheus, and nothing else, with:")
		for _, command := range result.HistoryAccess {
			p.line("  %s", command)
		}
		p.line("Or pass --prometheus-url if Prometheus is reachable from your machine.")
		p.line("")
	}
	if result.Prices.CPUAssumed || result.Prices.MemoryAssumed {
		p.line("Costs use %s", lowerFirst(result.pricesSentence()))
		p.line("Pass --cpu-price and --memory-price for yours.")
		p.line("")
	}
	if result.Totals.Idle > 0 && result.Totals.Replayed.Workloads == 0 {
		p.line("What pausing would save depends on how often they'd be woken, which one moment can't show.")
		p.line("Labelling them dry-run measures it, below; scanning with Prometheus history estimates it.")
		p.line("")
	}
	writeNextSteps(p, result)
	p.line("See every cluster together, with savings checked against your cloud bill and kept as history:")
	p.line("Hybernate Hub, free for up to 2 clusters: %s", hubURL)
	return p.err
}

func writeHeadline(p *printer, t discovery.Totals, replayed string) {
	if t.Paused > 0 {
		p.line("  Hybernate has %s paused right now, freeing %s an hour.",
			countOf(t.Paused, "workload"), cents(t.PausedHourlyCost))
	}
	if t.Live > 0 {
		p.line("  Hybernate has saved %s this month pausing %s.", dollars(t.SavedThisMonth),
			countOf(t.Live, "live workload"))
	}
	if t.DryRun > 0 {
		p.line("  %s in dry-run: measured by Hybernate since starting, %s would have slept %s, freeing %s,",
			plural(t.DryRun, "workload is", "workloads are"), pronoun(t.DryRun),
			hoursTotal(t.Measured.SleptHours), cents(t.Measured.Freed))
		p.line("  about %s a month.", dollars(t.Measured.MonthlyFreed))
	}
	if t.ScaledToZero > 0 {
		them := "them"
		if t.ScaledToZero == 1 {
			them = "it"
		}
		p.line("  %s scaled to zero by hand; Hybernate can pause %s while idle and wake %s on the",
			plural(t.ScaledToZero, "workload is", "workloads are"), them, them)
		p.line("  next request instead.")
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
			p.line("  Replaying %s, no unmanaged workload would have slept.", replayed)
		} else {
			p.line("  Replaying %s, Hybernate would have paused %d of %s for %s in all,",
				replayed, r.Sleepers, countOf(r.Workloads, "unmanaged workload"), hoursTotal(r.SleepHours))
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

// emptySentence says why a scan found no workloads.
func emptySentence(result scanResult) string {
	if result.Namespaces == 0 {
		return "There were no namespaces to scan."
	}
	if len(result.Notes) > 0 {
		return "No Deployments or StatefulSets were found that the scan could read; the notes say what it couldn't."
	}
	return "No Deployments or StatefulSets were found."
}

var unmeasuredExplained = map[string]string{
	"no CPU requests":         "set no CPU requests, so their use can't be measured",
	"no metrics yet":          "have no metrics yet, usually because their pods just started",
	"no Metrics API":          "couldn't be measured without the Metrics API",
	"pod metrics not allowed": "couldn't be measured, as your access doesn't allow reading pod metrics",
	"pod metrics unreadable":  "couldn't be measured, as reading pod metrics failed",
	"invalid selector":        "have a pod selector the scan couldn't read, so their pods weren't found",
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
	saving, couldSave := savingColumns(workloads)
	sleep, slept := sleepColumns(history, workloads), sleptColumn(workloads)
	header := []string{"NAMESPACE", "WORKLOAD", "STATE", "BECAUSE", "COST/MO"}
	if !history {
		header = append(header, "COST/HOUR")
	}
	if slept {
		header = append(header, "SLEPT THIS MONTH")
	}
	if saving {
		header = append(header, "SAVED THIS MONTH")
	}
	if sleep {
		header = append(header, "COULD SLEEP", "WAKES")
	}
	if couldSave {
		header = append(header, "COULD SAVE/MO")
	}
	tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  "+strings.Join(header, "\t"))
	for _, wl := range shown {
		couldSleep, wakes, since, _ := sleepCells(wl)
		evidence := because(wl)
		if since != "" {
			evidence = strings.TrimPrefix(evidence+"; measuring "+since, "; ")
		}
		row := []string{wl.Namespace, strings.ToLower(string(wl.Kind)) + "/" + wl.Name,
			fmt.Sprintf("%s (%s)", wl.State, management(wl)), evidence, dollars(wl.MonthlyCost)}
		if !history {
			row = append(row, cents(wl.HourlyCost))
		}
		if slept {
			hoursSlept, _, _, _ := sleptCells(wl)
			row = append(row, hoursSlept)
		}
		if saving {
			row = append(row, savedCell(wl))
		}
		if sleep {
			row = append(row, couldSleep, wakes)
		}
		if couldSave {
			row = append(row, couldSaveCell(wl))
		}
		_, _ = fmt.Fprintln(tw, "  "+strings.Join(row, "\t"))
	}
	_ = tw.Flush()
	if len(shown) < len(workloads) {
		p.line("  ...and %d more; --limit 0 lists them all", len(workloads)-len(shown))
	}
	p.line("")
}

// savingColumns says which money columns a table needs: what Hybernate has
// saved, for live workloads, and what pausing could save, for those it
// doesn't pause yet, when the scan knows either for any of them.
// sleptColumn says whether the scan knows how long Hybernate has had any
// live workload paused.
func sleptColumn(workloads []Workload) bool {
	for _, wl := range workloads {
		if wl.Slept != nil {
			return true
		}
	}
	return false
}

// sleptCells are how long Hybernate has had a live workload paused this
// month, how many times it was woken, and since when.
func sleptCells(wl Workload) (slept, wakes, since string, sleptSort float64) {
	s := wl.Slept
	if s == nil || !wl.Managed || wl.DryRun {
		return "-", "-", "", -1
	}
	return hours(s.Hours), strconv.Itoa(s.Wakes), "since " + s.Since.Format("Jan 2"), s.Hours
}

func savingColumns(workloads []Workload) (saving, couldSave bool) {
	for _, wl := range workloads {
		saving = saving || (wl.Managed && !wl.DryRun)
		couldSave = couldSave || wl.Measured != nil || (!wl.Managed && wl.History != nil)
	}
	return saving, couldSave
}

// savedCell is what Hybernate has saved this month pausing a live workload.
func savedCell(wl Workload) string {
	if !wl.Managed || wl.DryRun {
		return "-"
	}
	return dollars(wl.SavedThisMonth)
}

// couldSaveCell is what pausing a workload Hybernate doesn't pause yet
// would free a month, from what dry-run measured or history shows.
func couldSaveCell(wl Workload) string {
	if wl.ScaledByHand || (wl.Measured == nil && (wl.Managed || wl.Protected || wl.History == nil)) {
		return "-"
	}
	return dollars(discovery.CouldSave(wl))
}

// nextStepTargets are the workloads next steps are about: the first idle
// one Hybernate could manage, to measure, and the first already measuring
// in dry-run, to go live. Workloads come sorted most savings first, so each
// is the best example. Protected workloads are left out, as Hybernate won't
// manage them.
func nextStepTargets(workloads []Workload) (idle, measuring *Workload) {
	for i := range workloads {
		wl := &workloads[i]
		if wl.Protected {
			continue
		}
		if idle == nil && wl.State == discovery.StateIdle && !wl.Managed {
			idle = wl
		}
		if measuring == nil && wl.Measured != nil {
			measuring = wl
		}
	}
	return idle, measuring
}

// nextStepCommands are the commands that measure the idle workload and then
// take a workload live, which both the table and the report show.
func nextStepCommands(idle, measuring *Workload) (measure []string, enable string) {
	if idle != nil {
		kind := strings.ToLower(string(idle.Kind))
		measure = []string{
			fmt.Sprintf("kubectl label %s %s -n %s hybernate.io/managed=true", kind, idle.Name, idle.Namespace),
			fmt.Sprintf("kubectl annotate %s %s -n %s hybernate.io/dry-run=true", kind, idle.Name, idle.Namespace),
		}
	}
	if measuring == nil {
		measuring = idle
	}
	if measuring != nil {
		enable = fmt.Sprintf("kubectl hybernate enable %s/%s -n %s",
			strings.ToLower(string(measuring.Kind)), measuring.Name, measuring.Namespace)
	}
	return measure, enable
}

func writeNextSteps(p *printer, result scanResult) {
	idle, measuring := nextStepTargets(result.Workloads)
	if idle == nil && measuring == nil {
		return
	}
	installed := slices.ContainsFunc(result.Workloads, func(wl Workload) bool { return wl.Managed })
	measure, enable := nextStepCommands(idle, measuring)
	p.line("Next steps:")
	step := 0
	next := func(title string) {
		step++
		p.line("  %d. %s", step, title)
	}
	if len(measure) > 0 {
		// Managed workloads show Hybernate is installed; without any, it
		// may or may not be, so the install step is shown.
		if !installed {
			next("Install Hybernate in the cluster:")
			p.line("       helm install hybernate oci://ghcr.io/okedeji/charts/hybernate -n hybernate-system --create-namespace")
		}
		next("Measure a workload first; nothing is paused in dry-run:")
		for _, command := range measure {
			p.line("       %s", command)
		}
	}
	if measuring != nil {
		next("When you're happy with what dry-run measured, start pausing:")
	} else {
		next("When you're happy with what it measures, start pausing it while idle:")
	}
	p.line("       %s", enable)
	p.line("")
}

// because is the evidence for a workload's state, or, for one in dry-run,
// what Hybernate measured: that's from its own clock, which sees more than
// the scan can, so it's what to judge going live by.
func because(wl Workload) string {
	var parts []string
	for _, p := range append([]string{wl.Reason}, wl.Clues...) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// sleepCells are how long a workload could have slept and how many times
// it would have been woken: what Hybernate measured, for one in dry-run,
// with the period it covers; what history shows, for one it doesn't
// manage. A live workload has neither yet.
func sleepCells(wl Workload) (slept, wakes, since string, sleptSort float64) {
	switch {
	case wl.Measured != nil:
		m := wl.Measured
		return hours(m.SleptHours), strconv.Itoa(m.Wakes), "since " + m.Since.Format("Jan 2"), m.SleptHours
	case !wl.Managed && wl.History != nil:
		h := wl.History
		return hours(h.SleepHours), strconv.Itoa(h.Wakes), "", h.SleepHours
	}
	return "-", "-", "", -1
}

// sleepColumns says whether the scan knows how long any workload could
// have slept: from history, or from a dry-run measurement.
func sleepColumns(history bool, workloads []Workload) bool {
	for _, wl := range workloads {
		if wl.Measured != nil {
			return true
		}
	}
	return history
}

// dependencyLine is one dependency a workload was found to have.
type dependencyLine struct {
	wl Workload
	d  discovery.Dependency
}

// dependencyLines are the dependencies found, by workload.
func dependencyLines(workloads []Workload) []dependencyLine {
	n := 0
	for _, wl := range workloads {
		n += len(wl.Dependencies)
	}
	lines := make([]dependencyLine, 0, n)
	for _, wl := range workloads {
		for _, d := range wl.Dependencies {
			lines = append(lines, dependencyLine{wl: wl, d: d})
		}
	}
	return lines
}

// dependencyStatus says whether Hybernate wakes and holds a dependency with
// the workload that needs it.
func dependencyStatus(l dependencyLine) string {
	switch {
	case l.d.Declared:
		return "declared"
	case l.d.Connected:
		return "connected by Hybernate"
	case !l.wl.Managed:
		return "connected once Hybernate manages it"
	default:
		return "not connected yet"
	}
}

// foundVia says where a dependency was found.
func foundVia(d discovery.Dependency) string {
	if d.Source == discovery.SourceWake {
		return "learned from a wake"
	}
	return d.Via
}

// writeDependencies lists the dependencies found, and whether Hybernate
// wakes and holds each with the workload that needs it.
func writeDependencies(p *printer, workloads []Workload, limit int) {
	lines := dependencyLines(workloads)
	if len(lines) == 0 {
		return
	}
	p.line("  Dependencies Hybernate wakes and holds with the workloads that need them: %s",
		plural(len(lines), "dependency", "dependencies"))
	shown := lines
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
	for _, l := range shown {
		_, _ = fmt.Fprintf(tw, "  %s\t%s/%s\t->\t%s\t%s\t%s\n", l.wl.Namespace, strings.ToLower(string(l.wl.Kind)),
			l.wl.Name, dependencyRef(l.wl.Namespace, l.d), foundVia(l.d), dependencyStatus(l))
	}
	_ = tw.Flush()
	if len(shown) < len(lines) {
		p.line("  ...and %d more; --limit 0 lists them all", len(lines)-len(shown))
	}
	p.line("")
}

// dependencyRef writes a dependency as hybernate.io/depends-on takes it:
// kind/name, with its namespace first when it's in another one.
func dependencyRef(namespace string, d discovery.Dependency) string {
	ref := strings.ToLower(string(d.Kind)) + "/" + d.Name
	if d.Namespace != namespace {
		ref = d.Namespace + "/" + ref
	}
	return ref
}

// hours is a length of time to the nearest hour, or minute under one,
// rounded as hoursTotal rounds so a row and its total agree.
func hours(h float64) string {
	if h > 0 && h < 1 {
		return fmt.Sprintf("%dm", int(h*60+0.5))
	}
	return fmt.Sprintf("%dh", int(h+0.5))
}

// clusterName is how a kubeconfig context is shown. EKS and GKE contexts
// are long generated identifiers, so they're shortened to the cluster with
// its provider and region.
func clusterName(contextName string) string {
	if contextName == "" {
		return "current context"
	}
	return shortClusterName(contextName)
}

// shortClusterName recognises the context names EKS and GKE generate and
// leaves anything else as it is.
//
//	arn:aws:eks:us-east-1:123456789012:cluster/staging -> staging (EKS us-east-1)
//	gke_myproject_europe-west1_dev                     -> dev (GKE europe-west1)
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

// management says how Hybernate is involved with a workload: not at all,
// measuring it in dry-run, or pausing it live.
func management(wl Workload) string {
	switch {
	case wl.Protected && !wl.Managed:
		return "protected"
	case !wl.Managed:
		return "unmanaged"
	case wl.DryRun:
		return "dry-run"
	default:
		return "live"
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
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

// dollars formats whole dollars with thousands separators, a negative
// amount with its sign before the dollar sign.
func dollars(amount float64) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	s := strconv.FormatInt(int64(math.Round(amount)), 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if s == "0" {
		sign = ""
	}
	return sign + "$" + s
}
