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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
	"github.com/okedeji/hybernate/internal/discovery"
	"github.com/okedeji/hybernate/internal/export"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

const defaultScanInterval = 10 * time.Minute

// WorkloadPolicyReconciler reconciles a WorkloadPolicy object.
type WorkloadPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=hybernate.io,resources=workloadpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hybernate.io,resources=workloadpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hybernate.io,resources=workloadpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

// Reconcile scans the namespace for workloads, classifies them, and updates
// the WorkloadPolicy status with discovery results and savings estimates.
func (r *WorkloadPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy v1alpha1.WorkloadPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	th := thresholdsFromSpec(policy.Spec)
	kinds := policy.Spec.TargetKinds
	if len(kinds) == 0 {
		kinds = []v1alpha1.TargetKind{v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet}
	}

	scanner := discovery.NewScanner(r.Client)

	start := time.Now()
	result, err := scanner.Scan(ctx, policy.Namespace, kinds, th)
	elapsed := time.Since(start)
	opmetrics.DiscoveryScanDuration.Observe(elapsed.Seconds())

	if err != nil {
		log.Error(err, "scan failed", "namespace", policy.Namespace)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               "ScanSucceeded",
			Status:             metav1.ConditionFalse,
			Reason:             "ScanFailed",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			log.Error(statusErr, "updating status after scan failure")
		}
		return ctrl.Result{RequeueAfter: requeueInterval(policy.Spec)}, nil
	}

	// Auto-manage: create ManagedWorkload CRs for idle/wasteful workloads.
	if policy.Spec.Mode == v1alpha1.PolicyModeAutoManage {
		created := r.autoManage(ctx, &policy, result)
		if created > 0 {
			log.Info("auto-managed workloads", "namespace", policy.Namespace, "count", created)
			r.Recorder.Event(&policy, "Normal", "AutoManaged",
				fmt.Sprintf("Created %d ManagedWorkload CRs (dryRun: %t)", created, policy.Spec.DryRun))
		}
	}

	// Update metrics.
	opmetrics.DiscoveryWorkloads.WithLabelValues("Active").Set(float64(result.Summary.Active))
	opmetrics.DiscoveryWorkloads.WithLabelValues("Idle").Set(float64(result.Summary.Idle))
	opmetrics.DiscoveryWorkloads.WithLabelValues("Wasteful").Set(float64(result.Summary.Wasteful))

	now := metav1.Now()
	policy.Status.Summary = result.Summary
	policy.Status.Discovered = result.Discovered
	policy.Status.LastScanAt = &now

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "ScanSucceeded",
		Status:             metav1.ConditionTrue,
		Reason:             "ScanCompleted",
		Message:            fmt.Sprintf("Discovered %d workloads in %s", result.Summary.Total, elapsed.Round(time.Millisecond)),
		LastTransitionTime: now,
	})

	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	r.Recorder.Event(&policy, "Normal", "ScanCompleted",
		fmt.Sprintf("Discovered %d workloads: %d active, %d idle, %d wasteful (cost: %s, savings: %s)",
			result.Summary.Total, result.Summary.Active, result.Summary.Idle, result.Summary.Wasteful,
			result.Summary.EstimatedMonthlyCost, result.Summary.EstimatedPotentialSavings))

	return ctrl.Result{RequeueAfter: requeueInterval(policy.Spec)}, nil
}

func (r *WorkloadPolicyReconciler) autoManage(ctx context.Context, policy *v1alpha1.WorkloadPolicy, result *discovery.ScanResult) int {
	log := logf.FromContext(ctx)
	var created int

	for _, d := range result.Discovered {
		if d.Managed || d.Classification == v1alpha1.ClassificationActive {
			continue
		}

		mw := v1alpha1.ManagedWorkload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      export.ResourceName(d.Kind, d.Name),
				Namespace: policy.Namespace,
				Labels: map[string]string{
					v1alpha1.LabelAutoDiscovered: "true",
				},
				Annotations: map[string]string{
					v1alpha1.AnnotationWorkloadPolicy: policy.Name,
				},
			},
			Spec: v1alpha1.ManagedWorkloadSpec{
				Target: v1alpha1.WorkloadRef{
					Kind: d.Kind,
					Name: d.Name,
				},
				DryRun: policy.Spec.DryRun,
				Prediction: func() v1alpha1.PredictionSpec {
					if policy.Spec.Prediction != nil {
						return *policy.Spec.Prediction
					}
					return v1alpha1.PredictionSpec{}
				}(),
				IdlePolicy:     policy.Spec.IdlePolicy,
				ScalePolicy:    policy.Spec.ScalePolicy,
				Pause:          policy.Spec.Pause,
				Destroy:        policy.Spec.Destroy,
				CostTracking:   policy.Spec.CostTracking,
				ConflictAction: policy.Spec.ConflictAction,
			},
		}

		if err := r.Create(ctx, &mw); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			log.Error(err, "creating ManagedWorkload", "name", mw.Name)
			continue
		}
		opmetrics.DiscoveryAutoManaged.Inc()
		created++
	}

	return created
}

func thresholdsFromSpec(spec v1alpha1.WorkloadPolicySpec) discovery.Thresholds {
	th := discovery.DefaultThresholds()
	if spec.CPUIdleThreshold > 0 {
		th.IdlePercent = spec.CPUIdleThreshold
	}
	if spec.MemoryIdleThreshold > 0 {
		th.MemoryIdlePercent = spec.MemoryIdleThreshold
	}
	if spec.CPUWastefulThreshold > 0 {
		th.WastefulPercent = spec.CPUWastefulThreshold
	}
	if spec.MemoryWastefulThreshold > 0 {
		th.MemoryWastefulPercent = spec.MemoryWastefulThreshold
	}
	if spec.RightSizeTarget > 0 {
		th.RightSizePercent = spec.RightSizeTarget
	}
	if spec.Rates != nil {
		th.Rates = ratesToInternal(spec.Rates)
	}
	return th
}

func ratesToInternal(r *v1alpha1.CostRates) cost.Rates {
	rates := cost.DefaultRates
	if r.CPUPerHour != nil {
		rates.CPUPerHour = r.CPUPerHour.AsApproximateFloat64()
	}
	if r.MemoryPerHour != nil {
		rates.MemoryPerHour = r.MemoryPerHour.AsApproximateFloat64()
	}
	if r.StoragePerMonth != nil {
		rates.StoragePerMonth = r.StoragePerMonth.AsApproximateFloat64()
	}
	return rates
}

func requeueInterval(spec v1alpha1.WorkloadPolicySpec) time.Duration {
	if spec.ScanInterval != nil {
		return spec.ScanInterval.Duration
	}
	return defaultScanInterval
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkloadPolicy{}).
		Named("workloadpolicy").
		Complete(r)
}
