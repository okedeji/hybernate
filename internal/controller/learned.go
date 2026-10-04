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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
)

// relearnAfter is how long learned dependencies stand before they're
// learned again. A changed pod template or ignore annotation relearns them
// at once; this catches a ConfigMap changed since.
const relearnAfter = time.Hour

// learnDependencies finds the workloads this one's environment points at,
// the same way the scan does, and records them, so they're held and woken
// like dependsOn without anyone writing it.
func (r *Reconciler) learnDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	target client.Object) error {
	ignored := ignoredDependencies(workload, target)
	from := learnedFrom(target, ignored)
	now := r.clockTime()
	if l := workload.Status.LearnedDependencies; l != nil && l.From == from && now.Sub(l.At.Time) < relearnAfter {
		return nil
	}

	spec := podTemplateOf(target).Spec
	configMaps, err := r.configMapsFor(ctx, workload.Namespace, spec)
	if err != nil {
		return err
	}
	var services corev1.ServiceList
	if err := r.List(ctx, &services); err != nil {
		return fmt.Errorf("listing Services: %w", err)
	}
	byNamespace := map[string]map[string]corev1.Service{}
	for _, svc := range services.Items {
		if byNamespace[svc.Namespace] == nil {
			byNamespace[svc.Namespace] = map[string]corev1.Service{}
		}
		byNamespace[svc.Namespace][svc.Name] = svc
	}
	targets, err := r.dependencyTargets(ctx)
	if err != nil {
		return err
	}

	found, _ := discovery.DependenciesOf(workload.Namespace, workload.Spec.Target.Kind, workload.Spec.Target.Name,
		spec, configMaps, byNamespace, targets)
	learned := make([]v1alpha1.LearnedDependency, 0, len(found))
	for _, d := range found {
		if ignored[d.Namespace+"/"+d.Name] {
			continue
		}
		learned = append(learned, v1alpha1.LearnedDependency{Namespace: d.Namespace, Kind: d.Kind, Name: d.Name,
			Source: v1alpha1.LearnedFromEnvironment, Via: d.Via, Address: d.Address})
	}
	before := workload.Status.LearnedDependencies
	// What wakes taught stays: the environment can't confirm or deny it.
	if before != nil {
		for _, d := range before.Dependencies {
			if d.Source == v1alpha1.LearnedFromWake && !ignored[d.Namespace+"/"+d.Name] &&
				!slices.ContainsFunc(learned, sameTarget(d)) {
				learned = append(learned, d)
			}
		}
	}

	if before == nil || !sameDependencies(before.Dependencies, learned) {
		r.emitEvent(workload, false, "Normal", ReasonDependenciesLearned, actionLearnDependencies,
			"%s", learnedMessage(learned))
	}
	workload.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{From: from, At: now, Dependencies: learned}
	return nil
}

// configMapsFor reads the ConfigMaps a pod takes variables from, one by one
// and uncached: caching every ConfigMap in the cluster to read a few would
// cost the operator more memory than anything else it holds.
func (r *Reconciler) configMapsFor(ctx context.Context, namespace string, spec corev1.PodSpec) (
	map[string]map[string]string, error) {
	reader := r.PodReader
	if reader == nil {
		reader = r.Client
	}
	out := map[string]map[string]string{}
	for _, name := range discovery.ConfigMapsReferenced(spec) {
		var cm corev1.ConfigMap
		err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cm)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading ConfigMap %s/%s: %w", namespace, name, err)
		}
		out[name] = cm.Data
	}
	return out, nil
}

