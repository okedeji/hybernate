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
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/discovery"
	"github.com/okedeji/hybernate/internal/gitops"
)

// relearnAfter is how long learned dependencies stand before they're
// learned again. A changed pod template or ignore annotation relearns them
// at once; this catches a ConfigMap changed since.
const relearnAfter = time.Hour

// A dependency learned from a wake is forgotten once no request has woken
// it for wakeLearnedFor, so a one-off call, such as someone's curl from a
// pod, doesn't tie two workloads together for good. Held awake and woken
// with its dependent meanwhile, a dependency in steady use isn't woken by
// its requests either, and is forgotten too: relearning it costs one cold
// start a fortnight, where not holding it would cost one each time it
// paused on its own clock. A confirmation is written at most once per
// wakeConfirmEvery, not on every wake.
const (
	wakeLearnedFor   = 14 * 24 * time.Hour
	wakeConfirmEvery = 24 * time.Hour
)

// learnDependencies finds the workloads this one's environment points at,
// the same way the scan does, and records them, so they're held and woken
// like dependsOn without anyone writing it. A paused workload isn't
// relearned until its pod template or ignore list changes: nothing it
// runs reads a changed ConfigMap meanwhile, and it's learned again once
// it wakes.
func (r *Reconciler) learnDependencies(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	target client.Object) error {
	ignored := ignoredDependencies(workload, target)
	from := learnedFrom(r.templateHashes.of(workload.UID, target), ignored)
	now := r.clockTime()
	if l := workload.Status.LearnedDependencies; l != nil && l.From == from &&
		(workload.Status.Phase == v1alpha1.PhasePaused || now.Sub(l.At.Time) < relearnAfter) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()

	spec := podTemplateOf(target).Spec
	configMaps, err := r.configMapsFor(ctx, workload.Namespace, spec)
	if err != nil {
		return err
	}
	byNamespace := map[string]map[string]corev1.Service{}
	var targets []discovery.Target
	for _, ns := range r.namespacesNamed(workload.Namespace, spec, configMaps) {
		found, err := r.addServices(ctx, ns, byNamespace)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		in, err := r.dependencyTargets(ctx, ns)
		if err != nil {
			return err
		}
		targets = append(targets, in...)
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
	// What wakes taught stays until it goes unconfirmed: the environment
	// can't confirm or deny it.
	stamped := false
	if before != nil {
		for _, d := range before.Dependencies {
			if d.Source != v1alpha1.LearnedFromWake || ignored[d.Namespace+"/"+d.Name] ||
				slices.ContainsFunc(learned, sameTarget(d)) {
				continue
			}
			if d.LastSeen == nil {
				d.LastSeen = &now
				stamped = true
			} else if now.Sub(d.LastSeen.Time) > wakeLearnedFor {
				continue
			}
			learned = append(learned, d)
		}
	}

	workload.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{From: from, At: now, Dependencies: learned}
	changed := before == nil || !sameDependencies(before.Dependencies, learned)
	if !changed && !stamped {
		return nil
	}
	// Written before it's announced: a write that fails is learned again,
	// and would otherwise be announced again.
	if err := r.Status().Update(ctx, workload); err != nil {
		return fmt.Errorf("recording learned dependencies: %w", err)
	}
	if !changed {
		return nil
	}
	r.emitEvent(workload, false, "Normal", ReasonDependenciesLearned, actionLearnDependencies,
		"%s", learnedMessage(learned))
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

// maxNamedNamespaces bounds how many namespaces an environment can have
// listed one by one. An environment naming more, such as a ConfigMap
// holding a whole config file, has the cluster listed once instead.
const maxNamedNamespaces = 32

// namespacesNamed are the namespaces the pod's environment could address a
// Service in: its own, which a bare name resolves in, and each one a host in
// its values names, as in postgres.data or postgres.data.svc.cluster.local.
// That is more than the addresses name, since a host needn't be a Service's,
// and never fewer. Nil means every namespace. In a namespaced install, only
// the namespaces it watches can be read.
func (r *Reconciler) namespacesNamed(own string, spec corev1.PodSpec, configMaps map[string]map[string]string) []string {
	named := map[string]bool{own: true}
	addHosts := func(value string) {
		for _, token := range strings.FieldsFunc(value, isAddressSeparator) {
			if ns, ok := namespaceOf(token); ok {
				named[ns] = true
			}
		}
	}
	for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, env := range c.Env {
			addHosts(env.Value)
		}
	}
	for _, data := range configMaps {
		for _, value := range data {
			addHosts(value)
		}
	}
	if len(r.WatchNamespaces) > 0 {
		for ns := range named {
			if !slices.Contains(r.WatchNamespaces, ns) {
				delete(named, ns)
			}
		}
	} else if len(named) > maxNamedNamespaces {
		return []string{metav1.NamespaceAll}
	}
	return slices.Sorted(maps.Keys(named))
}

// isAddressSeparator splits a value into the tokens an address can be, as
// the scan does: hosts are listed with commas, semicolons or spaces, and
// quoted in config files.
func isAddressSeparator(r rune) bool {
	return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\''
}

// namespaceOf is the namespace a host in token names, as cluster DNS reads
// one: service.namespace, or pod.service.namespace for a headless
// Service's pod, either followed by .svc and the cluster domain. The URL's
// scheme, credentials, port and path are left out.
func namespaceOf(token string) (string, bool) {
	if _, rest, ok := strings.Cut(token, "://"); ok {
		token = rest
	}
	if i := strings.IndexAny(token, "/?#"); i >= 0 {
		token = token[:i]
	}
	if i := strings.LastIndex(token, "@"); i >= 0 {
		token = token[i+1:]
	}
	host, _, _ := strings.Cut(token, ":")
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if i := strings.Index(host+".", ".svc."); i >= 0 {
		host = host[:i]
	}
	if n := strings.Count(host, "."); n < 1 || n > 2 {
		return "", false
	}
	ns := host[strings.LastIndex(host, ".")+1:]
	return ns, len(validation.IsDNS1123Label(ns)) == 0
}

// addServices adds the Services in a namespace to byNamespace, and reports
// whether there were any. They're read from the cache without copying:
// dependencies are learned only by reading them.
func (r *Reconciler) addServices(ctx context.Context, namespace string,
	byNamespace map[string]map[string]corev1.Service) (bool, error) {
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(namespace), client.UnsafeDisableDeepCopy); err != nil {
		return false, fmt.Errorf("listing Services in %q: %w", namespace, err)
	}
	for _, svc := range services.Items {
		if byNamespace[svc.Namespace] == nil {
			byNamespace[svc.Namespace] = map[string]corev1.Service{}
		}
		byNamespace[svc.Namespace][svc.Name] = svc
	}
	return len(services.Items) > 0, nil
}

