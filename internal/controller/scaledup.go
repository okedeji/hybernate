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

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
	"github.com/okedeji/hybernate/internal/gitops"
	"github.com/okedeji/hybernate/internal/metrics"
)

const conditionGitOpsConflict = "GitOpsConflict"

// gitOpsRetryAfter is how long Hybernate waits to pause again after a
// GitOps tool undid a pause. Pausing is also how it finds out the tool has
// been told to leave the replicas, so it keeps trying, at the cost of one
// restart each time until then.
const gitOpsRetryAfter = time.Hour

// wakeOnScaleUp treats a paused workload scaled up outside Hybernate as
// woken: whoever did it wants it running. A GitOps tool doing it means it
// sets the replicas from Git and will undo every pause, which the workload
// reports with the fix. A pause that holds clears that report.
func (r *Reconciler) wakeOnScaleUp(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) error {
	if workload.Status.Phase != v1alpha1.PhasePaused {
		return nil
	}
	if replicasFromTarget(target) == 0 {
		return r.clearGitOpsConflictIfHeld(ctx, workload)
	}
	// Just after a pause the cache can still hold the target as it was
	// before it, which looks just like a scale-up. Only the API server can
	// tell them apart.
	replicas, err := r.liveReplicas(ctx, target)
	if err != nil {
		return err
	}
	if replicas == 0 {
		return nil
	}
	// Hybernate's own resume scales it up too, and the cache can still hold
	// the workload as Paused once that has begun. Releasing KEDA then would
	// let it scale the workload back down while it starts.
	if phase, err := r.livePhase(ctx, workload); err != nil {
		return err
	} else if phase != v1alpha1.PhasePaused {
		return nil
	}

	// Someone else woke it, so it's only released, never scaled.
	if _, err := r.pauser.Restore(ctx, workload); err != nil {
		return fmt.Errorf("releasing a workload scaled up outside Hybernate: %w", err)
	}

	writer, found := gitops.ReplicasWriter(target.GetManagedFields())
	by := writer.Manager
	if !found {
		by = "unknown"
	}
	now := r.clockTime()
	workload.Status.LastScaledUp = &v1alpha1.ScaledUp{At: now, By: by, GitOps: string(writer.Tool), Replicas: replicas}
	r.resetActivity(workload, v1alpha1.ActivitySourceScaledUp)
	var conflict string
	if writer.FromGit() {
		conflict = fmt.Sprintf("%s set the replicas from Git, undoing the pause; Hybernate pauses it again from %s. %s",
			writer.Tool, now.Add(gitOpsRetryAfter).UTC().Format("15:04 UTC"), strings.Join(gitops.Fix(writer.Tool), " "))
		r.setCondition(workload, conditionGitOpsConflict, metav1.ConditionTrue, "PauseUndone", conflict)
	}

	if err := r.wakeDependencies(ctx, workload); err != nil {
		return err
	}
	if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ScaledUp"); err != nil {
		return err
	}

	log.FromContext(ctx).Info("paused workload scaled up outside Hybernate",
		"workload", workload.Name, "namespace", workload.Namespace, "replicas", replicas, "by", by)
	metrics.ExternalScaleUps.WithLabelValues(scaledUpBy(writer)).Inc()
	r.emitEvent(workload, false, "Normal", ReasonScaledUp, actionCheckReplicas,
		"scaled up to %d replicas by %s outside Hybernate; counted as activity", replicas, by)
	if conflict != "" {
		r.emitEvent(workload, false, "Warning", ReasonGitOpsConflict, actionCheckReplicas, "%s", conflict)
	}
	return nil
}

const (
	conditionScaledToZero = "ScaledToZero"

	ReasonScaledToZero = "ScaledToZero"
)

// scaledToZero reports whether a target Hybernate hasn't paused is at zero
// replicas: scaled there by a person, a pipeline, or KEDA scaling it to
// zero itself. Hybernate leaves such a target off. Pausing it would record
// zero replicas to restore, and waking it, on a request through the doorman
// or as Hybernate let go of it, would start a workload someone meant to be
// off. So it isn't paused, woken, or routed to the doorman, and a request
// to its Services fails as it would without Hybernate, until its replicas
// are set above zero again. One that leaves zero is counted as active, so
// it isn't paused the moment it's back.
func (r *Reconciler) scaledToZero(ctx context.Context, workload *v1alpha1.ManagedWorkload, target client.Object) (bool, error) {
	switch workload.Status.Phase {
	case v1alpha1.PhaseRunning, v1alpha1.PhaseIdle:
	default:
		return false, nil
	}
	atZero := replicasFromTarget(target) == 0
	if atZero {
		// Just after Hybernate hands a target back, the cache can still hold
		// it at zero.
		replicas, err := r.liveReplicas(ctx, target)
		if err != nil {
			return false, err
		}
		atZero = replicas == 0
	}
	if !atZero && meta.IsStatusConditionTrue(workload.Status.Conditions, conditionScaledToZero) {
		r.setCondition(workload, conditionScaledToZero, metav1.ConditionFalse, "HasReplicas", "")
		r.resetActivity(workload, v1alpha1.ActivitySourceScaledUp)
	}
	return atZero, nil
}

