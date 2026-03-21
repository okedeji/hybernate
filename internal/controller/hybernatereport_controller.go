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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

const reportSingletonName = "hybernate-report"

// HybernateReportReconciler aggregates cost and lifecycle data from all
// ManagedWorkloads into a cluster-scoped HybernateReport singleton.
type HybernateReportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hybernate.io,resources=hybernatereports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hybernate.io,resources=hybernatereports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hybernate.io,resources=hybernatereports/finalizers,verbs=update
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads,verbs=list;watch

func (r *HybernateReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var workloads v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &workloads); err != nil {
		return ctrl.Result{}, err
	}

	var active, paused, destroyed int
	var totalSavings, totalCostWithout float64
	var totalCPU, totalMem, totalStorage float64
	var totalFreedCPU, totalFreedMem int64
	var totalFreedReplicas int32

	for i := range workloads.Items {
		w := &workloads.Items[i]

		switch w.Status.Phase {
		case v1alpha1.PhaseRunning, v1alpha1.PhaseIdle, v1alpha1.PhaseScaling,
			v1alpha1.PhaseCreating, v1alpha1.PhaseResuming:
			active++
		case v1alpha1.PhasePaused, v1alpha1.PhasePausing:
			paused++
		case v1alpha1.PhaseDestroyed, v1alpha1.PhaseDestroying:
			destroyed++
		}

		if w.Status.Cost == nil {
			continue
		}
		totalSavings += parseDollarAmount(w.Status.Cost.EstimatedMonthlySavings)
		totalCostWithout += parseDollarAmount(w.Status.Cost.EstimatedCostWithoutManagement)
		totalCPU += w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64()
		totalMem += w.Status.Cost.CurrentMonthMemoryHours.AsApproximateFloat64()
		totalStorage += w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()

		if w.Status.Cost.ResourceReduction != nil {
			totalFreedCPU += w.Status.Cost.ResourceReduction.CPUMillis
			totalFreedMem += w.Status.Cost.ResourceReduction.MemoryBytes
			totalFreedReplicas += w.Status.Cost.ResourceReduction.Replicas
		}
	}

	totalEstimated := totalCostWithout - totalSavings

	// Emit Prometheus metrics.
	phaseCounts := map[v1alpha1.WorkloadPhase]int{}
	for i := range workloads.Items {
		phaseCounts[workloads.Items[i].Status.Phase]++
	}
	opmetrics.WorkloadsTotal.Reset()
	for phase, count := range phaseCounts {
		opmetrics.WorkloadsTotal.WithLabelValues(string(phase)).Set(float64(count))
	}
	opmetrics.ActiveWorkloads.Set(float64(active))
	opmetrics.PausedWorkloads.Set(float64(paused))
	opmetrics.DestroyedWorkloads.Set(float64(destroyed))
	opmetrics.CostEstimatedSavingsDollars.Set(totalSavings)
	opmetrics.CostEstimatedDollars.Set(totalEstimated)
	opmetrics.CostEstimatedWithoutManagementDollars.Set(totalCostWithout)
	opmetrics.ResourceReductionCPUMillis.Set(float64(totalFreedCPU))
	opmetrics.ResourceReductionMemoryBytes.Set(float64(totalFreedMem))
	opmetrics.CostCPUHours.Set(totalCPU)
	opmetrics.CostMemoryHours.Set(totalMem)
	opmetrics.CostStorageHours.Set(totalStorage)

	report := &v1alpha1.HybernateReport{}
	err := r.Get(ctx, client.ObjectKey{Name: reportSingletonName}, report)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	if err != nil {
		report = &v1alpha1.HybernateReport{
			ObjectMeta: metav1.ObjectMeta{Name: reportSingletonName},
		}
		if err := r.Create(ctx, report); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	report.Status = v1alpha1.HybernateReportStatus{
		TotalManagedWorkloads:          len(workloads.Items),
		ActiveWorkloads:                active,
		PausedWorkloads:                paused,
		DestroyedWorkloads:             destroyed,
		TotalCPUHours:                  *resource.NewMilliQuantity(int64(totalCPU*1000), resource.DecimalSI),
		TotalMemoryHours:               *resource.NewMilliQuantity(int64(totalMem*1000), resource.DecimalSI),
		TotalStorageHours:              *resource.NewMilliQuantity(int64(totalStorage*1000), resource.DecimalSI),
		EstimatedMonthlyCost:           cost.FormatDollars(totalEstimated),
		EstimatedTotalSavings:          cost.FormatDollars(totalSavings),
		EstimatedCostWithoutManagement: cost.FormatDollars(totalCostWithout),
		LastUpdated:                    &now,
	}

	if totalFreedCPU > 0 || totalFreedMem > 0 {
		report.Status.TotalResourceReduction = &v1alpha1.ResourceReduction{
			CPUMillis:   totalFreedCPU,
			MemoryBytes: totalFreedMem,
			Replicas:    totalFreedReplicas,
		}
	}

	if err := r.Status().Update(ctx, report); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("report updated",
		"total", len(workloads.Items), "active", active,
		"paused", paused, "destroyed", destroyed)

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *HybernateReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HybernateReport{}).
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: reportSingletonName}}}
			},
		)).
		Named("hybernatereport").
		Complete(r)
}