// dependencyTargets are the Deployments and StatefulSets in a namespace a
// dependency can resolve to.
func (r *Reconciler) dependencyTargets(ctx context.Context, namespace string) ([]discovery.Target, error) {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(namespace), client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing Deployments in %q: %w", namespace, err)
	}
	var statefulSets appsv1.StatefulSetList
	if err := r.List(ctx, &statefulSets, client.InNamespace(namespace), client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("listing StatefulSets in %q: %w", namespace, err)
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
func learnedFrom(template string, ignored map[string]bool) string {
	names := make([]string, 0, len(ignored))
	for name := range ignored {
		names = append(names, name)
	}
	slices.Sort(names)
	sum := sha256.Sum256([]byte(template + "\n" + strings.Join(names, ",")))
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

// wakeLearnTimeout bounds learning from a wake, which runs as the workload
// wakes, with the request that woke it waiting. A link not learned in time
// is learned from a later wake.
const wakeLearnTimeout = 2 * time.Second

// learnFromWake learns that the workload whose request woke this one
// depends on it. Only a workload Hybernate manages counts: a request through
// an ingress controller comes from the controller's pod, which depends on
// nothing. Only an address the doorman stamped counts, and only a sender in
// a namespace this workload's allows dependents from.
func (r *Reconciler) learnFromWake(ctx context.Context, workload *v1alpha1.ManagedWorkload) error {
	if workload.Annotations[v1alpha1.AnnotationLastRequestFrom] == "" ||
		wakeSource(workload) != v1alpha1.ActivitySourceRequest {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, wakeLearnTimeout)
	defer cancel()

	ip, err := r.stampedByDoorman(ctx, workload)
	if err != nil || ip == "" {
		return err
	}
	namespaces, err := r.senderNamespaces(ctx, workload.Namespace)
	if err != nil {
		return err
	}
	pod, found, err := r.podAt(ctx, ip, namespaces)
	if err != nil || !found {
		return err
	}
	dependent, target, found, err := r.managing(ctx, pod)
	if err != nil || !found || dependent.UID == workload.UID {
		return err
	}
	now := r.clockTime()
	learned := v1alpha1.LearnedDependency{Namespace: workload.Namespace, Kind: workload.Spec.Target.Kind,
		Name: workload.Spec.Target.Name, Source: v1alpha1.LearnedFromWake, LastSeen: &now}
	if ignoredDependencies(dependent, target)[learned.Namespace+"/"+learned.Name] {
		return nil
	}
	l := dependent.Status.LearnedDependencies
	if i := slices.IndexFunc(learnedList(l), sameTarget(learned)); i >= 0 {
		return r.confirmLearnedFromWake(ctx, dependent, i, now)
	}
	if l == nil {
		// No From matches a pod template, so the dependent still learns
		// from its environment on its next reconcile, keeping this.
		l = &v1alpha1.LearnedDependencies{At: now}
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

// confirmLearnedFromWake records that a request confirmed the dependent's
// i-th learned dependency again, which keeps one learned from a wake from
// being forgotten. One learned from the environment needs no confirming.
func (r *Reconciler) confirmLearnedFromWake(ctx context.Context, dependent *v1alpha1.ManagedWorkload, i int,
	now metav1.Time) error {
	d := &dependent.Status.LearnedDependencies.Dependencies[i]
	if d.Source != v1alpha1.LearnedFromWake || d.LastSeen != nil && now.Sub(d.LastSeen.Time) < wakeConfirmEvery {
		return nil
	}
	d.LastSeen = &now
	if err := r.Status().Update(ctx, dependent); err != nil {
		return fmt.Errorf("confirming that %s/%s depends on %s/%s: %w", dependent.Namespace, dependent.Name,
			d.Namespace, d.Name, err)
	}
	return nil
}

func learnedList(l *v1alpha1.LearnedDependencies) []v1alpha1.LearnedDependency {
	if l == nil {
		return nil
	}
	return l.Dependencies
}

// stampedByDoorman is the workload's last-request-from when the doorman set
// it, or "". Anyone who can patch a ManagedWorkload can set the annotation
// too, and a forged address would have Hybernate write a dependency into
// the ManagedWorkload of whatever pod has it. The doorman writes under
// Hybernate's field manager, which the API server records; the cache drops
// managed fields, so the workload is read past it.
func (r *Reconciler) stampedByDoorman(ctx context.Context, workload *v1alpha1.ManagedWorkload) (string, error) {
	reader := r.PodReader
	if reader == nil {
		reader = r.Client
	}
	var live v1alpha1.ManagedWorkload
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workload), &live); err != nil {
		return "", fmt.Errorf("reading who set %s: %w", v1alpha1.AnnotationLastRequestFrom, err)
	}
	writer, ok := gitops.FieldWriter(live.ManagedFields, "metadata", "annotations", v1alpha1.AnnotationLastRequestFrom)
	if !ok || writer.Manager != v1alpha1.FieldManager {
		return "", nil
	}
	return live.Annotations[v1alpha1.AnnotationLastRequestFrom], nil
}

// senderNamespaces are the namespaces a workload depending on one in
// namespace can be in: namespace itself, and those its
// hybernate.io/dependents-from allows, as far as Hybernate watches them.
// A search of every namespace is metav1.NamespaceAll.
func (r *Reconciler) senderNamespaces(ctx context.Context, namespace string) ([]string, error) {
	value, err := r.dependentsFromOf(ctx, namespace)
	if err != nil {
		return nil, err
	}
	allowed := []string{namespace}
	for entry := range strings.SplitSeq(value, ",") {
		switch entry = strings.TrimSpace(entry); entry {
		case "*":
			if len(r.WatchNamespaces) > 0 {
				return r.WatchNamespaces, nil
			}
			return []string{metav1.NamespaceAll}, nil
		case "", namespace:
		default:
			allowed = append(allowed, entry)
		}
	}
	if len(r.WatchNamespaces) > 0 {
		allowed = slices.DeleteFunc(allowed, func(ns string) bool { return !slices.Contains(r.WatchNamespaces, ns) })
	}
	return allowed, nil
}

// podAt is the pod with the IP in one of namespaces, and false for none,
// or for pods on the node's network, which share the node's IP and so
// can't be told apart.
func (r *Reconciler) podAt(ctx context.Context, ip string, namespaces []string) (*corev1.Pod, bool, error) {
	reader := r.PodReader
	if reader == nil {
		reader = r.Client
	}
	if len(namespaces) == 0 {
		return nil, false, nil
	}
	for _, ns := range namespaces {
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(ns), client.MatchingFields{podIPField: ip}); err != nil {
			return nil, false, fmt.Errorf("finding the pod at %s: %w", ip, err)
		}
		if p, ok := podWithIP(pods.Items, ip); ok {
			return p, true, nil
		}
	}
	if addr, err := netip.ParseAddr(ip); err != nil || addr.Unmap().Is4() {
		return nil, false, nil
	}
	// A dual-stack pod's IPv6 address, in a cluster whose pods are IPv4
	// first, isn't its status.podIP, and the API server matches
	// status.podIPs only as the whole list. Such a pod is found by reading
	// pods a page at a time. Anywhere else, an IPv6 address no pod has as
	// its status.podIP is no pod's, such as a node's, and reading every pod
	// on every wake would find nothing.
	secondary, err := secondaryIPv6(ctx, reader, namespaces[0])
	if err != nil || !secondary {
		return nil, false, err
	}
	pages := 0
	for _, ns := range namespaces {
		for page := ""; ; {
			if pages == maxPodPages {
				return nil, false, fmt.Errorf("finding the pod at %s: not among the first %d pods",
					ip, maxPodPages*podPageSize)
			}
			pages++
			var pods corev1.PodList
			if err := reader.List(ctx, &pods, client.InNamespace(ns), client.Limit(podPageSize),
				client.Continue(page)); err != nil {
				return nil, false, fmt.Errorf("finding the pod at %s: %w", ip, err)
			}
			if p, ok := podWithIP(pods.Items, ip); ok {
				return p, true, nil
			}
			if page = pods.Continue; page == "" {
				break
			}
		}
	}
	return nil, false, nil
}

// A search by IPv6 address reads podPageSize pods at once, and at most
// maxPodPages pages.
const (
	podPageSize = 500
	maxPodPages = 4
)

// ipSamplePods is how many pods are read to tell how pods' addresses are
// ordered.
const ipSamplePods = 20

// secondaryIPv6 reports whether pods in the namespace may have an IPv6
// address second, as in a dual-stack cluster whose pods are IPv4 first,
// judged from the first of a few pods that has addresses. With none that
// has, they may.
func secondaryIPv6(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.Limit(ipSamplePods)); err != nil {
		return false, fmt.Errorf("reading how pods' addresses are ordered: %w", err)
	}
	for i := range pods.Items {
		ips := pods.Items[i].Status.PodIPs
		if len(ips) == 0 {
			continue
		}
		first, err := netip.ParseAddr(ips[0].IP)
		if err != nil {
			continue
		}
		return first.Unmap().Is4() && len(ips) > 1, nil
	}
	return true, nil
}

// podWithIP is the pod in pods with the IP among its addresses that isn't
// on its node's network or being deleted.
func podWithIP(pods []corev1.Pod, ip string) (*corev1.Pod, bool) {
	for i := range pods {
		p := &pods[i]
		if p.Spec.HostNetwork || p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.PodIP == ip || slices.ContainsFunc(p.Status.PodIPs, func(a corev1.PodIP) bool { return a.IP == ip }) {
			return p, true
		}
	}
	return nil, false
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
