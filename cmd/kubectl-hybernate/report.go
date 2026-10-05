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
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/okedeji/hybernate/internal/discovery"
	"github.com/okedeji/hybernate/internal/doorman"
)

//go:embed report.html
var reportHTML string

var reportTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"hours": hoursTotal,
	"cents": cents,
}).Parse(reportHTML))

// reportPage is what the HTML report shows, worded from the same report
// data and helpers as the terminal table, so the two never disagree.
type reportPage struct {
	Cluster      string
	ScannedAt    string
	Basis        string
	Prices       string
	Headline     headline
	Facts        []fact
	History      bool
	Sleep        bool
	Slept        bool
	Saving       bool
	CouldSave    bool
	Namespaces   []namespaceRow
	Workloads    []workloadRow
	Dependencies []dependencyRow
	Notes        []string
	Method       []string
	NextSteps    []template.HTML
	Threshold    int
	IdleAfter    string
	Over         string
	Tips         tips
	Terms        []term
	Mark         string
	HubURL       string
}

type headline struct {
	Figures []figure
}

// tips explain the money and sleep columns, on their headers and in the
// method section, where printing keeps them.
type tips struct {
	CouldSleep, Slept, Saved, CouldSave string
}

type term struct {
	Name, Meaning string
}

func tipsFor(history bool, over string) tips {
	couldSleep := "Hours Hybernate would have had it paused, measured since dry-run started for a dry-run workload"
	if history {
		couldSleep += ", or from history over " + over + " for an unmanaged one"
	}
	return tips{
		CouldSleep: couldSleep + ". Under it, how many times activity would have woken it, each a wait for someone.",
		Slept: "For live workloads: hours Hybernate has had it paused this month, and how many times activity " +
			"woke it.",
		Saved: "For live workloads: what Hybernate has freed pausing it this month, priced at the rates it uses " +
			"for the workload.",
		CouldSave: "What pausing would free a month: measured by Hybernate for a dry-run workload, estimated from " +
			"history for an unmanaged one.",
	}
}

// termsFor lists the column definitions for the columns the page shows.
func termsFor(page reportPage) []term {
	var terms []term
	if page.Slept {
		terms = append(terms, term{"Slept", page.Tips.Slept})
	}
	if page.Saving {
		terms = append(terms, term{"Saved", page.Tips.Saved})
	}
	if page.Sleep {
		terms = append(terms, term{"Could sleep", page.Tips.CouldSleep})
	}
	if page.CouldSave {
		terms = append(terms, term{"Could save", page.Tips.CouldSave})
	}
	return terms
}

type figure struct {
	Value, Label, Detail string
}

type fact struct {
	Value, Label string
}

type namespaceRow struct {
	Namespace                          string
	Workloads, Idle                    int
	Cost, Saved, CouldSave             string
	CostSort, SavedSort, CouldSaveSort float64
}

type workloadRow struct {
	Namespace, Workload                        string
	State, StateClass                          string
	Cost                                       string
	CostSort                                   float64
	CouldSleep, Wakes, Since, Saved, CouldSave string
	Slept, SleptWakes, SleptSince              string
	SleptSort                                  float64
	CouldSleepSort, SavedSort, CouldSaveSort   float64
	Because                                    string
}

type dependencyRow struct {
	Workload, Target, Found, Status string
}

// browse opens a file in the user's browser, and interactive says whether
// output goes to a terminal with a desktop to open one on. They're
// variables so tests can stand in for a terminal and a browser.
var (
	browse      = openInBrowser
	interactive = func(w io.Writer) bool { return isTerminal(w) && canBrowse() }
)

// writeReport writes the HTML report where asked, or, when the table is
// shown in a terminal, to a temporary file it opens in the browser. Output
// for scripts, or a session with nowhere to show a browser, gets no report
// unless one is asked for.
func writeReport(stdout, stderr io.Writer, result scanResult, opts scanOptions) error {
	open := opts.open && opts.output == outputTable && interactive(stdout)
	if opts.html == "" && !open {
		return nil
	}
	path, err := writeHTMLFile(opts.html, result, opts.idleAfter)
	if err != nil {
		return err
	}
	if !open {
		_, _ = fmt.Fprintf(stderr, "Report: %s\n", path)
		return nil
	}
	if err := browse(path); err != nil {
		_, _ = fmt.Fprintf(stderr, "Report: %s (couldn't open a browser: %v)\n", path, err)
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "Report: %s (opened in your browser; --open=false to skip)\n", path)
	return nil
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// canBrowse reports whether there's a desktop to open a browser on, which
// an SSH session or a container on Linux doesn't have.
func canBrowse() bool {
	if runtime.GOOS != "linux" {
		return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// browserTimeout bounds the opener, which hands the file to the browser and
// returns at once; one still running after this is stuck.
const browserTimeout = 10 * time.Second

func openInBrowser(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), browserTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", path)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", path)
	}
	return cmd.Run()
}

