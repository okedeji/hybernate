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
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
)

// stuckAfter is how long a pause or wake may take before status reports it.
// Both normally finish in seconds; a wake waiting on a slow dependency can
// take a few minutes.
const stuckAfter = 10 * time.Minute

// errNotInstalled means the cluster has no ManagedWorkload CRD.
var errNotInstalled = errors.New("no ManagedWorkload CRD, so Hybernate isn't installed in this cluster; " +
	"kubectl hybernate scan works without it, and shows what it would pause")

// recentLimit caps the pauses and wakes listed, so a busy cluster still fits
// one screen; -o json has them all.
const recentLimit = 10

type statusOptions struct {
	context    string
	namespaces []string
	output     string
	since      time.Duration
	now        func() time.Time
}

func statusCmd() *cobra.Command {
	opts := statusOptions{since: 24 * time.Hour, now: time.Now}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what Hybernate is doing in a cluster right now",
		Long: `Status shows every workload Hybernate manages on one screen: what each is
doing, its last activity, when it pauses next or what holds it awake, and what
Hybernate has saved this month. Above the table it lists what needs attention,
and below it recent pauses and wakes, with what caused each, as far back as
the cluster keeps events: an hour, unless its API server is set to keep more.

It only reads ManagedWorkloads and their events.

Examples:
  kubectl hybernate status
  kubectl hybernate status --context staging -n preview-42
  kubectl hybernate status --since 1h -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(opts.output); err != nil {
				return err
			}
			k8s, _, contextName, err := buildClientFor(opts.context)
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			result, err := clusterStatus(ctx, k8s, opts)
			if err != nil {
				return err
			}
			result.Context = contextName
			result.Cluster = clusterName(contextName)
			return writeStatus(cmd.OutOrStdout(), result, opts.output, opts.now())
		},
	}
	cmd.Flags().StringVar(&opts.context, "context", "", "Kubeconfig context of the cluster (defaults to the current one)")
	cmd.Flags().StringSliceVarP(&opts.namespaces, "namespace", "n", nil,
		"Namespace to show; repeat for several (defaults to all you can read)")
	addOutputFlag(cmd, &opts.output)
	cmd.Flags().DurationVar(&opts.since, "since", opts.since, "How far back to list pauses and wakes")
	return cmd
}

type statusResult struct {
	Context        string           `json:"context"`
	Cluster        string           `json:"cluster"`
	SavedThisMonth float64          `json:"savedThisMonth"`
	Workloads      []statusWorkload `json:"workloads"`
	Problems       []problem        `json:"problems"`
	Recent         []happening      `json:"recent"`
	// EventsHidden is set when the user can't read events, so Recent is
	// empty for that reason and not because nothing happened.
	EventsHidden bool `json:"eventsHidden,omitempty"`
}

type statusWorkload struct {
	Namespace      string                 `json:"namespace"`
	Name           string                 `json:"managedWorkload"`
	Target         string                 `json:"target"`
	Phase          v1alpha1.WorkloadPhase `json:"phase"`
	DryRun         bool                   `json:"dryRun"`
	Since          *time.Time             `json:"since,omitempty"`
	LastActivity   *time.Time             `json:"lastActivity,omitempty"`
	ActivitySource string                 `json:"activitySource,omitempty"`
	Next           string                 `json:"next"`
	SavedThisMonth float64                `json:"savedThisMonth"`
}

// problem is something about a workload that keeps Hybernate from doing
// its job, and needs a person. Workload is the target's name, which is how
// status names workloads; ManagedWorkload is what to kubectl describe.
type problem struct {
	Namespace       string `json:"namespace"`
	Workload        string `json:"workload"`
	ManagedWorkload string `json:"managedWorkload"`
	Type            string `json:"type"`
	Message         string `json:"message"`
}

// happening is one pause or wake.
type happening struct {
	At              time.Time `json:"at"`
	Namespace       string    `json:"namespace"`
	Workload        string    `json:"workload"`
	ManagedWorkload string    `json:"managedWorkload"`
	Reason          string    `json:"reason"`
	Message         string    `json:"message"`
}

// troubles are the conditions in the state that's a problem: most are
// raised as True, and the ones that report something working are a
// problem when False.
var troubles = map[string]metav1.ConditionStatus{
	"GitOpsConflict":      metav1.ConditionTrue,
	"DependencyCycle":     metav1.ConditionTrue,
	"DependencyNotFound":  metav1.ConditionTrue,
	"DuplicateTarget":     metav1.ConditionTrue,
	"Protected":           metav1.ConditionTrue,
	"TargetAvailable":     metav1.ConditionFalse,
	"MetricsAvailable":    metav1.ConditionFalse,
	"PrometheusAvailable": metav1.ConditionFalse,
	"WakeOnRequest":       metav1.ConditionFalse,
}

// happenings are the event reasons that say a workload paused or woke and
// why. Resumed is left out: it follows every wake, after the event that
// says what caused it. A wake by a request the doorman held is reported by
// the doorman's WokenByRequest, which names the Service, so the operator's
// own WokeByActivity for it is left out too.
var happenings = map[string]bool{
	"Paused":           true,
	"WokeByActivity":   true,
	"WokenByRequest":   true,
	"AutoResume":       true,
	"ScaledUp":         true,
	"RequestNotServed": true,
}

const requestWakeMessage = "a request is waiting, waking"

func clusterStatus(ctx context.Context, c client.Client, opts statusOptions) (statusResult, error) {
	now := opts.now()
	workloads, err := listManaged(ctx, c, opts.namespaces)
	if err != nil {
		return statusResult{}, err
	}
	result := statusResult{Workloads: []statusWorkload{}, Problems: []problem{}, Recent: []happening{}}
	targets := map[client.ObjectKey]string{}
	for i := range workloads {
		w := &workloads[i]
		row := workloadStatus(w, now)
		targets[client.ObjectKeyFromObject(w)] = w.Spec.Target.Name
		result.Workloads = append(result.Workloads, row)
		result.SavedThisMonth += row.SavedThisMonth
		result.Problems = append(result.Problems, problemsOf(w, now)...)
	}
	recent, err := recentHappenings(ctx, c, opts.namespaces, targets, now.Add(-opts.since))
	if apierrors.IsForbidden(err) {
		result.EventsHidden = true
	} else if err != nil {
		return statusResult{}, err
	}
	result.Recent = append(result.Recent, recent...)
	return result, nil
}

func listManaged(ctx context.Context, c client.Client, namespaces []string) ([]v1alpha1.ManagedWorkload, error) {
	var all []v1alpha1.ManagedWorkload
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, ns := range namespaces {
		var list v1alpha1.ManagedWorkloadList
		if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
			if meta.IsNoMatchError(err) {
				return nil, errNotInstalled
			}
			if ns == metav1.NamespaceAll && apierrors.IsForbidden(err) {
				return nil, fmt.Errorf("listing ManagedWorkloads: %w; pass -n for the namespaces you can read", err)
			}
			return nil, fmt.Errorf("listing ManagedWorkloads: %w", err)
		}
		all = append(all, list.Items...)
	}
	slices.SortFunc(all, func(a, b v1alpha1.ManagedWorkload) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return all, nil
}

func workloadStatus(w *v1alpha1.ManagedWorkload, now time.Time) statusWorkload {
	row := statusWorkload{
		Namespace:      w.Namespace,
		Name:           w.Name,
		Target:         strings.ToLower(string(w.Spec.Target.Kind)) + "/" + w.Spec.Target.Name,
		Phase:          w.Status.Phase,
		DryRun:         w.Spec.DryRun,
		Next:           nextFor(w, now),
		SavedThisMonth: discovery.SavedThisMonth(w),
	}
	if t := w.Status.LastTransitionTime; t != nil {
		row.Since = &t.Time
	}
	if a := w.Status.Activity; a != nil && !a.LastActivityTime.IsZero() {
		row.LastActivity = &a.LastActivityTime.Time
		row.ActivitySource = string(a.LastActivitySource)
	}
	return row
}

// nextFor says what happens to a workload next, or what's stopping it.
func nextFor(w *v1alpha1.ManagedWorkload, now time.Time) string {
	if d := w.Spec.DesiredState; d != nil {
		return "kept " + strings.ToLower(string(*d)) + " by desiredState"
	}
	switch w.Status.Phase {
	case v1alpha1.PhasePaused:
		if len(w.Status.Doorman) > 0 {
			return "wakes on a request or activity"
		}
		return "wakes on activity"
	case v1alpha1.PhaseRunning, v1alpha1.PhaseIdle:
	default:
		return "-"
	}
	if meta.IsStatusConditionTrue(w.Status.Conditions, "HeldByDependents") {
		return "held awake by its dependents"
	}
	until, err := time.Parse(time.RFC3339, w.Annotations[v1alpha1.AnnotationActiveUntil])
	if err == nil && until.After(now) {
		return "held awake for " + span(until.Sub(now))
	}
	a := w.Status.Activity
	if a == nil || a.PauseAt == nil {
		return "-"
	}
	verb := "pauses"
	if w.Spec.DryRun {
		verb = "would pause"
	}
	if !a.PauseAt.After(now) {
		return verb + " now"
	}
	return verb + " in " + span(a.PauseAt.Sub(now))
}

func problemsOf(w *v1alpha1.ManagedWorkload, now time.Time) []problem {
	var out []problem
	for _, c := range w.Status.Conditions {
		bad, ok := troubles[c.Type]
		if !ok || c.Status != bad || (c.Type == "WakeOnRequest" && c.Reason == "NoServices") {
			continue
		}
		out = append(out, problem{Namespace: w.Namespace, Workload: w.Spec.Target.Name, ManagedWorkload: w.Name,
			Type: c.Type, Message: cmp.Or(c.Message, c.Reason)})
	}
	phase := w.Status.Phase
	if t := w.Status.LastTransitionTime; t != nil && (phase == v1alpha1.PhasePausing || phase == v1alpha1.PhaseResuming) {
		if stuck := now.Sub(t.Time); stuck >= stuckAfter {
			msg := fmt.Sprintf("%s for %s", strings.ToLower(string(phase)), span(stuck))
			if c := meta.FindStatusCondition(w.Status.Conditions, "WaitingForDependencies"); c != nil &&
				c.Status == metav1.ConditionTrue {
				msg += ": " + c.Message
			}
			out = append(out, problem{Namespace: w.Namespace, Workload: w.Spec.Target.Name, ManagedWorkload: w.Name,
				Type: "Stuck", Message: msg})
		}
	}
	return out
}

// recentHappenings lists the pauses and wakes since a time, newest first.
// Dry-run events are left out: nothing paused or woke. The operator starts
// its messages with the target's name, which is dropped, since status shows
// the workload beside it.
func recentHappenings(ctx context.Context, c client.Client, namespaces []string,
	targets map[client.ObjectKey]string, since time.Time) ([]happening, error) {
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	var out []happening
	for _, ns := range namespaces {
		var events corev1.EventList
		if err := c.List(ctx, &events, client.InNamespace(ns),
			client.MatchingFields{"involvedObject.kind": "ManagedWorkload"}); err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		for i := range events.Items {
			ev := &events.Items[i]
			at := eventTime(ev)
			if !happenings[ev.Reason] || at.Before(since) || strings.HasPrefix(ev.Message, "[dry-run]") {
				continue
			}
			key := client.ObjectKey{Namespace: ev.InvolvedObject.Namespace, Name: ev.InvolvedObject.Name}
			// Events outlive the ManagedWorkload they're for; one that's gone
			// is named as it was.
			target := cmp.Or(targets[key], key.Name)
			message := strings.TrimPrefix(ev.Message, target+": ")
			if ev.Reason == "WokeByActivity" && message == requestWakeMessage {
				continue
			}
			out = append(out, happening{At: at, Namespace: key.Namespace, Workload: target, ManagedWorkload: key.Name,
				Reason: ev.Reason, Message: message})
		}
	}
	slices.SortStableFunc(out, func(a, b happening) int { return b.At.Compare(a.At) })
	return out, nil
}

// eventTime is when an event last happened. Events recorded through
// events.k8s.io set eventTime, and series.lastObservedTime once repeated;
// older recorders set lastTimestamp.
func eventTime(ev *corev1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	}
	return ev.FirstTimestamp.Time
}

func writeStatus(w io.Writer, result statusResult, output string, now time.Time) error {
	return writeOutput(w, output, result, func() error { return writeStatusTable(w, result, now) })
}

func writeStatusTable(w io.Writer, result statusResult, now time.Time) error {
	p := &printer{w: w}
	if len(result.Workloads) == 0 {
		p.line("%s: Hybernate manages no workloads here.", result.Cluster)
		p.line("kubectl hybernate scan finds idle ones; label one hybernate.io/managed=true for Hybernate to manage it.")
		return p.err
	}
	p.line("%s: Hybernate manages %s.", result.Cluster, countOf(len(result.Workloads), "workload"))
	p.line("  %s", phaseCounts(result.Workloads))
	if result.SavedThisMonth > 0 {
		p.line("  Saved %s this month.", cents(result.SavedThisMonth))
	}

	if len(result.Problems) > 0 {
		p.line("")
		p.line("Needs attention:")
		tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
		for _, pr := range result.Problems {
			_, _ = fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", pr.Namespace, pr.Workload, pr.Type, pr.Message)
		}
		_ = tw.Flush() // writes through p, which keeps the first error
	}

	p.line("")
	tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAMESPACE\tWORKLOAD\tSTATE\tFOR\tLAST ACTIVITY\tNEXT\tSAVED THIS MONTH")
	for _, wl := range result.Workloads {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", wl.Namespace, wl.Target, stateCell(wl),
			sinceCell(wl.Since, now), activityCell(wl, now), wl.Next, statusSavedCell(wl))
	}
	_ = tw.Flush() // writes through p, which keeps the first error

	p.line("")
	switch {
	case result.EventsHidden:
		p.line("Recent pauses and wakes aren't shown: your access doesn't allow reading events.")
	case len(result.Recent) == 0:
		p.line("No pauses or wakes in this period.")
	default:
		p.line("Recent pauses and wakes:")
		tw := tabwriter.NewWriter(p, 0, 0, 3, ' ', 0)
		for i, h := range result.Recent {
			if i == recentLimit {
				_, _ = fmt.Fprintf(tw, "  and %d more; -o json lists them all\n", len(result.Recent)-recentLimit)
				break
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n", ago(h.At, now), h.Namespace, h.Workload, h.Message)
		}
		_ = tw.Flush() // writes through p, which keeps the first error
	}
	return p.err
}

// phaseCounts is how many workloads are in each phase, busiest phases first.
func phaseCounts(workloads []statusWorkload) string {
	order := []v1alpha1.WorkloadPhase{v1alpha1.PhasePaused, v1alpha1.PhaseRunning, v1alpha1.PhaseIdle,
		v1alpha1.PhasePausing, v1alpha1.PhaseResuming, v1alpha1.PhaseCreating}
	counts := map[v1alpha1.WorkloadPhase]int{}
	dryRun := 0
	for _, wl := range workloads {
		counts[cmp.Or(wl.Phase, v1alpha1.PhaseCreating)]++
		if wl.DryRun {
			dryRun++
		}
	}
	var parts []string
	for _, phase := range order {
		if n := counts[phase]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(phase))))
		}
	}
	s := strings.Join(parts, ", ")
	if dryRun > 0 {
		s += fmt.Sprintf("; %d in dry-run, measured but never paused", dryRun)
	}
	return s
}

func stateCell(wl statusWorkload) string {
	state := strings.ToLower(string(cmp.Or(wl.Phase, v1alpha1.PhaseCreating)))
	if wl.DryRun {
		state += " (dry-run)"
	}
	return state
}

func sinceCell(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	return span(now.Sub(*t))
}

func activityCell(wl statusWorkload, now time.Time) string {
	if wl.LastActivity == nil {
		return "-"
	}
	return wl.ActivitySource + ", " + ago(*wl.LastActivity, now)
}

func statusSavedCell(wl statusWorkload) string {
	if wl.DryRun || wl.SavedThisMonth == 0 {
		return "-"
	}
	return cents(wl.SavedThisMonth)
}

func ago(t, now time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return span(d) + " ago"
}

// span is a duration to its largest unit, as kubectl shows ages: 45s, 12m,
// 3h, 6d.
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
