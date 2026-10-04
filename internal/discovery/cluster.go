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
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/metrics"
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
	History   *HistorySource `json:"history,omitempty"`
	Workloads []Workload     `json:"workloads"`
	// NodePrices are the node types the workloads were priced at.
	NodePrices Prices `json:"nodePrices"`
	// Notes say what the scan couldn't see and how that limits it.
	Notes  []string `json:"notes,omitempty"`
	Totals Totals   `json:"totals"`
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
}

// ScanCluster scans every Deployment and StatefulSet in the namespaces and
// judges each from its CPU right now, plus clues that don't need history.
// With History, it also replays the activity clock over each workload's
// recorded CPU. It only reads, and carries on past what it can't read,
// saying so in the report's notes.
func (s *Scanner) ScanCluster(ctx context.Context, opts ClusterOptions) (*ClusterReport, error) {
	report := &ClusterReport{Mode: ModeSnapshot}
	var since time.Time
	if opts.History != nil {
		report.Mode = ModeHistory
		report.History = &HistorySource{Prometheus: opts.History.Source}
	}
	haveMetrics := s.metricsAvailable(ctx, opts.Namespaces)
	if !haveMetrics {
		report.Notes = append(report.Notes,
			"the Metrics API isn't available, so CPU couldn't be measured; is metrics-server installed?")
	}

	pricing, nodePrices := s.readNodePrices(ctx)
	report.NodePrices = nodePrices

	var noHistory []string
	sources := map[workloadKey]workloadSource{}
	for _, namespace := range opts.Namespaces {
		history, historySince, err := s.readHistory(ctx, namespace, opts)
		if err != nil {
			report.Notes = append(report.Notes, fmt.Sprintf("couldn't read history for namespace %s: %v", namespace, err))
		}
		if history != nil && (since.IsZero() || historySince.Before(since)) {
			since = historySince
		}
		workloads, err := s.scanNamespace(ctx, namespace, haveMetrics, history, historySince, sources, pricing, opts)
		if err != nil {
			report.Notes = append(report.Notes, fmt.Sprintf("skipped namespace %s: %v", namespace, err))
			continue
		}
		for _, w := range workloads {
			if history != nil && w.History == nil && w.State != StateUnknown {
				noHistory = append(noHistory, w.Namespace+"/"+w.Name)
			}
		}
		report.Workloads = append(report.Workloads, workloads...)
	}
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
	if len(noHistory) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("%s no CPU history in Prometheus, so only their CPU right now is known: %s",
			plural(len(noHistory), "workload has", "workloads have"), listSome(noHistory)))
	}
	report.Notes = append(report.Notes, s.findDependencies(ctx, report.Workloads, sources, opts.Namespaces)...)

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

// stateOrder lists idle workloads first, the ones worth acting on.
var stateOrder = map[State]int{StateIdle: 0, StatePaused: 1, StateActive: 2, StateUnknown: 3}

func (s *Scanner) metricsAvailable(ctx context.Context, namespaces []string) bool {
	if len(namespaces) == 0 {
		return false
	}
	var list metricsv1beta1.PodMetricsList
	return s.client.List(ctx, &list, client.InNamespace(namespaces[0]), client.Limit(1)) == nil
}

