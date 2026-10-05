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

package discovery

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

// State is how a scan judges a workload's use.
type State string

const (
	StateIdle   State = "idle"
	StateActive State = "active"
	// StatePaused means Hybernate has it paused right now.
	StatePaused State = "paused"
	// StateUnknown means nothing showed activity and its CPU couldn't be
	// measured: no metrics, or no CPU requests to measure against.
	StateUnknown State = "unknown"
)

// Mode says what a scan's judgement of use is based on.
type Mode string

const (
	// ModeSnapshot judges from CPU at the moment of the scan, which can't
	// tell a quiet hour from a quiet week.
	ModeSnapshot Mode = "snapshot"
	// ModeHistory also replays the activity clock over CPU history from
	// Prometheus, which shows how much a workload would have slept.
	ModeHistory Mode = "history"
)

// staleAfter is how long without a rollout is worth pointing out.
const staleAfter = 7 * 24 * time.Hour

// defaultIdleAfter matches the activity clock's default: activity this
// recent keeps a workload awake.
const defaultIdleAfter = time.Hour

// defaultCPUThreshold matches the activity clock's default CPU threshold.
const defaultCPUThreshold = 10

// ClusterReport is what a scan found in one cluster.
type ClusterReport struct {
	Mode Mode `json:"mode"`
	// History says what the replay read, in ModeHistory.
	History *HistorySource `json:"history,omitempty"`
	// Namespaces is how many namespaces were scanned.
	Namespaces int        `json:"namespaces"`
	Workloads  []Workload `json:"workloads"`
	// NodePrices are the node types the workloads were priced at.
	NodePrices Prices `json:"nodePrices"`
	// Notes say what the scan couldn't see and how that limits it.
	Notes []string `json:"notes,omitempty"`
	// Incomplete are the namespaces the scan couldn't read all it should
	// have of, by why; nil when it read them all.
	Incomplete *Incomplete `json:"incomplete,omitempty"`
	Totals     Totals      `json:"totals"`
}

// Incomplete are the namespaces a scan couldn't read all it should have
// of. Their workloads are missing or judged on less than they should be.
type Incomplete struct {
	// Missing were named to scan but don't exist.
	Missing []string `json:"missing,omitempty"`
	// Denied were named to scan, but the user's access doesn't allow
	// reading their workloads.
	Denied []string `json:"denied,omitempty"`
	// Failed couldn't be read for another reason, such as a timeout.
	Failed []string `json:"failed,omitempty"`
}