// reconcileScaledToZero leaves a target scaled to zero outside Hybernate
// where it is, and says so. Its cost goes on being counted: its claims
// still cost while it has no replicas.
func (r *Reconciler) reconcileScaledToZero(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	observed *v1alpha1.ManagedWorkloadStatus) (ctrl.Result, error) {
	began := !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionScaledToZero)
	r.setCondition(workload, conditionScaledToZero, metav1.ConditionTrue, "ScaledToZero",
		"scaled to zero outside Hybernate, so Hybernate leaves it off: it doesn't pause it, wake it, or route "+
			"requests for it until its replicas are set above zero")
	r.clearIdleVeto(workload)
	r.trackDryRun(workload)
	if workload.Status.Phase == v1alpha1.PhaseIdle {
		r.endWouldBePause(workload)
		if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ScaledToZero"); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		r.accumulateCost(ctx, workload)
		if err := r.persistStatus(ctx, workload, observed); err != nil {
			return ctrl.Result{}, err
		}
	}
	if began {
		r.emitEvent(workload, false, "Normal", ReasonScaledToZero, actionCheckReplicas,
			"scaled to zero outside Hybernate; left off, and not paused or woken until it has replicas again")
	}
	return ctrl.Result{RequeueAfter: targetRecheckInterval}, nil
}

// liveReplicas reads the target's replicas from the API server: the scale
// subresource is never served from the cache.
func (r *Reconciler) liveReplicas(ctx context.Context, target client.Object) (int32, error) {
	scale := &autoscalingv1.Scale{}
	if err := r.SubResource("scale").Get(ctx, target.DeepCopyObject().(client.Object), scale); err != nil {
		return 0, fmt.Errorf("reading the target's replicas: %w", err)
	}
	return scale.Spec.Replicas, nil
}

// livePhase reads the workload's phase from the API server, past the cache.
func (r *Reconciler) livePhase(ctx context.Context, workload *v1alpha1.ManagedWorkload) (v1alpha1.WorkloadPhase, error) {
	reader := r.PodReader
	if reader == nil {
		reader = r.Client
	}
	var live v1alpha1.ManagedWorkload
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workload), &live); err != nil {
		return "", fmt.Errorf("reading the workload's phase: %w", err)
	}
	return live.Status.Phase, nil
}

func scaledUpBy(w gitops.Writer) string {
	switch w.Tool {
	case gitops.ArgoCD:
		return "argo-cd"
	case gitops.Flux:
		return "flux"
	}
	return "other"
}

// gitOpsHold is when Hybernate may pause a workload whose last pause a
// GitOps tool undid, and true until then.
func gitOpsHold(workload *v1alpha1.ManagedWorkload, now time.Time) (time.Time, bool) {
	s := workload.Status.LastScaledUp
	if s == nil || !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionGitOpsConflict) {
		return time.Time{}, false
	}
	until := s.At.Add(gitOpsRetryAfter)
	return until, now.Before(until)
}

// clearGitOpsConflictIfHeld clears the conflict once a pause made after it
// has held for as long as Hybernate waits to retry: the tool now leaves the
// replicas alone.
func (r *Reconciler) clearGitOpsConflictIfHeld(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	pause := workload.Status.Pause
	if pause == nil || pause.PausedAt == nil || r.now().Sub(pause.PausedAt.Time) < gitOpsRetryAfter {
		return nil
	}
	tool, cleared := r.clearGitOpsConflict(workload)
	if !cleared {
		return nil
	}
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("clearing the GitOps conflict: %w", err)
	}
	r.announceGitOpsConflictResolved(workload, tool)
	return nil
}

// clearGitOpsConflict removes the conflict, which a pause holding shows is
// over, and returns the GitOps tool it was with, or false if there was none.
// Scaling up clears the pause, so any pause since began after the conflict.
func (r *Reconciler) clearGitOpsConflict(workload *v1alpha1.ManagedWorkload) (string, bool) {
	s := workload.Status.LastScaledUp
	if s == nil || !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionGitOpsConflict) {
		return "", false
	}
	meta.RemoveStatusCondition(&workload.Status.Conditions, conditionGitOpsConflict)
	return s.GitOps, true
}

func (r *Reconciler) announceGitOpsConflictResolved(workload *v1alpha1.ManagedWorkload, tool string) {
	r.emitEvent(workload, false, "Normal", ReasonGitOpsConflictResolved, actionCheckReplicas,
		"a pause held, so %s leaves the replicas to Hybernate now", tool)
}

const conditionAutoscaled = "Autoscaled"

// reportAutoscaler says what scales the workload while it runs, and how
// Hybernate pauses it alongside: an HPA stops at zero replicas, and KEDA is
// held at zero through its own annotation.
func (r *Reconciler) reportAutoscaler(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if r.autoscalers == nil {
		return nil
	}
	a, found, err := r.autoscalers.Find(ctx, workload.Namespace, workload.Spec.Target.Kind, workload.Spec.Target.Name)
	if err != nil {
		return fmt.Errorf("finding the workload's autoscaler: %w", err)
	}
	if !found {
		meta.RemoveStatusCondition(&workload.Status.Conditions, conditionAutoscaled)
		return nil
	}
	how := "Hybernate pauses it at zero, where the HPA stops scaling, and resumes it within that range"
	if a.Kind == autoscaler.KEDA {
		how = "Hybernate pauses it by having KEDA hold it at zero, and resumes it within that range"
	}
	r.setCondition(workload, conditionAutoscaled, metav1.ConditionTrue, string(a.Kind),
		fmt.Sprintf("%s %s scales it between %d and %d replicas; %s", a.Kind, a.Name, a.Min, a.Max, how))
	return nil
}
