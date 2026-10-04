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
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/lifecycle"
)

const (
	conditionHeldByDependents       = "HeldByDependents"
	conditionWaitingForDependencies = "WaitingForDependencies"
	conditionDependencyCycle        = "DependencyCycle"
	conditionDependencyNotFound     = "DependencyNotFound"

	// dependencyReadyCheckInterval is how often a resume waiting on a
	// dependency re-checks, as a fallback to the watch on the dependency.
	dependencyReadyCheckInterval = 10 * time.Second
)

// workloadID identifies a Deployment or StatefulSet across namespaces.
type workloadID struct {
	namespace string
	kind      v1alpha1.TargetKind
	name      string
}

func (id workloadID) String() string {
	return id.namespace + "/" + id.name
}

func targetID(workload *v1alpha1.ManagedWorkload) workloadID {
	return workloadID{namespace: workload.Namespace, kind: workload.Spec.Target.Kind, name: workload.Spec.Target.Name}
}

// dependencyID resolves a dependency, which defaults to the dependent's namespace.
func dependencyID(dependent *v1alpha1.ManagedWorkload, ref v1alpha1.DependencyRef) workloadID {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = dependent.Namespace
	}
	return workloadID{namespace: namespace, kind: ref.Kind, name: ref.Name}
}

// dependencyGraph is a snapshot of every ManagedWorkload in the cluster,
// indexed by the workload each one manages.
type dependencyGraph struct {
	all      []v1alpha1.ManagedWorkload
	byTarget map[workloadID]*v1alpha1.ManagedWorkload
}

func (r *Reconciler) loadDependencyGraph(ctx context.Context) (*dependencyGraph, error) {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing managed workloads: %w", err)
	}
	g := &dependencyGraph{all: list.Items, byTarget: make(map[workloadID]*v1alpha1.ManagedWorkload, len(list.Items))}
	for i := range g.all {
		w := &g.all[i]
		id := targetID(w)
		// When two ManagedWorkloads share a target, the duplicate check's
		// owner is the one that acts on it.
		if owner, ok := g.byTarget[id]; ok && claimsTargetFirst(owner, w) {
			continue
		}
		g.byTarget[id] = w
	}
	return g, nil
}

// dependents returns the ManagedWorkloads that list workload's target in
// dependsOn.
func (g *dependencyGraph) dependents(workload *v1alpha1.ManagedWorkload) []*v1alpha1.ManagedWorkload {
	id := targetID(workload)
	var out []*v1alpha1.ManagedWorkload
	for i := range g.all {
		w := &g.all[i]
		if w.UID == workload.UID {
			continue
		}
		for _, ref := range w.Spec.DependsOn {
			if dependencyID(w, ref) == id {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

// inCycle reports whether following dependsOn from workload leads back to it.
// In a cycle, each workload holds the other awake, so neither could pause.
func (g *dependencyGraph) inCycle(workload *v1alpha1.ManagedWorkload) bool {
	start := targetID(workload)
	visited := map[workloadID]bool{}
	var walk func(w *v1alpha1.ManagedWorkload) bool
	walk = func(w *v1alpha1.ManagedWorkload) bool {
		for _, ref := range w.Spec.DependsOn {
			id := dependencyID(w, ref)
			if id == start {
				return true
			}
			if visited[id] {
				continue
			}
			visited[id] = true
			if next, ok := g.byTarget[id]; ok && walk(next) {
				return true
			}
		}
		return false
	}
	return walk(workload)
}

// isAwake reports whether a workload is, or is about to be, running. Pausing
// counts: its pods may still be serving.
func isAwake(phase v1alpha1.WorkloadPhase) bool {
	switch phase {
	case v1alpha1.PhasePaused:
		return false
	default:
		return true
	}
}

// dependencyHold reports why a workload whose clock has run out must stay
// up anyway, and records it in conditions. A nil result means it may act.
func (r *Reconciler) dependencyHold(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return nil, err
	}

	if g.inCycle(workload) {
		msg := "dependsOn forms a cycle through this workload, so it won't pause until the cycle is removed"
		if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionDependencyCycle) {
			r.emitEvent(workload, workload.Spec.DryRun, "Warning", conditionDependencyCycle, actionEvaluateIdle, "%s", msg)
		}
		r.setCondition(workload, conditionDependencyCycle, metav1.ConditionTrue, conditionDependencyCycle, msg)
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}
	r.clearCondition(workload, conditionDependencyCycle, "NoCycle")

	var holders []string
	for _, d := range g.dependents(workload) {
		if isAwake(d.Status.Phase) {
			holders = append(holders, d.Namespace+"/"+d.Name)
		}
	}
	if len(holders) == 0 {
		r.clearCondition(workload, conditionHeldByDependents, "NoAwakeDependents")
		return nil, nil
	}

	slices.Sort(holders)
	msg := "kept awake for " + strings.Join(holders, ", ")
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionHeldByDependents) {
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", conditionHeldByDependents, actionEvaluateIdle,
			"idle, but %s", msg)
	}
	r.setCondition(workload, conditionHeldByDependents, metav1.ConditionTrue, "DependentsAwake", msg)
	return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
}

