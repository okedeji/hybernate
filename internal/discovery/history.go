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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

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
	// QuietHours were running with no activity: CPU below the threshold
	// and no rollout. A workload sleeps through fewer, since after each
	// activity the clock waits idleAfter before pausing it.
	QuietHours float64 `json:"quietHours"`
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
			h.QuietHours += stepHours
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

// podNames matches the names of a workload's pods.
type podNames func(pod string) bool

// The names a ReplicaSet gives its pods: the API server cuts a generated
// name's base to leave room for the random suffix in 63 characters.
const (
	generatedSuffixLength = 5
	maxGeneratedNameBase  = 63 - generatedSuffixLength
)

// replicaSetPods matches the pods of a Deployment's ReplicaSets, each named
// after its ReplicaSet, a dash, and a random suffix. Matching the
// ReplicaSets' own names, hash and all, rather than the Deployment's, keeps a
// Job's or another Deployment's pods that share its name as a prefix out. A
// long name's base is cut short, which is matched as the API server cuts it.
func replicaSetPods(replicaSets []string) podNames {
	prefixes := make([]string, 0, len(replicaSets))
	for _, rs := range replicaSets {
		prefix := rs + "-"
		if len(prefix) > maxGeneratedNameBase {
			prefix = prefix[:maxGeneratedNameBase]
		}
		prefixes = append(prefixes, prefix)
	}
	return func(pod string) bool {
		for _, prefix := range prefixes {
			if len(pod) == len(prefix)+generatedSuffixLength && strings.HasPrefix(pod, prefix) {
				return true
			}
		}
		return false
	}
}

// statefulSetPods matches a StatefulSet's pods, named after it with an
// ordinal.
func statefulSetPods(name string) podNames {
	return func(pod string) bool {
		ordinal, ok := strings.CutPrefix(pod, name+"-")
		if !ok || ordinal == "" || (ordinal[0] == '0' && len(ordinal) > 1) {
			return false
		}
		return strings.Trim(ordinal, "0123456789") == ""
	}
}

// workloadSeries sums a workload's own containers' CPU at each step, and
// counts its pods, from a namespace's history. Containers not in its pod
// template, such as injected sidecars, cost money but aren't activity, as
// for the activity clock.
func workloadSeries(history []containerCPU, pods podNames, containers []string) (
	usedCores map[int64]float64, podCount map[int64]int, found bool) {
	usedCores = map[int64]float64{}
	running := map[int64]map[string]bool{}
	for _, series := range history {
		if !pods(series.pod) {
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

// readHistory reads a namespace's CPU history over the window. It returns
// nothing when the scan has no history to read, or the namespace has none
// recorded.
func (s *Scanner) readHistory(ctx context.Context, namespace string, opts ClusterOptions) ([]containerCPU, error) {
	if opts.History == nil {
		return nil, nil
	}
	step := historyStep(opts.Window)
	end := opts.Now().Truncate(step)
	history, err := opts.History.namespaceCPU(ctx, namespace, end.Add(-opts.Window), end, step)
	if err != nil {
		return nil, err
	}
	if _, ok := earliestSample(history); !ok {
		return nil, nil
	}
	return history, nil
}

// replayStart is the first step of the history at or after a workload was
// created. Steps fall where Prometheus put them, which is where the
// namespace's history begins and every step after.
func replayStart(historySince, created time.Time, step time.Duration) time.Time {
	if !created.After(historySince) {
		return historySince
	}
	steps := (created.Sub(historySince) + step - 1) / step
	return historySince.Add(steps * step)
}

// replayWorkload replays the activity clock over one workload's share of
// its namespace's history, or returns nil when none of it is the workload's.
// The replay starts at since, where the history begins or the workload was
// created if later, as the activity clock starts when a workload is created:
// one younger than the window isn't charged for hours before it existed.
// Its requests and prices are today's, since the history only records CPU.
func replayWorkload(w Workload, spec corev1.PodSpec, history []containerCPU, podsOf podNames, since time.Time,
	rollouts []time.Time, threshold int, idleAfter time.Duration, opts ClusterOptions) *History {
	containers := make([]string, 0, len(spec.Containers))
	for _, c := range metrics.WorkloadContainers(spec) {
		containers = append(containers, c.Name)
	}
	used, pods, found := workloadSeries(history, podsOf, containers)
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
			float64(w.PodMemoryRequestBytes)/(1<<30), w.rates),
		threshold: threshold,
		idleAfter: idleAfter,
	}.run()
	return &h
}

// CouldSave is what pausing a workload Hybernate doesn't pause yet would
// free a month: measured, for one in dry-run, or estimated from history,
// for one Hybernate doesn't manage. It's 0 for a live workload, whose
// saving is already happening, and for one already scaled to zero, which
// pausing can't free any more of.
func CouldSave(w Workload) float64 {
	switch {
	case w.ScaledByHand:
		return 0
	case w.Measured != nil:
		return w.Measured.MonthlyFreed
	case !w.Managed && !w.Protected && w.History != nil:
		return w.History.MonthlyFreed
	}
	return 0
}