// dependencyTargets are the Deployments and StatefulSets a dependency can
// resolve to.
func (r *Reconciler) dependencyTargets(ctx context.Context) ([]discovery.Target, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments); err != nil {
		return nil, fmt.Errorf("listing Deployments: %w", err)
	}
	var statefulSets appsv1.StatefulSetList
	if err := r.List(ctx, &statefulSets); err != nil {
		return nil, fmt.Errorf("listing StatefulSets: %w", err)
	}
	targets := make([]discovery.Target, 0, len(deployments.Items)+len(statefulSets.Items))
	for _, d := range deployments.Items {
		targets = append(targets, discovery.Target{Namespace: d.Namespace, Kind: v1alpha1.TargetKindDeployment,
			Name: d.Name, Template: d.Spec.Template})
	}
	for _, s := range statefulSets.Items {
		targets = append(targets, discovery.Target{Namespace: s.Namespace, Kind: v1alpha1.TargetKindStatefulSet,
			Name: s.Name, Template: s.Spec.Template})
	}
	return targets, nil
}

func podTemplateOf(target client.Object) corev1.PodTemplateSpec {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return t.Spec.Template
	case *appsv1.StatefulSet:
		return t.Spec.Template
	}
	return corev1.PodTemplateSpec{}
}

// ignoredDependencies are the dependencies hybernate.io/ignore-dependencies
// names on the workload or its ManagedWorkload, as namespace/name.
func ignoredDependencies(workload *v1alpha1.ManagedWorkload, target client.Object) map[string]bool {
	ignored := map[string]bool{}
	for _, raw := range []string{workload.Annotations[v1alpha1.AnnotationIgnoreDependencies],
		target.GetAnnotations()[v1alpha1.AnnotationIgnoreDependencies]} {
		for entry := range strings.SplitSeq(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if !strings.Contains(entry, "/") {
				entry = workload.Namespace + "/" + entry
			}
			ignored[entry] = true
		}
	}
	return ignored
}

// learnedFrom identifies the pod template and ignore list dependencies are
// learned from.
func learnedFrom(target client.Object, ignored map[string]bool) string {
	names := make([]string, 0, len(ignored))
	for name := range ignored {
		names = append(names, name)
	}
	slices.Sort(names)
	sum := sha256.Sum256([]byte(podTemplateHash(target) + "\n" + strings.Join(names, ",")))
	return hex.EncodeToString(sum[:8])
}

func sameTarget(d v1alpha1.LearnedDependency) func(v1alpha1.LearnedDependency) bool {
	return func(other v1alpha1.LearnedDependency) bool {
		return other.Namespace == d.Namespace && other.Kind == d.Kind && other.Name == d.Name
	}
}

func sameDependencies(a, b []v1alpha1.LearnedDependency) bool {
	return slices.EqualFunc(a, b, func(x, y v1alpha1.LearnedDependency) bool {
		return x.Namespace == y.Namespace && x.Kind == y.Kind && x.Name == y.Name
	})
}

func learnedMessage(learned []v1alpha1.LearnedDependency) string {
	if len(learned) == 0 {
		return "no dependencies found in its environment"
	}
	parts := make([]string, 0, len(learned))
	for _, d := range learned {
		how := d.Via
		if d.Source == v1alpha1.LearnedFromWake {
			how = "a request that woke it"
		}
		parts = append(parts, fmt.Sprintf("%s/%s (%s)", d.Namespace, d.Name, how))
	}
	return "depends on " + strings.Join(parts, ", ") + "; held and woken with it"
}