// Workload is one Deployment or StatefulSet as a scan sees it.
type Workload struct {
	Namespace string              `json:"namespace"`
	Kind      v1alpha1.TargetKind `json:"kind"`
	Name      string              `json:"name"`
	Replicas  int32               `json:"replicas"`
	State     State               `json:"state"`
	// CPUPercent is CPU use as a share of the workload's own requests,
	// the measure the activity clock uses, and CPUMillisUsed the use itself.
	// Nil and zero when it couldn't be measured.
	CPUPercent    *int  `json:"cpuPercent,omitempty"`
	CPUMillisUsed int64 `json:"cpuMillisUsed,omitempty"`
	// PodCPURequestMillis and PodMemoryRequestBytes are what one replica's
	// pod reserves, sidecars included: what pausing a replica frees.
	PodCPURequestMillis   int64      `json:"podCPURequestMillis"`
	PodMemoryRequestBytes int64      `json:"podMemoryRequestBytes"`
	LastDeployed          *time.Time `json:"lastDeployed,omitempty"`
	// Reason is the evidence for State, e.g. "CPU 64% of its request" or "deployed 20m ago".
	Reason string `json:"reason,omitempty"`
	// Unmeasured says why CPU couldn't be measured.
	Unmeasured string `json:"unmeasured,omitempty"`
	// DryRun means Hybernate measures it but never pauses it.
	DryRun bool `json:"dryRun,omitempty"`
	// ScaledByHand means it's at zero replicas, put there by something
	// other than Hybernate, so nothing will wake it on a request.
	ScaledByHand bool `json:"scaledByHand,omitempty"`
	// Measured is what dry-run has measured, for a workload in dry-run.
	Measured *Measured `json:"measured,omitempty"`
	// SavedThisMonth is what Hybernate has saved this month pausing a live
	// workload, priced at the rates it uses for the workload.
	SavedThisMonth float64 `json:"savedThisMonth,omitempty"`
	// Slept is how long Hybernate has had a live workload paused, and how
	// many times it was woken, since Since: the start of the month, or when
	// it went live if later. Hybernate doesn't record these yet, so they
	// stay unset until it does.
	Slept *Slept `json:"slept,omitempty"`
	// Protected means its namespace is labelled protected, so Hybernate
	// won't manage it whatever its labels.
	Protected bool `json:"protected,omitempty"`
	// ReplicasFromGit is the GitOps tool that last set its replicas from
	// Git, which would undo every pause until it's told to leave them.
	ReplicasFromGit string `json:"replicasFromGit,omitempty"`
	// OnSpot means a pod of it is on a spot node, priced at on-demand, so it
	// costs less than shown.
	OnSpot bool `json:"onSpot,omitempty"`
	// rates are what it's priced at.
	rates cost.Rates
	// History is what replaying the activity clock over its recorded CPU
	// found, in ModeHistory.
	History *History `json:"history,omitempty"`
	// Dependencies are the workloads its environment points at.
	Dependencies []Dependency `json:"dependencies,omitempty"`
	// Clues are facts beyond CPU that bear on whether it's in use.
	Clues   []string `json:"clues,omitempty"`
	Managed bool     `json:"managed"`
	// HourlyCost and MonthlyCost are what the workload's requests cost at
	// list prices while it runs. An hour asleep frees HourlyCost, which
	// becomes money only if the autoscaler removes that capacity.
	HourlyCost  float64 `json:"hourlyCost"`
	MonthlyCost float64 `json:"monthlyCost"`
}

// Measured is what Hybernate would have done to a workload in dry-run.
type Measured struct {
	Since  time.Time `json:"since"`
	Pauses int       `json:"pauses"`
	// Wakes is how many of those pauses activity would have ended, which
	// is all of them but one still under way.
	Wakes int `json:"wakes"`
	// SleptHours is how long its would-be pauses lasted, one under way
	// included.
	SleptHours float64 `json:"sleptHours"`
	// Freed is what those hours would have freed, at the scan's prices and
	// the replicas it runs now, and MonthlyFreed that over an average month
	// at the rate measured so far.
	Freed        float64 `json:"freed"`
	MonthlyFreed float64 `json:"monthlyFreed"`
}

// Slept is what Hybernate has done pausing a live workload.
type Slept struct {
	Since time.Time `json:"since"`
	Hours float64   `json:"hours"`
	Wakes int       `json:"wakes"`
}

// Totals sum a scan's workloads.
type Totals struct {
	Workloads   int     `json:"workloads"`
	MonthlyCost float64 `json:"monthlyCost"`
	// SavingsBasis is what the workloads in MonthlyCost cost a month on the
	// basis their saving is estimated on: over the replayed history for
	// those replayed, with the pods they ran then, and as they run now for
	// the rest. What Replayed and Measured could save is a share of it.
	SavingsBasis float64 `json:"savingsBasis"`
	// Paused and PausedHourlyCost are what Hybernate has paused right now
	// and what that frees each hour.
	Paused           int     `json:"paused"`
	PausedHourlyCost float64 `json:"pausedHourlyCost"`
	// ScaledToZero is how many are at zero replicas by hand.
	ScaledToZero int `json:"scaledToZero"`
	Idle         int `json:"idle"`
	// IdleCPUMillis and IdleMemoryBytes are what idle workloads reserve,
	// and IdleHourlyCost what each hour of it costs.
	IdleCPUMillis   int64   `json:"idleCPUMillis"`
	IdleMemoryBytes int64   `json:"idleMemoryBytes"`
	IdleHourlyCost  float64 `json:"idleHourlyCost"`
	IdleMonthlyCost float64 `json:"idleMonthlyCost"`
	// Replayed sums the history of unmanaged workloads, Measured what
	// Hybernate measured for those in dry-run, and SavedThisMonth what it
	// has saved this month pausing live ones: each from its own source.
	Replayed       ReplayTotals `json:"replayed"`
	Measured       Measured     `json:"measured"`
	DryRun         int          `json:"dryRun"`
	SavedThisMonth float64      `json:"savedThisMonth"`
	Live           int          `json:"live"`
}