func (s *Scanner) scanNamespace(ctx context.Context, namespace string, haveMetrics bool,
	history []containerCPU, historySince time.Time, sources map[workloadKey]workloadSource, pricing nodePricing,
	opts ClusterOptions) ([]Workload, error) {
	managed := s.managedInNamespace(ctx, namespace)
	rollouts, err := s.rollouts(ctx, namespace)
	if err != nil {
		return nil, err
	}
	now := opts.Now()

	var out []Workload
	for _, kind := range []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet} {
		objs, err := s.listWorkloads(ctx, namespace, kind)
		if err != nil {
			return nil, fmt.Errorf("listing %ss: %w", kind, err)
		}
		for _, obj := range objs {
			if obj.GetLabels()[v1alpha1.LabelIgnore] == "true" {
				continue
			}
			mw := managed[string(kind)+"/"+obj.GetName()]
			replicas, spec, matchLabels := workloadFields(obj)
			sources[workloadKey{namespace, kind, obj.GetName()}] = workloadSource{template: podTemplate(obj), managed: mw}
			n := int32(1)
			if replicas != nil {
				n = *replicas
			}
			w := Workload{
				Namespace: namespace,
				Kind:      kind,
				Name:      obj.GetName(),
				Replicas:  n,
				State:     StateUnknown,
				Managed:   mw != nil,
				DryRun:    mw != nil && mw.Spec.DryRun,
			}
			if times := rollouts[obj.GetUID()]; len(times) > 0 {
				last := slices.MaxFunc(times, func(a, b time.Time) int { return a.Compare(b) })
				w.LastDeployed = &last
			}
			ownCPU, ownMemory := metrics.Requests(metrics.WorkloadContainers(spec))
			w.PodCPURequestMillis, w.PodMemoryRequestBytes = ownCPU, ownMemory

			var pods []corev1.Pod
			if n == 0 {
				if pausedByHybernate(mw) {
					judgePaused(&w, mw, now)
				} else {
					w.State, w.Reason, w.ScaledByHand = StatePaused, "scaled to zero, not by Hybernate", true
				}
			} else {
				if len(matchLabels) > 0 {
					sel := labels.SelectorFromSet(matchLabels)
					pods = s.workloadPods(ctx, namespace, sel)
					w.PodCPURequestMillis, w.PodMemoryRequestBytes = metrics.PodRequests(pods, spec)
					used, measured := s.cpuUsed(ctx, namespace, sel, spec)
					switch {
					case !haveMetrics:
						w.Unmeasured = "no Metrics API"
					case ownCPU == 0:
						w.Unmeasured = "no CPU requests"
					case !measured:
						w.Unmeasured = "no metrics yet"
					default:
						percent := int(float64(used) / float64(ownCPU*int64(n)) * 100)
						w.CPUPercent, w.CPUMillisUsed = &percent, used
					}
				}
				if mw != nil && mw.Status.Activity != nil {
					judgeManaged(&w, mw, now, thresholdFor(mw, opts))
				} else {
					judgeUnmanaged(&w, obj, now, thresholdFor(mw, opts), idleAfterFor(mw, opts))
				}
			}
			w.Clues = clues(w, now)
			w.rates, w.OnSpot = pricing.ratesFor(pods, mw, opts)
			w.HourlyCost = hourlyCost(w, w.rates)
			w.MonthlyCost = w.HourlyCost * hoursPerMonth
			w.Measured = measured(mw, w.HourlyCost, now)
			w.SavedThisMonth = savedThisMonth(mw)
			if history != nil && ownCPU > 0 {
				w.History = replayWorkload(w, spec, history, historySince, rollouts[obj.GetUID()],
					thresholdFor(mw, opts), idleAfterFor(mw, opts), opts)
			}
			out = append(out, w)
		}
	}
	return out, nil
}

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
	m := &Measured{
		Since:      d.Since.Time,
		Pauses:     int(d.Pauses),
		Wakes:      max(wakes, 0),
		SleptHours: slept.Hours(),
		Freed:      hourlyCost * slept.Hours(),
	}
	if measuring := now.Sub(d.Since.Time).Hours(); measuring > 0 {
		m.MonthlyFreed = m.Freed / measuring * hoursPerMonth
	}
	return m
}

// savedThisMonth reads what Hybernate records it has saved this month
// pausing a live workload.
func savedThisMonth(mw *v1alpha1.ManagedWorkload) float64 {
	if mw == nil || mw.Spec.DryRun || mw.Status.Cost == nil {
		return 0
	}
	saved, err := strconv.ParseFloat(strings.TrimPrefix(mw.Status.Cost.EstimatedMonthlySavings, "$"), 64)
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

// judgeUnmanaged applies the activity clock's sources a scan can see
// without Hybernate: activity annotations, a recent rollout, and CPU.
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

// cpuUsed returns the CPU the workload's own containers use, and false when
// the Metrics API has nothing for its pods yet, which isn't the same as idle.
func (s *Scanner) cpuUsed(ctx context.Context, namespace string, sel labels.Selector, spec corev1.PodSpec) (int64, bool) {
	var list metricsv1beta1.PodMetricsList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil ||
		len(list.Items) == 0 {
		return 0, false
	}
	used, _ := metrics.Usage(list.Items, spec)
	return used, true
}

// managedInNamespace returns the ManagedWorkload covering each workload,
// keyed by "Kind/name". Without Hybernate installed there are none, which
// isn't an error.
func (s *Scanner) managedInNamespace(ctx context.Context, namespace string) map[string]*v1alpha1.ManagedWorkload {
	out := map[string]*v1alpha1.ManagedWorkload{}
	var list v1alpha1.ManagedWorkloadList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return out
	}
	for i := range list.Items {
		mw := &list.Items[i]
		out[string(mw.Spec.Target.Kind)+"/"+mw.Spec.Target.Name] = mw
	}
	return out
}

// rollouts returns when each workload rolled out, from the ReplicaSets and
// ControllerRevisions it keeps: one per pod template it has run, as many as
// its revision history limit keeps.
func (s *Scanner) rollouts(ctx context.Context, namespace string) (map[types.UID][]time.Time, error) {
	times := map[types.UID][]time.Time{}
	note := func(owner client.Object) {
		for _, ref := range owner.GetOwnerReferences() {
			if ref.Controller != nil && *ref.Controller {
				times[ref.UID] = append(times[ref.UID], owner.GetCreationTimestamp().Time)
			}
		}
	}
	var replicaSets appsv1.ReplicaSetList
	if err := s.client.List(ctx, &replicaSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	for i := range replicaSets.Items {
		note(&replicaSets.Items[i])
	}
	var revisions appsv1.ControllerRevisionList
	if err := s.client.List(ctx, &revisions, client.InNamespace(namespace)); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("listing controllerrevisions: %w", err)
	}
	for i := range revisions.Items {
		note(&revisions.Items[i])
	}
	return times, nil
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
		if h := w.History; h != nil && !w.Managed {
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