// clearCondition flips a condition to False if it's present, so a resolved
// problem is shown as resolved rather than disappearing.
func (r *Reconciler) clearCondition(workload *v1alpha1.ManagedWorkload, condType, reason string) {
	if meta.FindStatusCondition(workload.Status.Conditions, condType) != nil {
		r.setCondition(workload, condType, metav1.ConditionFalse, reason, "")
	}
}

// wakeDependencies stamps an activity annotation on each dependency that
// Hybernate manages, which wakes it through the annotation wake path. A
// dependency that's already awake gets its clock reset, since this workload
// is about to use it.
func (r *Reconciler) wakeDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if len(workload.Spec.DependsOn) == 0 {
		return nil
	}
	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return err
	}
	stamp := r.now().UTC().Format(time.RFC3339)
	for _, ref := range workload.Spec.DependsOn {
		dep, ok := g.byTarget[dependencyID(workload, ref)]
		if !ok || dep.UID == workload.UID {
			continue
		}
		patch := client.MergeFrom(dep.DeepCopy())
		if dep.Annotations == nil {
			dep.Annotations = map[string]string{}
		}
		dep.Annotations[v1alpha1.AnnotationLastActivity] = stamp
		if err := r.Patch(ctx, dep, patch); err != nil {
			return fmt.Errorf("waking dependency %s/%s: %w", dep.Namespace, dep.Name, err)
		}
	}
	return nil
}

// waitForDependencies holds a resume until every waitForReady dependency's
// pods are Ready. A dependency that doesn't exist doesn't block: that's
// reported by checkDependenciesExist instead.
func (r *Reconciler) waitForDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	var waiting []string
	for _, ref := range workload.Spec.DependsOn {
		if !ref.WaitForReady {
			continue
		}
		id := dependencyID(workload, ref)
		target, err := r.getWorkload(ctx, id)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking dependency %s: %w", id, err)
		}
		if !lifecycle.IsReady(target) {
			waiting = append(waiting, fmt.Sprintf("%s (%d ready)", id, lifecycle.ReadyReplicas(target)))
		}
	}

	if len(waiting) == 0 {
		r.clearCondition(workload, conditionWaitingForDependencies, "DependenciesReady")
		return nil, nil
	}
	// The resume path returns before the reconcile's single status write, so
	// write here, but only when the condition changes: this re-checks every
	// dependencyReadyCheckInterval.
	msg := "waiting for " + strings.Join(waiting, ", ")
	if prev := meta.FindStatusCondition(workload.Status.Conditions, conditionWaitingForDependencies); prev == nil ||
		prev.Status != metav1.ConditionTrue || prev.Message != msg {
		r.setCondition(workload, conditionWaitingForDependencies, metav1.ConditionTrue, "DependencyNotReady", msg)
		if err := r.Status().Update(ctx, workload); err != nil {
			return nil, fmt.Errorf("updating dependency wait condition: %w", err)
		}
	}
	return &ctrl.Result{RequeueAfter: dependencyReadyCheckInterval}, nil
}

// checkDependenciesExist reports dependencies whose workload doesn't exist.
// Only unmanaged ones are checked: a managed dependency reports a missing
// target through its own TargetAvailable condition.
func (r *Reconciler) checkDependenciesExist(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if len(workload.Spec.DependsOn) == 0 {
		r.clearCondition(workload, conditionDependencyNotFound, "NoDependencies")
		return nil
	}
	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return err
	}
	var missing []string
	for _, ref := range workload.Spec.DependsOn {
		id := dependencyID(workload, ref)
		if _, managed := g.byTarget[id]; managed {
			continue
		}
		if _, err := r.getWorkload(ctx, id); apierrors.IsNotFound(err) {
			missing = append(missing, string(id.kind)+" "+id.String())
		} else if err != nil {
			return fmt.Errorf("checking dependency %s: %w", id, err)
		}
	}
	if len(missing) == 0 {
		r.clearCondition(workload, conditionDependencyNotFound, "DependenciesFound")
		return nil
	}
	r.setCondition(workload, conditionDependencyNotFound, metav1.ConditionTrue, conditionDependencyNotFound,
		"not found: "+strings.Join(missing, ", "))
	return nil
}

// warnIfDependentsAwake notes when a manual pause overrides the
// dependency hold. The user's choice wins, but dependents may now fail.
func (r *Reconciler) warnIfDependentsAwake(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return err
	}
	var awake []string
	for _, d := range g.dependents(workload) {
		if isAwake(d.Status.Phase) {
			awake = append(awake, d.Namespace+"/"+d.Name)
		}
	}
	if len(awake) > 0 {
		slices.Sort(awake)
		r.emitEvent(workload, false, "Warning", "DependentsAwake", actionPause,
			"desiredState overrides the dependency hold while %s still depend on it", strings.Join(awake, ", "))
	}
	return nil
}

func (r *Reconciler) getWorkload(ctx context.Context, id workloadID) (client.Object, error) {
	var obj client.Object
	switch id.kind {
	case v1alpha1.TargetKindStatefulSet:
		obj = &appsv1.StatefulSet{}
	default:
		obj = &appsv1.Deployment{}
	}
	err := r.Get(ctx, types.NamespacedName{Namespace: id.namespace, Name: id.name}, obj)
	return obj, err
}
