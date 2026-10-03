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
	"context"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/metrics"
)

// minStep is the finest history a replay reads: the activity clock checks
// CPU about once a minute, but five minutes is what most Prometheus setups
// can return for a week without hitting their per-query limits.
const minStep = 5 * time.Minute

// maxPointsPerSeries keeps a range query under Prometheus's default limit
// of 11,000 points per series, so long windows use coarser steps.
const maxPointsPerSeries = 10000

// HistorySource says what a scan's history came from.
type HistorySource struct {
	// Prometheus is the Prometheus the CPU history was read from.
	Prometheus string    `json:"prometheus"`
	Since      time.Time `json:"since"`
	// Hours is how much history the replay covered, which is less than the
	// window asked for when Prometheus keeps less.
	Hours float64 `json:"hours"`
}

// History is what replaying the activity clock over a workload's recorded
// CPU and rollouts found: what Hybernate would have done had it managed the
// workload with these settings.
type History struct {
	Hours float64 `json:"hours"`
	// RunningHours had at least one of its pods running.
	RunningHours float64 `json:"runningHours"`
	// IdleHours were running with CPU below the threshold and no rollout.
	IdleHours float64 `json:"idleHours"`
	// SleepHours were running while the clock had run out, so Hybernate
	// would have had the workload paused.
	SleepHours float64 `json:"sleepHours"`
	// Wakes is how many times activity after a would-be pause would have
	// woken it, each costing whoever caused it a wait while it started.
	Wakes int `json:"wakes"`
	// Freed is what the pods running during SleepHours cost, and
	// MonthlyFreed that over an average month at the same rate.
	Freed        float64 `json:"freed"`
	MonthlyFreed float64 `json:"monthlyFreed"`
}

// replay is one workload's recorded history, and the clock's settings to
// replay it with.
type replay struct {
	start, end time.Time
	step       time.Duration
	// usedCores is the CPU the workload's own containers used at each step,
	// and pods how many of its pods were running then, keyed by Unix time.
	usedCores map[int64]float64
	pods      map[int64]int
	rollouts  []time.Time
	// requestCores is what one pod's own containers request; podHourly is
	// what one pod, sidecars included, costs an hour.
	requestCores float64
	podHourly    float64
	threshold    int
	idleAfter    time.Duration
}

// run walks the history a step at a time under the activity clock's rules:
// CPU at or above the threshold, or a rollout, is activity, and once there
// has been none for idleAfter the workload would be paused until the next.
// The clock starts at the beginning of the history, as it does for a new
// ManagedWorkload.
func (r replay) run() History {
	stepHours := r.step.Hours()
	var h History
	lastActive := r.start
	asleep := false
	rollouts := slices.Clone(r.rollouts)
	slices.SortFunc(rollouts, func(a, b time.Time) int { return a.Compare(b) })

	for t := r.start; !t.After(r.end); t = t.Add(r.step) {
		at := t.Unix()
		pods := r.pods[at]
		active := rolledOutBetween(rollouts, t.Add(-r.step), t)
		if pods > 0 && r.requestCores > 0 {
			percent := r.usedCores[at] / (float64(pods) * r.requestCores) * 100
			active = active || percent >= float64(r.threshold)
		}
		switch {
		case active:
			if asleep {
				h.Wakes++
				asleep = false
			}
			lastActive = t
		case t.Sub(lastActive) >= r.idleAfter:
			asleep = true
		}

		h.Hours += stepHours
		if pods == 0 {
			continue
		}
		h.RunningHours += stepHours
		if !active {
			h.IdleHours += stepHours
		}
		if asleep {
			h.SleepHours += stepHours
			h.Freed += float64(pods) * r.podHourly * stepHours
		}
	}
	if h.Hours > 0 {
		h.MonthlyFreed = h.Freed / h.Hours * hoursPerMonth
	}
	return h
}

func rolledOutBetween(sorted []time.Time, after, upTo time.Time) bool {
	i, _ := slices.BinarySearchFunc(sorted, after, func(t, target time.Time) int {
		if t.After(target) {
			return 1
		}
		return -1
	})
	return i < len(sorted) && !sorted[i].After(upTo)
}

