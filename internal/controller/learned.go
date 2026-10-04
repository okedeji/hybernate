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
			Via: d.Via, Address: d.Address})
	}

	before := workload.Status.LearnedDependencies
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
		parts = append(parts, fmt.Sprintf("%s/%s (%s)", d.Namespace, d.Name, d.Via))
	}
	return "depends on " + strings.Join(parts, ", ") + ", found in its environment; held and woken with it"
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