// ReplayTotals sum what replaying the activity clock found.
type ReplayTotals struct {
	Workloads int `json:"workloads"`
	// Sleepers is how many of them would have slept at all.
	Sleepers     int     `json:"sleepers"`
	SleepHours   float64 `json:"sleepHours"`
	Wakes        int     `json:"wakes"`
	Freed        float64 `json:"freed"`
	MonthlyFreed float64 `json:"monthlyFreed"`
}

// ClusterOptions configures ScanCluster.
type ClusterOptions struct {
	Namespaces []string
	// CPUThreshold is the CPU use, as a percentage of requests, at which a
	// workload Hybernate doesn't manage counts as active; managed ones use
	// their own. Zero means the activity clock's default.
	CPUThreshold int
	// Rates price what node prices can't: pods on nodes the price table
	// doesn't have, or every workload when nodes can't be read.
	Rates cost.Rates
	// OwnCPUPrice and OwnMemoryPrice say the user gave Rates' CPU or memory
	// price, which then prices every workload, in place of node prices.
	OwnCPUPrice, OwnMemoryPrice bool
	Now                         func() time.Time
	// IdleAfter is how long without activity makes a workload idle, for
	// workloads Hybernate doesn't manage; managed ones use their own.
	// Zero means the activity clock's default.
	IdleAfter time.Duration
	// History, when set, is replayed over Window.
	History *Prometheus
	Window  time.Duration
	// NamedNamespaces says the user named the namespaces, so one their
	// access doesn't let the scan read is a failure to scan what was asked,
	// not a limit to note.
	NamedNamespaces bool
}

