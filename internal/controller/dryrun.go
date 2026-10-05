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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

// trackDryRun starts the dry-run summary when a workload enters dry-run and
// drops it when it leaves, since it describes a measurement that's over.
func (r *Reconciler) trackDryRun(workload *v1alpha1.ManagedWorkload) {
	if !workload.Spec.DryRun {
		workload.Status.DryRun = nil
		return
	}
	if workload.Status.DryRun == nil {
		workload.Status.DryRun = &v1alpha1.DryRunStatus{Since: r.clockTime(), EstimatedSavings: cost.FormatDollars(0)}
	}
}

// beginWouldBePause counts a pause dry-run held back, and records what the
// workload runs now, which the pause would have freed.
func (r *Reconciler) beginWouldBePause(ctx context.Context, workload *v1alpha1.ManagedWorkload) {
	r.trackDryRun(workload)
	workload.Status.DryRun.Pauses++
	workload.Status.DryRun.Resources = r.captureResourceSnapshot(ctx, workload)
}

// endWouldBePause adds the would-be pause that began when the workload went
// Idle and ended at its latest activity, when a paused workload would have
// been woken. It returns how long that pause lasted and what it would have
// freed, and false when dry-run wasn't measuring as it began, such as for a
// workload already Idle when the operator was upgraded.
func (r *Reconciler) endWouldBePause(workload *v1alpha1.ManagedWorkload) (time.Duration, float64, bool) {
	d := workload.Status.DryRun
	if !workload.Spec.DryRun || d == nil || d.Pauses == 0 || workload.Status.LastTransitionTime == nil {
		return 0, 0, false
	}
	slept := max(workload.Status.Activity.LastActivityTime.Sub(workload.Status.LastTransitionTime.Time), 0)
	rates := resolveCostRates(workload)
	var freed cost.Hours
	if rs := d.Resources; rs != nil {
		cores, gib := requested(rs)
		freed.CPU = addHours(&d.FreedCPUHours, cores, slept)
		freed.Memory = addHours(&d.FreedMemoryHours, gib, slept)
	}
	d.Slept.Duration += slept
	d.EstimatedSavings = cost.FormatDollars(cost.Hours{
		CPU:    d.FreedCPUHours.AsApproximateFloat64(),
		Memory: d.FreedMemoryHours.AsApproximateFloat64(),
	}.Price(rates))
	d.Resources = nil
	return slept, freed.Price(rates), true
}

// dryRunSummary describes what dry-run has measured so far, for events.
func dryRunSummary(d *v1alpha1.DryRunStatus) string {
	times := "times"
	if d.Pauses == 1 {
		times = "time"
	}
	return fmt.Sprintf("since %s: would have paused %d %s, slept %s, freeing %s",
		d.Since.Format("Jan 2"), d.Pauses, times, roundedDuration(d.Slept.Duration), d.EstimatedSavings)
}

// roundedDuration is d to the minute, without the zero units Duration's
// String adds: "3h12m", "96h", "45m".
func roundedDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
