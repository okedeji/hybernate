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
	"errors"
	"fmt"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/metrics"
)

const bytesPerGiB = 1024 * 1024 * 1024

// pricingInterval is how long what a replica requests, and the list rates
// of the nodes it runs on, are kept before they're read from its pods
// again. Requests change with a deploy, which changes the pod template and
// prompts a read of its own, so this mostly catches pods moving to another
// node type.
const pricingInterval = time.Hour

// accumulateCost brings the workload's cost up to date once status is due a
// flush. Doing it on every check would read the target and its claims each
// minute only for the totals to be dropped unwritten.
func (r *Reconciler) accumulateCost(ctx context.Context, workload *v1alpha1.ManagedWorkload) {
	if c := workload.Status.Cost; c != nil && c.LastAccumulatedAt != nil &&
		r.now().Sub(c.LastAccumulatedAt.Time) < statusFlushInterval {
		return
	}
	r.settleCost(ctx, workload)
}

// settleCost counts the time since cost was last brought up to date in the
// workload's current phase. A transition settles before it changes the
// phase, so a pause is counted as paused right up to the wake.
func (r *Reconciler) settleCost(ctx context.Context, workload *v1alpha1.ManagedWorkload) {
	if r.metrics == nil {
		return
	}
	now := r.now()
	if workload.Status.Cost == nil {
		workload.Status.Cost = &v1alpha1.CostStatus{}
	}
	c := workload.Status.Cost
	startMonth(c, now)

	from := now
	if c.LastAccumulatedAt != nil {
		from = c.LastAccumulatedAt.Time
	}
	elapsed := cost.Counted(now.Sub(from))

	if workload.Status.Phase == v1alpha1.PhasePaused {
		accruePaused(c, workload.Status.Pause, elapsed)
	} else if err := r.accrueAwake(ctx, workload, elapsed, now); err != nil {
		log.FromContext(ctx).V(1).Info("not accumulating cost", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
		return
	}

	c.Tracked.Duration += elapsed
	c.LastAccumulatedAt = &metav1.Time{Time: now}
	priceCost(c, resolveCostRates(workload), now)

	c.ResourceReduction = nil
	if p := workload.Status.Pause; workload.Status.Phase == v1alpha1.PhasePaused && p != nil && p.Resources != nil {
		rs := p.Resources
		c.ResourceReduction = &v1alpha1.ResourceReduction{
			CPUMillis:   rs.CPUMillis * int64(rs.Replicas),
			MemoryBytes: rs.MemoryBytes * int64(rs.Replicas),
			Replicas:    rs.Replicas,
		}
	}
}

// startMonth starts new totals when now is in a later UTC month than they
// were last brought up to date in, counting from the start of the month so
// the time since then isn't lost. What was last read about the workload
// carries over. The time before the month began belonged to the last
// month's totals, which aren't kept.
func startMonth(c *v1alpha1.CostStatus, now time.Time) {
	last := c.LastAccumulatedAt
	month := cost.MonthOf(now)
	if last == nil || cost.MonthOf(last.Time).Equal(month) {
		return
	}
	*c = v1alpha1.CostStatus{
		Running:            c.Running,
		PricedAt:           c.PricedAt,
		PricedTemplateHash: c.PricedTemplateHash,
		ListRates:          c.ListRates,
	}
	if last.Time.Before(month) {
		c.LastAccumulatedAt = &metav1.Time{Time: month}
	}
}

// accruePaused counts a paused interval: what the paused replicas requested
// is saved, and their claims still cost.
func accruePaused(c *v1alpha1.CostStatus, pause *v1alpha1.PauseStatus, elapsed time.Duration) {
	if pause == nil || pause.Resources == nil {
		return
	}
	rs := pause.Resources
	cores, gib := requested(rs)
	addHours(&c.PausedCPUHours, cores, elapsed)
	addHours(&c.PausedMemoryHours, gib, elapsed)
	addHours(&c.StorageHours, float64(rs.StorageBytes)/bytesPerGiB, elapsed)
}

// accrueAwake counts an awake interval at what the workload runs now: its
// replicas from the target, what each one requests, and its claims.
func (r *Reconciler) accrueAwake(ctx context.Context, workload *v1alpha1.ManagedWorkload, elapsed time.Duration, now time.Time) error {
	logger := log.FromContext(ctx)
	c := workload.Status.Cost
	running := &v1alpha1.ResourceSnapshot{}
	if c.Running != nil {
		*running = *c.Running
	}

	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil {
		return fmt.Errorf("reading replicas: %w", err)
	}
	running.Replicas = replicas

	if storage, err := r.metrics.TotalPVCBytes(ctx, workload); err != nil {
		logger.V(1).Info("keeping the last storage read", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
	} else {
		running.StorageBytes = int64(storage)
	}

	if pricingDue(workload, now) {
		r.recordListRates(ctx, workload)
		if cpuMillis, memBytes, err := r.metrics.PodRequestsPerReplica(ctx, workload); err != nil {
			logger.V(1).Info("keeping the last requests read", "workload", workload.Name,
				"namespace", workload.Namespace, "error", err.Error())
		} else {
			running.CPUMillis, running.MemoryBytes = int64(cpuMillis), int64(memBytes)
		}
		c.PricedAt = &metav1.Time{Time: now}
		c.PricedTemplateHash = templateHashOf(workload)
	}
	c.Running = running

	cores, gib := requested(running)
	addHours(&c.AwakeCPUHours, cores, elapsed)
	addHours(&c.AwakeMemoryHours, gib, elapsed)
	addHours(&c.StorageHours, float64(running.StorageBytes)/bytesPerGiB, elapsed)
	return nil
}

// pricingDue reports whether what a replica requests and its nodes' list
// rates should be read again: they never have been, they're older than
// pricingInterval, or the pod template has changed since.
func pricingDue(workload *v1alpha1.ManagedWorkload, now time.Time) bool {
	c := workload.Status.Cost
	if c.PricedAt == nil || now.Sub(c.PricedAt.Time) >= pricingInterval {
		return true
	}
	hash := templateHashOf(workload)
	return hash != "" && hash != c.PricedTemplateHash
}

func templateHashOf(workload *v1alpha1.ManagedWorkload) string {
	if a := workload.Status.Activity; a != nil {
		return a.TemplateHash
	}
	return ""
}

// priceCost derives the dollar figures from the month's hours at rates.
func priceCost(c *v1alpha1.CostStatus, rates cost.Rates, now time.Time) {
	spent := cost.Hours{
		CPU:     c.AwakeCPUHours.AsApproximateFloat64(),
		Memory:  c.AwakeMemoryHours.AsApproximateFloat64(),
		Storage: c.StorageHours.AsApproximateFloat64(),
	}.Price(rates)
	saved := cost.Hours{
		CPU:    c.PausedCPUHours.AsApproximateFloat64(),
		Memory: c.PausedMemoryHours.AsApproximateFloat64(),
	}.Price(rates)

	c.CostThisMonth = cost.FormatDollars(spent)
	c.SavedThisMonth = cost.FormatDollars(saved)
	c.CostWithoutHybernateThisMonth = cost.FormatDollars(spent + saved)
	c.ProjectedMonthlyCost = projected(spent, c.Tracked.Duration, now)
	c.ProjectedMonthlySavings = projected(saved, c.Tracked.Duration, now)
}

func projected(amount float64, tracked time.Duration, now time.Time) string {
	p, ok := cost.Project(amount, tracked, now)
	if !ok {
		return "pending"
	}
	return cost.FormatDollars(p)
}

// requested is what all of rs's replicas request, in vCPUs and GiB.
func requested(rs *v1alpha1.ResourceSnapshot) (cores, gib float64) {
	n := float64(rs.Replicas)
	return n * float64(rs.CPUMillis) / 1000, n * float64(rs.MemoryBytes) / bytesPerGiB
}

// addHours adds perHour over elapsed to q, to a billionth of an hour, and
// returns what it added. Five minutes of a 10m CPU request is under a
// thousandth of a vCPU-hour, which coarser units would round to nothing on
// every update.
func addHours(q *resource.Quantity, perHour float64, elapsed time.Duration) float64 {
	n := int64(math.Round(perHour * elapsed.Hours() * 1e9))
	q.Add(*resource.NewScaledQuantity(n, resource.Nano))
	return float64(n) / 1e9
}

// recordListRates notes what the nodes the workload runs on cost, which it's
// priced at from then on, including while it's paused. Without a pod on a
// node there's nothing new to price, so the last rates are kept.
func (r *Reconciler) recordListRates(ctx context.Context, workload *v1alpha1.ManagedWorkload) {
	if r.prices == nil {
		return
	}
	rates, listed, err := r.prices.ListRates(ctx, workload)
	if errors.Is(err, metrics.ErrNoScheduledPods) {
		return
	}
	if err != nil {
		log.FromContext(ctx).V(1).Info("keeping the last list rates", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
		return
	}
	if !listed {
		workload.Status.Cost.ListRates = nil
		return
	}
	workload.Status.Cost.ListRates = &v1alpha1.CostRates{
		CPUPerHour:    microQuantity(rates.CPUPerHour),
		MemoryPerHour: microQuantity(rates.MemoryPerHour),
	}
}

// microQuantity keeps a rate to a millionth of a dollar: a GiB-hour costs
// a few thousandths, which milli-units would round by a fifth.
func microQuantity(v float64) *resource.Quantity {
	return resource.NewScaledQuantity(int64(math.Round(v*1e6)), resource.Micro)
}

func (r *Reconciler) captureResourceSnapshot(ctx context.Context, workload *v1alpha1.ManagedWorkload) *v1alpha1.ResourceSnapshot {
	if r.metrics == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	snap := &v1alpha1.ResourceSnapshot{Replicas: 1}

	// Savings are priced on requests, not live usage: requests are what a
	// pod reserves on a node, and what discovery estimates are based on.
	replicas, err := r.metrics.Replicas(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture replicas for resource snapshot", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
	} else {
		snap.Replicas = replicas
	}

	cpuMillis, memBytes, err := r.metrics.PodRequestsPerReplica(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture requests for resource snapshot", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
	} else {
		snap.CPUMillis = int64(cpuMillis)
		snap.MemoryBytes = int64(memBytes)
	}

	pvcBytes, err := r.metrics.TotalPVCBytes(ctx, workload)
	if err != nil {
		logger.V(1).Info("could not capture pvc for resource snapshot", "workload", workload.Name,
			"namespace", workload.Namespace, "error", err.Error())
	} else {
		snap.StorageBytes = int64(pvcBytes)
	}

	return snap
}
