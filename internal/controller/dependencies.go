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
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// dependencyTimeout bounds each dependency step of a reconcile: a
	// ManagedWorkload list and a read or write per dependency.
	dependencyTimeout = 15 * time.Second
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

// dependencyGraph is a snapshot of every ManagedWorkload Hybernate can see,
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

// dependents returns the ManagedWorkloads that depend on workload's
// target, declared or learned.
func (g *dependencyGraph) dependents(workload *v1alpha1.ManagedWorkload) []*v1alpha1.ManagedWorkload {
	id := targetID(workload)
	var out []*v1alpha1.ManagedWorkload
	for i := range g.all {
		w := &g.all[i]
		if w.UID == workload.UID {
			continue
		}
		for _, ref := range dependencyRefs(w) {
			if dependencyID(w, ref) == id {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

func declaredRefs(w *v1alpha1.ManagedWorkload) []v1alpha1.DependencyRef {
	return w.Spec.DependsOn
}

// reaches reports whether following the edges from w leads to the workload
// to.
func (g *dependencyGraph) reaches(w *v1alpha1.ManagedWorkload, to workloadID,
	edges func(*v1alpha1.ManagedWorkload) []v1alpha1.DependencyRef) bool {
	visited := map[workloadID]bool{}
	queue := []*v1alpha1.ManagedWorkload{w}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, ref := range edges(next) {
			id := dependencyID(next, ref)
			if id == to {
				return true
			}
			if visited[id] {
				continue
			}
			visited[id] = true
			if managed, ok := g.byTarget[id]; ok {
				queue = append(queue, managed)
			}
		}
	}
	return false
}

// inCycle reports whether following dependsOn from workload leads back to
// it. In a cycle, each workload holds the other awake, so neither could
// pause. Learned dependencies don't count: see holds.
func (g *dependencyGraph) inCycle(workload *v1alpha1.ManagedWorkload) bool {
	return g.reaches(workload, targetID(workload), declaredRefs)
}

// holds reports whether dependent, while awake, keeps workload from
// pausing. Declaring it in dependsOn always does. Having only learned it
// doesn't when workload in turn depends on dependent, directly or through
// others: two services that call each other would otherwise learn a cycle
// and keep each other awake forever. Each pauses when idle instead, and
// waking either wakes the other.
func (g *dependencyGraph) holds(dependent, workload *v1alpha1.ManagedWorkload) bool {
	id := targetID(workload)
	if slices.ContainsFunc(dependent.Spec.DependsOn, func(ref v1alpha1.DependencyRef) bool {
		return dependencyID(dependent, ref) == id
	}) {
		return true
	}
	return !g.reaches(workload, targetID(dependent), dependencyRefs)
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
	h, err := r.heldByDependencies(ctx, workload)
	if err != nil {
		return nil, err
	}

	if h.cycle {
		if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionDependencyCycle) {
			r.emitEvent(workload, workload.Spec.DryRun, "Warning", conditionDependencyCycle, actionEvaluateIdle, "%s",
				h.message())
		}
		r.setCondition(workload, conditionDependencyCycle, metav1.ConditionTrue, conditionDependencyCycle, h.message())
		return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
	}
	r.clearCondition(workload, conditionDependencyCycle, "NoCycle")

	if len(h.holders) == 0 {
		r.clearCondition(workload, conditionHeldByDependents, "NoAwakeDependents")
		return nil, nil
	}
	if !meta.IsStatusConditionTrue(workload.Status.Conditions, conditionHeldByDependents) {
		r.emitEvent(workload, workload.Spec.DryRun, "Normal", conditionHeldByDependents, actionEvaluateIdle,
			"idle, but %s", h.message())
	}
	r.setCondition(workload, conditionHeldByDependents, metav1.ConditionTrue, "DependentsAwake", h.message())
	return &ctrl.Result{RequeueAfter: activityCheckInterval}, nil
}

// dependencyHolds is what keeps a workload from pausing for the workloads
// around it: a dependsOn cycle through it, or awake dependents.
type dependencyHolds struct {
	cycle   bool
	holders []string
}

func (h dependencyHolds) held() bool {
	return h.cycle || len(h.holders) > 0
}

// condition is the condition type, and the event reason, that reports h.
func (h dependencyHolds) condition() string {
	if h.cycle {
		return conditionDependencyCycle
	}
	return conditionHeldByDependents
}

func (h dependencyHolds) message() string {
	if h.cycle {
		return "dependsOn forms a cycle through this workload, so it won't pause until the cycle is removed"
	}
	return "kept awake for " + strings.Join(h.holders, ", ")
}

// heldByDependencies finds what keeps workload from pausing for the
// workloads around it, without recording it.
func (r *Reconciler) heldByDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload) (dependencyHolds, error) {
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()

	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return dependencyHolds{}, err
	}
	if g.inCycle(workload) {
		return dependencyHolds{cycle: true}, nil
	}
	var h dependencyHolds
	for _, d := range g.dependents(workload) {
		if isAwake(d.Status.Phase) && g.holds(d, workload) {
			h.holders = append(h.holders, d.Namespace+"/"+d.Name)
		}
	}
	slices.Sort(h.holders)
	return h, nil
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
// is about to use it. Every dependency is tried even when one fails.
func (r *Reconciler) wakeDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	refs := dependencyRefs(workload)
	if len(refs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()

	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ref := range refs {
		dep, ok := g.byTarget[dependencyID(workload, ref)]
		if !ok || dep.UID == workload.UID {
			continue
		}
		errs = append(errs, r.stampActivity(ctx, dep))
	}
	return errors.Join(errs...)
}

func (r *Reconciler) stampActivity(ctx context.Context, dep *v1alpha1.ManagedWorkload) error {
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[v1alpha1.AnnotationLastActivity] = r.now().UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, dep, patch); err != nil {
		return fmt.Errorf("waking dependency %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	return nil
}

// waitForDependencies holds a resume until every waitForReady dependency's
// pods are Ready. It doesn't wait for one that won't become Ready by
// waiting, which would leave this workload at zero for good: one that
// doesn't exist or can't be seen (reported by checkDependenciesExist), or
// one scaled to zero outside Hybernate. A managed one still paused is woken
// again, in case the first wake was missed.
func (r *Reconciler) waitForDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload) (*ctrl.Result, error) {
	refs := slices.DeleteFunc(slices.Clone(workload.Spec.DependsOn), func(ref v1alpha1.DependencyRef) bool {
		return !ref.WaitForReady
	})
	if len(refs) == 0 {
		r.clearCondition(workload, conditionWaitingForDependencies, "DependenciesReady")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()

	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return nil, err
	}
	var waiting, skipped []string
	for _, ref := range refs {
		id := dependencyID(workload, ref)
		target, state, err := r.getDependency(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("checking dependency %s: %w", id, err)
		}
		if state != dependencyFound || lifecycle.IsReady(target) {
			continue
		}
		dep := g.byTarget[id]
		if why := wontStart(dep, target); why != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", id, why))
			continue
		}
		if dep != nil && dep.UID != workload.UID && !isAwake(dep.Status.Phase) {
			if err := r.stampActivity(ctx, dep); err != nil {
				return nil, err
			}
		}
		waiting = append(waiting, fmt.Sprintf("%s (%d ready)", id, lifecycle.ReadyReplicas(target)))
	}

	if len(waiting) == 0 {
		if len(skipped) > 0 {
			r.setCondition(workload, conditionWaitingForDependencies, metav1.ConditionFalse, "DependencyWontStart",
				"not waiting for "+strings.Join(skipped, ", "))
		} else {
			r.clearCondition(workload, conditionWaitingForDependencies, "DependenciesReady")
		}
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

// wontStart says why a dependency that isn't Ready won't become Ready by
// waiting, or "" when it may. dep is its ManagedWorkload, if it has one.
func wontStart(dep *v1alpha1.ManagedWorkload, target client.Object) string {
	if dep != nil {
		switch dep.Status.Phase {
		case v1alpha1.PhasePaused, v1alpha1.PhasePausing, v1alpha1.PhaseResuming:
			return ""
		}
	}
	if replicasFromTarget(target) == 0 {
		return "it's scaled to zero"
	}
	return ""
}

// checkDependenciesExist reports dependencies whose workload doesn't exist
// or can't be seen. Only unmanaged ones are checked: a managed dependency
// reports a missing target through its own TargetAvailable condition. It
// never stops the lifecycle: a dependency that can't be checked is left out
// of the report.
func (r *Reconciler) checkDependenciesExist(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if len(workload.Spec.DependsOn) == 0 {
		r.clearCondition(workload, conditionDependencyNotFound, "NoDependencies")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()

	g, err := r.loadDependencyGraph(ctx)
	if err != nil {
		return err
	}
	var missing, hidden []string
	for _, ref := range workload.Spec.DependsOn {
		id := dependencyID(workload, ref)
		if _, managed := g.byTarget[id]; managed {
			continue
		}
		_, state, err := r.getDependency(ctx, id)
		if err != nil {
			log.FromContext(ctx).Info("couldn't check a dependency", "workload", workload.Name,
				"namespace", workload.Namespace, "dependency", id.String(), "error", err.Error())
			continue
		}
		name := string(id.kind) + " " + id.String()
		switch state {
		case dependencyMissing:
			missing = append(missing, name)
		case dependencyUnwatched:
			hidden = append(hidden, fmt.Sprintf("%s (Hybernate doesn't watch namespace %s)", name, id.namespace))
		case dependencyForbidden:
			hidden = append(hidden, fmt.Sprintf("%s (Hybernate isn't allowed to read it)", name))
		case dependencyFound:
		}
	}
	if len(missing) == 0 && len(hidden) == 0 {
		r.clearCondition(workload, conditionDependencyNotFound, "DependenciesFound")
		return nil
	}
	var parts []string
	reason := conditionDependencyNotFound
	if len(missing) > 0 {
		parts = append(parts, "not found: "+strings.Join(missing, ", "))
	} else {
		reason = "DependencyNotVisible"
	}
	if len(hidden) > 0 {
		parts = append(parts, "can't be seen, so it isn't held or woken: "+strings.Join(hidden, ", "))
	}
	r.setCondition(workload, conditionDependencyNotFound, metav1.ConditionTrue, reason, strings.Join(parts, "; "))
	return nil
}

// dependencyState is what reading a dependency's workload found.
type dependencyState int

const (
	dependencyFound dependencyState = iota
	dependencyMissing
	// dependencyUnwatched is a dependency in a namespace outside
	// watchNamespaces, which the operator's cache can't read at all.
	dependencyUnwatched
	dependencyForbidden
)

// getDependency reads a dependency's workload. One Hybernate can't see,
// because of watchNamespaces or RBAC, is reported as such rather than as an
// error, so it never stops the dependent's lifecycle.
func (r *Reconciler) getDependency(ctx context.Context, id workloadID) (client.Object, dependencyState, error) {
	if len(r.WatchNamespaces) > 0 && !slices.Contains(r.WatchNamespaces, id.namespace) {
		return nil, dependencyUnwatched, nil
	}
	target, err := r.getWorkload(ctx, id)
	switch {
	case err == nil:
		return target, dependencyFound, nil
	case apierrors.IsNotFound(err):
		return nil, dependencyMissing, nil
	case apierrors.IsForbidden(err):
		return nil, dependencyForbidden, nil
	}
	return nil, dependencyMissing, err
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