// ScanCluster scans every Deployment and StatefulSet in the namespaces and
// judges each from its CPU right now, plus clues that don't need history.
// With History, it also replays the activity clock over each workload's
// recorded CPU. It only reads, and carries on past what it can't read,
// saying so in the report's notes, and listing in Incomplete the namespaces
// it should have been able to read but couldn't.
func (s *Scanner) ScanCluster(ctx context.Context, opts ClusterOptions) (*ClusterReport, error) {
	report := &ClusterReport{Mode: ModeSnapshot, Namespaces: len(opts.Namespaces), Workloads: []Workload{}}
	if opts.History != nil {
		report.Mode = ModeHistory
		report.History = &HistorySource{Prometheus: opts.History.Source}
	}
	pricing, nodePrices := s.readNodePrices(ctx)
	report.NodePrices = nodePrices

	scans := make([]namespaceScan, len(opts.Namespaces))
	var g errgroup.Group
	g.SetLimit(namespaceWorkers)
	for i, namespace := range opts.Namespaces {
		g.Go(func() error {
			scans[i] = s.scanNamespace(ctx, namespace, pricing, opts)
			return nil
		})
	}
	_ = g.Wait() // each namespace's problems are in its scan; none is returned

	var since time.Time
	noHistory := make([]string, 0, len(scans))
	problems := make([]readProblem, 0, len(scans))
	metricsUnavailable := false
	sources := map[workloadKey]workloadSource{}
	services := map[string]map[string]corev1.Service{}
	for _, scan := range scans {
		report.Workloads = append(report.Workloads, scan.workloads...)
		problems = append(problems, scan.problems...)
		noHistory = append(noHistory, scan.noHistory...)
		metricsUnavailable = metricsUnavailable || scan.metricsUnavailable
		maps.Copy(sources, scan.sources)
		if scan.services != nil {
			services[scan.namespace] = scan.services
		}
		if !scan.historySince.IsZero() && (since.IsZero() || scan.historySince.Before(since)) {
			since = scan.historySince
		}
	}
	dependencyNotes, dependencyProblems := s.findDependencies(ctx, report.Workloads, sources, services)
	problems = append(problems, dependencyProblems...)

	if metricsUnavailable {
		report.Notes = append(report.Notes,
			"the Metrics API isn't available, so CPU couldn't be measured; is metrics-server installed?")
	}
	problemNotes, incomplete := problemNotes(problems, opts.NamedNamespaces)
	report.Notes = append(report.Notes, problemNotes...)
	report.Incomplete = incomplete
	if report.History != nil && !since.IsZero() {
		report.History.Since = since
		report.History.Hours = opts.Now().Sub(since).Hours()
		report.Notes = append(report.Notes, "the history replay sees CPU and rollouts only; requests and activity "+
			"annotations aren't in Prometheus, so a workload used with little CPU can look like it would sleep more than it would")
		if covered := opts.Now().Sub(since); covered < opts.Window-historyStep(opts.Window) {
			report.Notes = append(report.Notes, fmt.Sprintf("Prometheus keeps %s of history, so the replay covers that, not the %s asked for",
				duration(covered), duration(opts.Window)))
		}
	}
	if opts.History != nil {
		if note := opts.History.mixedSourcesNote(); note != "" {
			report.Notes = append(report.Notes, note)
		}
	}
	if len(noHistory) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%s no CPU history in Prometheus, so only their CPU right now is known: %s",
			plural(len(noHistory), "workload has", "workloads have"), listSome(noHistory)))
	}
	report.Notes = append(report.Notes, dependencyNotes...)
	report.Notes = append(report.Notes, gitOpsNotes(report.Workloads)...)

	slices.SortFunc(report.Workloads, func(a, b Workload) int {
		if c := cmp.Compare(stateOrder[a.State], stateOrder[b.State]); c != 0 {
			return c
		}
		if c := cmp.Compare(CouldSave(b), CouldSave(a)); c != 0 {
			return c
		}
		if c := cmp.Compare(b.MonthlyCost, a.MonthlyCost); c != 0 {
			return c
		}
		return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	report.Totals = totals(report.Workloads)
	return report, nil
}

// gitOpsNotes say which workloads have replicas set from Git, which their
// GitOps tool would set again after each pause, and where the fix is.
func gitOpsNotes(workloads []Workload) []string {
	byTool := map[string][]string{}
	for _, w := range workloads {
		if w.ReplicasFromGit != "" {
			byTool[w.ReplicasFromGit] = append(byTool[w.ReplicasFromGit], w.Namespace+"/"+w.Name)
		}
	}
	tools := slices.Sorted(maps.Keys(byTool))
	notes := make([]string, 0, len(tools))
	for _, tool := range tools {
		names := byTool[tool]
		notes = append(notes, fmt.Sprintf("%s set from Git by %s, which would undo every pause: %s. "+
			"Have %s leave the replicas to Hybernate first: %s", plural(len(names), "workload has its replicas",
			"workloads have their replicas"), tool, listSome(names), tool, gitOpsGuide))
	}
	return notes
}

const gitOpsGuide = "https://okedeji.io/hybernate/guides/gitops/"

// stateOrder lists idle workloads first, the ones worth acting on.
var stateOrder = map[State]int{StateIdle: 0, StatePaused: 1, StateActive: 2, StateUnknown: 3}

func pausedByHybernate(mw *v1alpha1.ManagedWorkload) bool {
	return mw != nil && mw.Status.Phase == v1alpha1.PhasePaused && mw.Status.Pause != nil
}

// measured reads the dry-run summary Hybernate keeps for the workload. The
// summary adds a would-be pause when it ends, so one under way is added here.
func measured(mw *v1alpha1.ManagedWorkload, hourlyCost float64, now time.Time) *Measured {
	if mw == nil || !mw.Spec.DryRun || mw.Status.DryRun == nil {
		return nil
	}
	d := mw.Status.DryRun
	slept := d.Slept.Duration
	wakes := int(d.Pauses)
	if mw.Status.Phase == v1alpha1.PhaseIdle && d.Resources != nil && mw.Status.LastTransitionTime != nil {
		slept += max(now.Sub(mw.Status.LastTransitionTime.Time), 0)
		wakes--
	}
	measuring := max(now.Sub(d.Since.Time), 0)
	// However the recorded times line up, what it would have freed is never
	// more than what it cost meanwhile.
	slept = min(slept, measuring)
	m := &Measured{
		Since:      d.Since.Time,
		Pauses:     int(d.Pauses),
		Wakes:      max(wakes, 0),
		SleptHours: slept.Hours(),
		Freed:      hourlyCost * slept.Hours(),
	}
	if measuring > 0 {
		m.MonthlyFreed = m.Freed / measuring.Hours() * hoursPerMonth
	}
	return m
}

// SavedThisMonth reads what Hybernate records it has saved this month
// pausing a live workload.
func SavedThisMonth(mw *v1alpha1.ManagedWorkload) float64 {
	if mw == nil || mw.Spec.DryRun || mw.Status.Cost == nil {
		return 0
	}
	saved, err := strconv.ParseFloat(strings.TrimPrefix(mw.Status.Cost.SavedThisMonth, "$"), 64)
	if err != nil {
		return 0
	}
	return saved
}

// judgePaused describes a workload Hybernate has paused, priced on what it
// had running before, so the scan shows what the pause frees.
func judgePaused(w *Workload, mw *v1alpha1.ManagedWorkload, now time.Time) {
	w.State = StatePaused
	w.Replicas = mw.Status.Pause.PreviousReplicas
	if r := mw.Status.Pause.Resources; r != nil && r.CPUMillis > 0 {
		w.PodCPURequestMillis, w.PodMemoryRequestBytes = r.CPUMillis, r.MemoryBytes
	}
	if at := mw.Status.Pause.PausedAt; at != nil {
		w.Reason = "paused " + ago(now.Sub(at.Time))
	}
}

// judgeManaged reads the activity clock Hybernate keeps for the workload,
// which already combines every activity source it's configured with.
func judgeManaged(w *Workload, mw *v1alpha1.ManagedWorkload, now time.Time, threshold int) {
	idleAfter := idleAfterFor(mw, ClusterOptions{})
	a := mw.Status.Activity
	since := now.Sub(a.LastActivityTime.Time)
	switch {
	case mw.Status.Phase == v1alpha1.PhaseResuming:
		w.State, w.Reason = StateActive, "waking"
	case since < idleAfter:
		w.State, w.Reason = StateActive, fmt.Sprintf("active %s (%s)", ago(since), a.LastActivitySource)
	case w.CPUPercent != nil && *w.CPUPercent >= threshold:
		w.State, w.Reason = StateActive, cpuReason(*w)
	default:
		w.State, w.Reason = StateIdle, "no activity for "+duration(since)
		if meta.IsStatusConditionTrue(mw.Status.Conditions, "HeldByDependents") {
			w.Reason += ", held awake by dependents"
		}
	}
}

// idleAfterFor is how long without activity makes a workload idle: its own
// setting when Hybernate manages it, otherwise the scan's.
func idleAfterFor(mw *v1alpha1.ManagedWorkload, opts ClusterOptions) time.Duration {
	if mw != nil {
		if p := mw.Spec.IdlePolicy; p != nil && p.IdleAfter != nil && p.IdleAfter.Duration > 0 {
			return p.IdleAfter.Duration
		}
		return defaultIdleAfter
	}
	if opts.IdleAfter > 0 {
		return opts.IdleAfter
	}
	return defaultIdleAfter
}

// thresholdFor is the CPU use, as a percentage of requests, at which a
// workload counts as active: its own setting when Hybernate manages it,
// otherwise the scan's.
func thresholdFor(mw *v1alpha1.ManagedWorkload, opts ClusterOptions) int {
	if mw != nil {
		if p := mw.Spec.IdlePolicy; p != nil && p.Activity != nil && p.Activity.CPUThreshold > 0 {
			return p.Activity.CPUThreshold
		}
		return defaultCPUThreshold
	}
	if opts.CPUThreshold > 0 {
		return opts.CPUThreshold
	}
	return defaultCPUThreshold
}

// judgeUnmanaged applies the activity clock's sources a scan can see
// without Hybernate: activity annotations, a recent rollout, and CPU.
func judgeUnmanaged(w *Workload, obj client.Object, now time.Time, threshold int, idleAfter time.Duration) {
	if w.CPUPercent != nil && *w.CPUPercent >= threshold {
		w.State, w.Reason = StateActive, cpuReason(*w)
		return
	}
	annotations := obj.GetAnnotations()
	if until, err := time.Parse(time.RFC3339, annotations[v1alpha1.AnnotationActiveUntil]); err == nil && until.After(now) {
		w.State, w.Reason = StateActive, "kept awake until "+until.UTC().Format("Jan 2 15:04 UTC")
		return
	}
	if last, err := time.Parse(time.RFC3339, annotations[v1alpha1.AnnotationLastActivity]); err == nil &&
		now.Sub(last) < idleAfter {
		w.State, w.Reason = StateActive, "activity annotation "+ago(now.Sub(last))
		return
	}
	if w.LastDeployed != nil && now.Sub(*w.LastDeployed) < idleAfter {
		w.State, w.Reason = StateActive, "deployed "+ago(now.Sub(*w.LastDeployed))
		return
	}
	if w.CPUPercent != nil {
		w.State, w.Reason = StateIdle, cpuReason(*w)
	}
}

// cpuReason says how much CPU a workload uses against what it requests, so
// a little use that rounds to 0% doesn't read as a broken measurement.
func cpuReason(w Workload) string {
	switch {
	case w.CPUMillisUsed == 0:
		return "no CPU use"
	case *w.CPUPercent == 0:
		return "CPU under 1% of its request"
	default:
		return fmt.Sprintf("CPU %d%% of its request", *w.CPUPercent)
	}
}

func ago(d time.Duration) string {
	return duration(d) + " ago"
}

// duration says how long, to the largest whole unit.
func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// listSome names the first few of a list, and how many more there are.
func listSome(names []string) string {
	const shown = 3
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func clues(w Workload, now time.Time) []string {
	var out []string
	if w.LastDeployed != nil {
		if age := now.Sub(*w.LastDeployed); age >= staleAfter {
			out = append(out, fmt.Sprintf("last deployed %d days ago", int(age.Hours()/24)))
		}
	}
	return out
}

func hourlyCost(w Workload, rates cost.Rates) float64 {
	cores := float64(w.PodCPURequestMillis) / 1000
	gib := float64(w.PodMemoryRequestBytes) / (1 << 30)
	return cost.ComputeHourly(cores, gib, rates) * float64(w.Replicas)
}

func totals(workloads []Workload) Totals {
	var t Totals
	for _, w := range workloads {
		t.Workloads++
		switch {
		case w.Measured != nil:
			t.DryRun++
			t.Measured.Pauses += w.Measured.Pauses
			t.Measured.SleptHours += w.Measured.SleptHours
			t.Measured.Freed += w.Measured.Freed
			t.Measured.MonthlyFreed += w.Measured.MonthlyFreed
		case w.Managed && !w.DryRun:
			t.Live++
			t.SavedThisMonth += w.SavedThisMonth
		}
		replayed := w.History != nil && !w.Managed && !w.Protected
		if h := w.History; replayed {
			t.Replayed.Workloads++
			if h.SleepHours > 0 {
				t.Replayed.Sleepers++
			}
			t.Replayed.SleepHours += h.SleepHours
			t.Replayed.Wakes += h.Wakes
			t.Replayed.Freed += h.Freed
			t.Replayed.MonthlyFreed += h.MonthlyFreed
		}
		if w.ScaledByHand {
			t.ScaledToZero++
			continue
		}
		if w.State == StatePaused {
			t.Paused++
			t.PausedHourlyCost += w.HourlyCost
			continue
		}
		t.MonthlyCost += w.MonthlyCost
		if replayed {
			t.SavingsBasis += w.History.MonthlyCost
		} else {
			t.SavingsBasis += w.MonthlyCost
		}
		if w.State == StateIdle {
			t.Idle++
			t.IdleCPUMillis += w.PodCPURequestMillis * int64(w.Replicas)
			t.IdleMemoryBytes += w.PodMemoryRequestBytes * int64(w.Replicas)
			t.IdleHourlyCost += w.HourlyCost
			t.IdleMonthlyCost += w.MonthlyCost
		}
	}
	return t
}