// podsOf matches the names of a workload's pods: a Deployment's are named
// after it with its ReplicaSet's hash and a random suffix, a StatefulSet's
// with an ordinal. A Deployment's hash and suffix never contain a dash, so
// "api" doesn't match "api-gateway"'s pods.
func podsOf(kind v1alpha1.TargetKind, name string) *regexp.Regexp {
	if kind == v1alpha1.TargetKindStatefulSet {
		return regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[0-9]+$`)
	}
	return regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-[a-z0-9]{1,10}-[a-z0-9]{5}$`)
}

// workloadSeries sums a workload's own containers' CPU at each step, and
// counts its pods, from a namespace's history. Containers not in its pod
// template, such as injected sidecars, cost money but aren't activity, as
// for the activity clock.
func workloadSeries(history []containerCPU, pods *regexp.Regexp, containers []string) (
	usedCores map[int64]float64, podCount map[int64]int, found bool) {
	usedCores = map[int64]float64{}
	running := map[int64]map[string]bool{}
	for _, series := range history {
		if !pods.MatchString(series.pod) {
			continue
		}
		found = true
		own := slices.Contains(containers, series.container)
		for at, cores := range series.samples {
			if running[at] == nil {
				running[at] = map[string]bool{}
			}
			running[at][series.pod] = true
			if own {
				usedCores[at] += cores
			}
		}
	}
	podCount = make(map[int64]int, len(running))
	for at, names := range running {
		podCount[at] = len(names)
	}
	return usedCores, podCount, found
}

// historyStep is the step a window is read at: minStep, or coarser for a
// window too long to read at it.
func historyStep(window time.Duration) time.Duration {
	step := max(minStep, window/maxPointsPerSeries)
	return step.Round(time.Minute)
}

// earliestSample is the first step any series in the history has, which is
// where the history Prometheus keeps begins.
func earliestSample(history []containerCPU) (int64, bool) {
	var first int64
	found := false
	for _, series := range history {
		for at := range series.samples {
			if !found || at < first {
				first, found = at, true
			}
		}
	}
	return first, found
}

// readHistory reads a namespace's CPU history over the window, and where it
// begins. It returns nothing when the scan has no history to read, or the
// namespace has none recorded.
func (s *Scanner) readHistory(ctx context.Context, namespace string, opts ClusterOptions) (
	[]containerCPU, time.Time, error) {
	if opts.History == nil {
		return nil, time.Time{}, nil
	}
	step := historyStep(opts.Window)
	end := opts.Now().Truncate(step)
	history, err := opts.History.namespaceCPU(ctx, namespace, end.Add(-opts.Window), end, step)
	if err != nil {
		return nil, time.Time{}, err
	}
	first, ok := earliestSample(history)
	if !ok {
		return nil, time.Time{}, nil
	}
	return history, time.Unix(first, 0), nil
}

// replayWorkload replays the activity clock over one workload's share of
// its namespace's history, or returns nil when none of it is the workload's.
// Its requests and prices are today's, since the history only records CPU.
func replayWorkload(w Workload, spec corev1.PodSpec, history []containerCPU, since time.Time,
	rollouts []time.Time, opts ClusterOptions) *History {
	containers := make([]string, 0, len(spec.Containers))
	for _, c := range metrics.WorkloadContainers(spec) {
		containers = append(containers, c.Name)
	}
	used, pods, found := workloadSeries(history, podsOf(w.Kind, w.Name), containers)
	if !found {
		return nil
	}
	ownCPU, _ := metrics.Requests(metrics.WorkloadContainers(spec))
	step := historyStep(opts.Window)
	h := replay{
		start:        since,
		end:          opts.Now().Truncate(step),
		step:         step,
		usedCores:    used,
		pods:         pods,
		rollouts:     rollouts,
		requestCores: float64(ownCPU) / 1000,
		podHourly: cost.ComputeHourly(float64(w.PodCPURequestMillis)/1000,
			float64(w.PodMemoryRequestBytes)/(1<<30), opts.Rates),
		threshold: opts.CPUThreshold,
		idleAfter: opts.IdleAfter,
	}.run()
	return &h
}

// monthlyFreed is what a workload's history says pausing it would free a
// month, for ordering.
func monthlyFreed(w Workload) float64 {
	if w.History == nil {
		return 0
	}
	return w.History.MonthlyFreed
}