// writeHTMLFile writes the report to path, or, without one, to a new
// temporary file only the user can read, since the report names the
// cluster's workloads. It returns where it wrote.
func writeHTMLFile(path string, result scanResult, idleAfter time.Duration) (string, error) {
	var f *os.File
	var err error
	if path == "" {
		f, err = os.CreateTemp("", "hybernate-scan-*.html")
	} else {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	}
	if err != nil {
		return "", fmt.Errorf("creating the report: %w", err)
	}
	if err := writeHTML(f, result, idleAfter); err != nil {
		_ = f.Close() // the write error is the one to report
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

func writeHTML(w io.Writer, result scanResult, idleAfter time.Duration) error {
	if err := reportTemplate.Execute(w, buildReport(result, idleAfter)); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

func buildReport(result scanResult, idleAfter time.Duration) reportPage {
	history := result.Mode == discovery.ModeHistory
	page := reportPage{
		Cluster:   result.Cluster,
		ScannedAt: result.ScannedAt.Format("2 January 2006, 15:04 MST"),
		Basis:     basisSentence(result),
		Prices:    result.pricesSentence(),
		History:   history,
		Threshold: result.Settings.CPUThreshold,
		IdleAfter: roundedDuration(idleAfter),
		Over:      replayedOver(result.History),
		Mark:      doorman.Mark,
		HubURL:    hubURL,
	}
	page.Headline, page.Facts = headlineFor(result.Totals, replayedOver(result.History))
	page.Saving, page.CouldSave = savingColumns(result.Workloads)
	page.Sleep = sleepColumns(history, result.Workloads)
	page.Slept = sleptColumn(result.Workloads)
	page.Tips = tipsFor(history, page.Over)
	page.Terms = termsFor(page)

	byNamespace := map[string]*namespaceRow{}
	for _, wl := range result.Workloads {
		if wl.State != discovery.StateUnknown {
			page.Workloads = append(page.Workloads, workloadRowFor(wl))
		}
		ns := byNamespace[wl.Namespace]
		if ns == nil {
			ns = &namespaceRow{Namespace: wl.Namespace}
			byNamespace[wl.Namespace] = ns
		}
		ns.Workloads++
		if wl.State == discovery.StateIdle {
			ns.Idle++
		}
		if wl.State != discovery.StatePaused {
			ns.CostSort += wl.MonthlyCost
		}
		ns.SavedSort += wl.SavedThisMonth
		ns.CouldSaveSort += discovery.CouldSave(wl)
	}
	for _, ns := range byNamespace {
		ns.Cost, ns.Saved, ns.CouldSave = dollars(ns.CostSort), dollars(ns.SavedSort), dollars(ns.CouldSaveSort)
		page.Namespaces = append(page.Namespaces, *ns)
	}
	slices.SortFunc(page.Namespaces, func(a, b namespaceRow) int {
		if c := cmp.Compare(b.CouldSaveSort, a.CouldSaveSort); c != 0 {
			return c
		}
		if c := cmp.Compare(b.CostSort, a.CostSort); c != 0 {
			return c
		}
		return strings.Compare(a.Namespace, b.Namespace)
	})

	for _, l := range dependencyLines(result.Workloads) {
		page.Dependencies = append(page.Dependencies, dependencyRow{Workload: workloadRef(l.wl),
			Target: dependencyRef(l.wl.Namespace, l.d), Found: foundVia(l.d), Status: dependencyStatus(l)})
	}

	_, unjudged := splitJudged(result.Workloads)
	page.Notes = append(unjudgedNotes(unjudged), result.Notes...)
	if n := result.Totals.ScaledToZero; n > 0 {
		page.Notes = append([]string{plural(n, "workload is", "workloads are") + " scaled to zero by hand; Hybernate " +
			"can pause them while idle and wake them on the next request instead"}, page.Notes...)
	}
	page.Method = methodFor(result, history, len(page.Dependencies) > 0, idleAfter)
	for _, step := range nextStepsFor(result.Workloads) {
		page.NextSteps = append(page.NextSteps, shellHTML(step))
	}
	return page
}

func workloadRef(wl Workload) string {
	return fmt.Sprintf("%s/%s/%s", wl.Namespace, strings.ToLower(string(wl.Kind)), wl.Name)
}

func basisSentence(result scanResult) string {
	if over := replayedOver(result.History); result.Mode == discovery.ModeHistory && over != "" {
		return fmt.Sprintf("Based on %s of Prometheus history, and CPU at the time of the scan.", over)
	}
	return "A snapshot of CPU at the time of the scan, without history."
}

// headlineFor leads with two figures: what Hybernate has saved this month,
// and what pausing could save a month, measured for dry-run workloads and
// estimated from history for unmanaged ones. Without either, the second is
// what idle workloads cost while running, since one moment can't show a
// saving. Four facts follow: the scale, the waste right now, how many
// workloads there are to act on, and how far Hybernate's rollout has got.
func headlineFor(t discovery.Totals, over string) (headline, []fact) {
	saved := figure{Value: dollars(t.SavedThisMonth), Label: "saved by Hybernate this month",
		Detail: "nothing is live yet"}
	if t.Live > 0 {
		saved.Detail = "so far, pausing " + countOf(t.Live, "live workload")
	}
	h := headline{Figures: []figure{saved}}

	r := t.Replayed
	switch {
	case r.Sleepers > 0 || t.DryRun > 0:
		could := r.MonthlyFreed + t.Measured.MonthlyFreed
		f := figure{Value: dollars(could), Label: "could be saved a month"}
		if t.MonthlyCost > 0 {
			f.Detail = fmt.Sprintf("%d%% of what these workloads cost", int(could/t.MonthlyCost*100+0.5))
		}
		h.Figures = append(h.Figures, f)
	case t.Idle > 0:
		h.Figures = append(h.Figures, figure{Value: dollars(t.IdleMonthlyCost),
			Label: "a month, what idle workloads cost while running", Detail: cents(t.IdleHourlyCost) + " an hour"})
	}

	idle := fact{Value: strconv.Itoa(t.Idle), Label: "idle right now"}
	if t.Idle > 0 {
		idle.Label = fmt.Sprintf("idle right now, reserving %s and %s of memory",
			vcpu(t.IdleCPUMillis), gib(t.IdleMemoryBytes))
	}
	facts := []fact{
		{Value: dollars(t.MonthlyCost), Label: "a month, what these workloads cost while running"},
		idle,
	}
	if r.Workloads > 0 {
		facts = append(facts, fact{Value: fmt.Sprintf("%d of %d", r.Sleepers, r.Workloads),
			Label: fmt.Sprintf("unmanaged workloads would have slept over %s", over)})
	} else {
		facts = append(facts, fact{Value: cents(t.IdleHourlyCost), Label: "an hour, what idle workloads cost while running"})
	}
	facts = append(facts, fact{Value: fmt.Sprintf("%d of %d", t.Live+t.DryRun, t.Workloads),
		Label: fmt.Sprintf("managed by Hybernate: %d live, %d in dry-run", t.Live, t.DryRun)})
	return h, facts
}

func workloadRowFor(wl Workload) workloadRow {
	row := workloadRow{
		Namespace:  wl.Namespace,
		Workload:   strings.ToLower(string(wl.Kind)) + "/" + wl.Name,
		State:      string(wl.State),
		StateClass: string(wl.State),
		Cost:       dollars(wl.MonthlyCost),
		CostSort:   wl.MonthlyCost,
		Saved:      savedCell(wl), SavedSort: -1,
		CouldSave: couldSaveCell(wl), CouldSaveSort: -1,
		Because: because(wl),
	}
	row.State += " (" + management(wl) + ")"
	row.CouldSleep, row.Wakes, row.Since, row.CouldSleepSort = sleepCells(wl)
	row.Slept, row.SleptWakes, row.SleptSince, row.SleptSort = sleptCells(wl)
	if row.Saved != "-" {
		row.SavedSort = wl.SavedThisMonth
	}
	if row.CouldSave != "-" {
		row.CouldSaveSort = discovery.CouldSave(wl)
	}
	return row
}

func methodFor(result scanResult, history, dependencies bool, idleAfter time.Duration) []string {
	method := []string{
		"Costs are what each workload's pods request, sidecars included, at the prices above. They're list " +
			"prices for the capacity reserved, not your bill.",
		"An hour asleep frees that capacity. It becomes money when your cluster autoscaler removes the nodes " +
			"it no longer needs.",
		fmt.Sprintf("A workload is active when its CPU is at least %d%% of what it requests (the scan's "+
			"--cpu-threshold), it was deployed, or an activity annotation says so. It's idle once it has had no "+
			"activity for %s (the scan's --idle-after), which is when Hybernate would pause it. Workloads Hybernate "+
			"manages are judged by the activity clock it keeps for them, with their own CPU threshold and "+
			"idle-after.",
			result.Settings.CPUThreshold, roundedDuration(idleAfter)),
	}
	if history {
		method = append(method, fmt.Sprintf("The history replay applies Hybernate's rules to recorded CPU and "+
			"rollouts: after its idle-after with no activity (%s, or a managed workload's own), a workload would "+
			"be paused until the next activity, which wakes it. "+
			"Requests and activity annotations aren't recorded in Prometheus, so a workload used with little CPU "+
			"can look like it would sleep more than it would. Requests and prices are today's. The total counts "+
			"only workloads Hybernate doesn't pause yet.", roundedDuration(idleAfter)))
	} else {
		method = append(method, "Without history, the scan sees CPU at one moment, which can't show how often a "+
			"workload would be woken, so this report doesn't estimate a saving. Scanning a cluster with "+
			"Prometheus, or measuring with dry-run, does.")
	}
	if dependencies {
		method = append(method, "Dependencies are found in workloads' environment variables, literal and from "+
			"ConfigMaps, matched to the cluster's Services. Secrets aren't read, and addresses are left out of "+
			"this report.")
	}
	return method
}

func nextStepsFor(workloads []Workload) []string {
	var idle, measuring *Workload
	for i := range workloads {
		wl := &workloads[i]
		if idle == nil && wl.State == discovery.StateIdle && !wl.Managed {
			idle = wl
		}
		if measuring == nil && wl.Measured != nil {
			measuring = wl
		}
	}
	var steps []string
	if idle != nil {
		kind := strings.ToLower(string(idle.Kind))
		steps = append(steps,
			fmt.Sprintf("kubectl label %s %s -n %s hybernate.io/managed=true", kind, idle.Name, idle.Namespace),
			fmt.Sprintf("kubectl annotate %s %s -n %s hybernate.io/dry-run=true", kind, idle.Name, idle.Namespace))
	}
	if measuring == nil {
		measuring = idle
	}
	if measuring != nil {
		steps = append(steps, fmt.Sprintf("kubectl hybernate enable %s/%s -n %s",
			strings.ToLower(string(measuring.Kind)), measuring.Name, measuring.Namespace))
	}
	return steps
}

// shellHTML colours a command the way a terminal would: the program and
// its subcommands, flags, and the key and value of each key=value.
func shellHTML(command string) template.HTML {
	var b strings.Builder
	b.WriteString(`<span class="line">`)
	for i, word := range strings.Fields(command) {
		if i > 0 {
			b.WriteByte(' ')
		}
		esc := template.HTMLEscapeString(word)
		key, value, isPair := strings.Cut(word, "=")
		switch {
		case i == 0:
			fmt.Fprintf(&b, `<span class="sh-cmd">%s</span>`, esc)
		case strings.HasPrefix(word, "-"):
			fmt.Fprintf(&b, `<span class="sh-flag">%s</span>`, esc)
		case isPair:
			fmt.Fprintf(&b, `<span class="sh-key">%s</span>=<span class="sh-val">%s</span>`,
				template.HTMLEscapeString(key), template.HTMLEscapeString(value))
		default:
			b.WriteString(esc)
		}
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String()) //nolint:gosec // every part is escaped above
}

// roundedDuration writes d to the second without zero units: "1h", "90m"
// as "1h30m", "10s", "2h0m5s" as "2h5s".
func roundedDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d == 0 {
		return "0s"
	}
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	var b strings.Builder
	b.WriteString(sign)
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / unit.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, unit.name)
			d -= n * unit.size
		}
	}
	return b.String()
}