// dependencyRefs are the workloads a ManagedWorkload depends on: those its
// dependsOn names, and those Hybernate learned.
func dependencyRefs(workload *v1alpha1.ManagedWorkload) []v1alpha1.DependencyRef {
	refs := slices.Clone(workload.Spec.DependsOn)
	l := workload.Status.LearnedDependencies
	if l == nil {
		return refs
	}
	for _, d := range l.Dependencies {
		ref := v1alpha1.DependencyRef{Namespace: d.Namespace, Kind: d.Kind, Name: d.Name}
		if !slices.ContainsFunc(refs, func(have v1alpha1.DependencyRef) bool {
			return dependencyID(workload, have) == dependencyID(workload, ref)
		}) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// podIPField finds pods by their IP, which the API server indexes.
const podIPField = "status.podIP"

// learnFromWake learns that the workload whose request woke this one
// depends on it. Only a workload Hybernate manages counts: a request through
// an ingress controller comes from the controller's pod, which depends on
// nothing.
func (r *Reconciler) learnFromWake(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	ip := workload.Annotations[v1alpha1.AnnotationLastRequestFrom]
	if ip == "" || wakeSource(workload) != v1alpha1.ActivitySourceRequest {
		return nil
	}
	pod, found, err := r.podAt(ctx, ip)
	if err != nil || !found {
		return err
	}
	dependent, target, found, err := r.managing(ctx, pod)
	if err != nil || !found || dependent.UID == workload.UID {
		return err
	}
	learned := v1alpha1.LearnedDependency{Namespace: workload.Namespace, Kind: workload.Spec.Target.Kind,
		Name: workload.Spec.Target.Name, Source: v1alpha1.LearnedFromWake}
	if ignoredDependencies(dependent, target)[learned.Namespace+"/"+learned.Name] {
		return nil
	}
	l := dependent.Status.LearnedDependencies
	if l != nil && slices.ContainsFunc(l.Dependencies, sameTarget(learned)) {
		return nil
	}
	if l == nil {
		l = &v1alpha1.LearnedDependencies{}
		dependent.Status.LearnedDependencies = l
	}
	l.Dependencies = append(l.Dependencies, learned)
	if err := r.Status().Update(ctx, dependent); err != nil {
		return fmt.Errorf("recording that %s/%s depends on %s/%s: %w", dependent.Namespace, dependent.Name,
			learned.Namespace, learned.Name, err)
	}
	r.emitEvent(dependent, false, "Normal", ReasonDependenciesLearned, actionLearnDependencies,
		"depends on %s/%s, learned from a request it sent that woke it; held and woken with it",
		learned.Namespace, learned.Name)
	return nil
}

// podAt is the pod with the IP, and false for none, or for pods on the
// node's network, which share the node's IP and so can't be told apart.
func (r *Reconciler) podAt(ctx context.Context, ip string) (*corev1.Pod, bool, error) {
	reader := r.PodReader
	if reader == nil {
		reader = r.Client
	}
	// Listing pods across namespaces needs a ClusterRole, which a
	// namespaced install doesn't have; the watched namespaces are searched
	// one by one instead.
	namespaces := r.WatchNamespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, ns := range namespaces {
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(ns), client.MatchingFields{podIPField: ip}); err != nil {
			return nil, false, fmt.Errorf("finding the pod at %s: %w", ip, err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if !p.Spec.HostNetwork && p.DeletionTimestamp == nil {
				return p, true, nil
			}
		}
	}
	return nil, false, nil
}

// managing is the ManagedWorkload whose target the pod belongs to, and the
// target.
func (r *Reconciler) managing(ctx context.Context, pod *corev1.Pod) (*v1alpha1.ManagedWorkload, client.Object,
	bool, error) {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(pod.Namespace)); err != nil {
		return nil, nil, false, fmt.Errorf("listing managed workloads in %s: %w", pod.Namespace, err)
	}
	for i := range list.Items {
		mw := &list.Items[i]
		target, err := r.getWorkload(ctx, targetID(mw))
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, false, fmt.Errorf("getting the target of %s/%s: %w", mw.Namespace, mw.Name, err)
		}
		if selects(target, pod) {
			return mw, target, true, nil
		}
	}
	return nil, nil, false, nil
}

// selects says the workload's selector picks the pod.
func selects(target client.Object, pod *corev1.Pod) bool {
	var selector *metav1.LabelSelector
	switch t := target.(type) {
	case *appsv1.Deployment:
		selector = t.Spec.Selector
	case *appsv1.StatefulSet:
		selector = t.Spec.Selector
	}
	if selector == nil {
		return false
	}
	s, err := metav1.LabelSelectorAsSelector(selector)
	return err == nil && !s.Empty() && s.Matches(labels.Set(pod.Labels))
}
