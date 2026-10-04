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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	replicas := replicasFromTarget(target)
	if replicas == 0 {
		return r.clearGitOpsConflictIfHeld(ctx, workload)
	}

	writer, found := gitops.ReplicasWriter(target.GetManagedFields())
	by := writer.Manager
	if !found {
		by = "unknown"
	}
	now := r.clockTime()
	workload.Status.LastScaledUp = &v1alpha1.ScaledUp{At: now, By: by, GitOps: string(writer.Tool), Replicas: replicas}
	workload.Status.Pause = nil
	r.resetActivity(workload, v1alpha1.ActivitySourceScaledUp)

	log.FromContext(ctx).Info("paused workload scaled up outside Hybernate",
		"workload", workload.Name, "namespace", workload.Namespace, "replicas", replicas, "by", by)
	metrics.ExternalScaleUps.WithLabelValues(scaledUpBy(writer)).Inc()
	r.emitEvent(workload, false, "Normal", ReasonScaledUp, actionCheckReplicas,
		"scaled up to %d replicas by %s outside Hybernate; counted as activity", replicas, by)
	if writer.FromGit() {
		msg := fmt.Sprintf("%s set the replicas from Git, undoing the pause; Hybernate pauses it again from %s. %s",
			writer.Tool, now.Add(gitOpsRetryAfter).UTC().Format("15:04 UTC"), strings.Join(gitops.Fix(writer.Tool), " "))
		r.setCondition(workload, conditionGitOpsConflict, metav1.ConditionTrue, "PauseUndone", msg)
		r.emitEvent(workload, false, "Warning", ReasonGitOpsConflict, actionCheckReplicas, "%s", msg)
	}

	if err := r.wakeDependencies(ctx, workload); err != nil {
		return err
	}
	if _, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "ScaledUp"); err != nil {
		return err
	}
	return nil
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
	if !r.clearGitOpsConflict(workload) {
		return nil
	}
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("clearing the GitOps conflict: %w", err)
	}
	return nil
}

// clearGitOpsConflict removes the conflict, which a pause holding shows is
// over, and says whether there was one. Scaling up clears the pause, so any
// pause since began after the conflict.
func (r *Reconciler) clearGitOpsConflict(workload *v1alpha1.ManagedWorkload) bool {
	s := workload.Status.LastScaledUp
	if s == nil || !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionGitOpsConflict) {
		return false
	}
	meta.RemoveStatusCondition(&workload.Status.Conditions, conditionGitOpsConflict)
	r.emitEvent(workload, false, "Normal", ReasonGitOpsConflictResolved, actionCheckReplicas,
		"a pause held, so %s leaves the replicas to Hybernate now", s.GitOps)
	return true
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
