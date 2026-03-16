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
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
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
		totalSavings += parseDollarAmount(w.Status.Cost.MonthlySavings)
		totalCostWithout += parseDollarAmount(w.Status.Cost.CostWithoutManagement)
		totalCPU += w.Status.Cost.CurrentMonthCPUHours.AsApproximateFloat64()
		totalMem += w.Status.Cost.CurrentMonthMemoryHours.AsApproximateFloat64()
		totalStorage += w.Status.Cost.CurrentMonthStorageHours.AsApproximateFloat64()
	}

	totalEstimated := totalCostWithout - totalSavings

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
		TotalManagedWorkloads: len(workloads.Items),
		ActiveWorkloads:       active,
		PausedWorkloads:       paused,
		DestroyedWorkloads:    destroyed,
		TotalCPUHours:         *resource.NewMilliQuantity(int64(totalCPU*1000), resource.DecimalSI),
		TotalMemoryHours:      *resource.NewMilliQuantity(int64(totalMem*1000), resource.DecimalSI),
		TotalStorageHours:     *resource.NewMilliQuantity(int64(totalStorage*1000), resource.DecimalSI),
		EstimatedMonthlyCost:  cost.FormatDollars(totalEstimated),
		TotalMonthlySavings:   cost.FormatDollars(totalSavings),
		CostWithoutManagement: cost.FormatDollars(totalCostWithout),
		LastUpdated:           &now,
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
		Named("hybernatereport").
		Complete(r)
}
