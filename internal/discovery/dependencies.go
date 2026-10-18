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

package discovery

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Dependency is a workload another one's environment points at, through a
// Service in front of it.
type Dependency struct {
	Namespace string              `json:"namespace"`
	Kind      v1alpha1.TargetKind `json:"kind"`
	Name      string              `json:"name"`
	// Via is the environment variable, and Address the address found in
	// it as scheme://host:port only, so no credential in it is shown.
	Via     string `json:"via"`
	Address string `json:"address"`
	// Headless means the address is a headless Service, or one of its
	// pods, which connect straight to pods. The doorman can't hold those
	// connections, so only dependsOn wakes the dependency for them.
	Headless bool `json:"headless"`
	// Declared means the workload's ManagedWorkload already depends on it.
	Declared bool `json:"declared"`
	// Source is how it was found: in the workload's environment, or, once
	// Hybernate learns them, from a wake it saw the workload cause.
	Source DependencySource `json:"source"`
	// Connected means Hybernate has learned it for the workload's
	// ManagedWorkload, so it wakes and holds the dependency with the
	// workload.
	Connected bool `json:"connected,omitempty"`
}

// DependencySource is how a dependency was found.
type DependencySource string

const (
	SourceEnvironment DependencySource = "environment"
	SourceWake        DependencySource = "wake"
)

type workloadKey struct {
	namespace string
	kind      v1alpha1.TargetKind
	name      string
}

// workloadSource is what the dependency pass reads about a workload.
type workloadSource struct {
	template corev1.PodTemplateSpec
	managed  *v1alpha1.ManagedWorkload
}

// variable is one environment variable a workload's containers get.
type variable struct {
	name, value string
}

// findDependencies fills in each workload's dependencies from the addresses
// in its environment that name a Service in a scanned namespace, with
// configMaps the ConfigMaps each namespace's workloads take variables from,
// by namespace. It only matches what was read, so it costs as much as the
// workloads and Services in each namespace, not the cluster. It returns
// notes on what it couldn't see; Secrets are never read.
func findDependencies(ctx context.Context, workloads []Workload, sources map[workloadKey]workloadSource,
	services map[string]map[string]corev1.Service, configMaps map[string]map[string]map[string]string) []string {
	targets := indexTargets(sources)
	var fromSecrets, unresolved []string
	for i := range workloads {
		if ctx.Err() != nil {
			for _, w := range workloads[i:] {
				unresolved = append(unresolved, w.Namespace+"/"+w.Name)
			}
			break
		}
		w := &workloads[i]
		self := workloadKey{w.Namespace, w.Kind, w.Name}
		source, ok := sources[self]
		if !ok {
			continue
		}
		deps, usesSecrets := dependenciesOf(self, source.template.Spec, configMaps[w.Namespace], services, targets)
		if usesSecrets {
			fromSecrets = append(fromSecrets, w.Namespace+"/"+w.Name)
		}
		for _, d := range deps {
			target := workloadKey{d.Namespace, d.Kind, d.Name}
			d.Declared = declares(source.managed, w.Namespace, target)
			d.Connected = learned(source.managed, target)
			w.Dependencies = append(w.Dependencies, d)
		}
		w.Dependencies = append(w.Dependencies, learnedFromWakes(source.managed, w.Dependencies)...)
	}
	var notes []string
	if len(fromSecrets) > 0 {
		notes = append(notes, fmt.Sprintf("%s environment variables from Secrets, which the scan doesn't read, "+
			"so dependencies set there aren't found: %s",
			plural(len(fromSecrets), "workload takes", "workloads take"), listSome(fromSecrets)))
	}
	if len(unresolved) > 0 {
		notes = append(notes, fmt.Sprintf("the scan stopped before finding the dependencies of %s, so "+
			"none are listed for %s: %s", plural(len(unresolved), "workload", "workloads"),
			oneOrMany(len(unresolved), "it", "them"), listSome(unresolved)))
	}
	return notes
}

// readConfigMaps reads the ConfigMaps a namespace's pods take variables
// from, by name. One that doesn't exist is left out, as the pod would leave
// it out if optional. It stops at the first it isn't allowed to read, since
// the rest won't be either. Those listed across the cluster are taken from
// there.
func (s *Scanner) readConfigMaps(ctx context.Context, reads *clusterReads, namespace string, workloads []workload) (
	map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	if listed := reads.configMaps.byNamespace; listed != nil {
		for _, cm := range listed[namespace] {
			out[cm.Name] = cm.Data
		}
		return out, nil
	}
	names := make([]string, 0, len(workloads))
	for _, wl := range workloads {
		names = append(names, ConfigMapsReferenced(wl.template.Spec)...)
	}
	var failed error
	for _, name := range uniqueSorted(names) {
		var cm corev1.ConfigMap
		err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm)
		switch {
		case err == nil:
			out[name] = cm.Data
		case apierrors.IsNotFound(err):
		case apierrors.IsForbidden(err):
			return out, err
		case failed == nil:
			failed = fmt.Errorf("reading ConfigMap %s: %w", name, err)
		}
	}
	return out, failed
}

// targetIndex is the workloads a Service can select, by namespace, each
// namespace's sorted by name, so resolving an address only looks at the
// workloads in its Service's namespace.
type targetIndex map[string][]indexedTarget

type indexedTarget struct {
	key    workloadKey
	labels map[string]string
}

func indexTargets(sources map[workloadKey]workloadSource) targetIndex {
	index := targetIndex{}
	for key, source := range sources {
		index[key.namespace] = append(index[key.namespace], indexedTarget{key: key, labels: source.template.Labels})
	}
	for _, targets := range index {
		slices.SortFunc(targets, func(a, b indexedTarget) int {
			return cmp.Or(strings.Compare(a.key.name, b.key.name), strings.Compare(string(a.key.kind), string(b.key.kind)))
		})
	}
	return index
}

// Target is a workload a dependency can resolve to, by the labels on its
// pod template that a Service selects.
type Target struct {
	Namespace string
	Kind      v1alpha1.TargetKind
	Name      string
	Template  corev1.PodTemplateSpec
}

// DependenciesOf finds the workloads a workload's environment points at:
// the addresses in its variables, literal and from configMaps, that name a
// Service in services (by namespace, then name), followed to the targets
// it selects. It also says whether the workload takes variables from
// Secrets, which aren't read.
func DependenciesOf(namespace string, kind v1alpha1.TargetKind, name string, spec corev1.PodSpec,
	configMaps map[string]map[string]string, services map[string]map[string]corev1.Service, targets []Target) (
	[]Dependency, bool) {
	sources := make(map[workloadKey]workloadSource, len(targets))
	for _, t := range targets {
		sources[workloadKey{t.Namespace, t.Kind, t.Name}] = workloadSource{template: t.Template}
	}
	return dependenciesOf(workloadKey{namespace, kind, name}, spec, configMaps, services, indexTargets(sources))
}

func dependenciesOf(self workloadKey, spec corev1.PodSpec, configMaps map[string]map[string]string,
	services map[string]map[string]corev1.Service, targets targetIndex) ([]Dependency, bool) {
	vars, usesSecrets := environment(spec, configMaps)
	var deps []Dependency
	for _, v := range vars {
		for _, addr := range addresses(v.value) {
			for _, target := range resolve(addr, self.namespace, services, targets) {
				if target.key == self {
					continue
				}
				deps = addDependency(deps, Dependency{
					Namespace: target.key.namespace, Kind: target.key.kind, Name: target.key.name,
					Via: v.name, Address: addr.shown, Headless: target.headless, Source: SourceEnvironment,
				})
			}
		}
	}
	return deps, usesSecrets
}

// ConfigMapsReferenced are the ConfigMaps a pod's containers take variables
// from, so they can be read before DependenciesOf.
func ConfigMapsReferenced(spec corev1.PodSpec) []string {
	var names []string
	for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
		for _, from := range c.EnvFrom {
			if from.ConfigMapRef != nil {
				names = append(names, from.ConfigMapRef.Name)
			}
		}
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
				names = append(names, env.ValueFrom.ConfigMapKeyRef.Name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// environment returns the variables a pod's containers get from literal
// values and ConfigMaps, and whether any come from Secrets.
func environment(spec corev1.PodSpec, configMaps map[string]map[string]string) (vars []variable, usesSecrets bool) {
	containers := append(slices.Clone(spec.InitContainers), spec.Containers...)
	for _, c := range containers {
		for _, from := range c.EnvFrom {
			switch {
			case from.SecretRef != nil:
				usesSecrets = true
			case from.ConfigMapRef != nil:
				data := configMaps[from.ConfigMapRef.Name]
				keys := make([]string, 0, len(data))
				for k := range data {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				for _, k := range keys {
					vars = append(vars, variable{name: from.Prefix + k, value: data[k]})
				}
			}
		}
		for _, env := range c.Env {
			switch {
			case env.ValueFrom == nil:
				vars = append(vars, variable{name: env.Name, value: env.Value})
			case env.ValueFrom.SecretKeyRef != nil:
				usesSecrets = true
			case env.ValueFrom.ConfigMapKeyRef != nil:
				ref := env.ValueFrom.ConfigMapKeyRef
				if value, ok := configMaps[ref.Name][ref.Key]; ok {
					vars = append(vars, variable{name: env.Name, value: value})
				}
			}
		}
	}
	return vars, usesSecrets
}

// address is a host found in a variable's value, and the address as the
// report shows it: only its scheme, host and port, since a user, password,
// path or query string can each hold a credential.
type address struct {
	host, shown string
}

// addresses finds the hosts in a value: in URLs (jdbc: and lists of hosts
// included), host:port pairs, and DNS names. A bare word counts only in one
// of those forms, so MODE=api doesn't name a Service called api.
func addresses(value string) []address {
	var out []address
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\''
	}) {
		rest, scheme, shaped := token, "", false
		if i := strings.Index(rest, "://"); i >= 0 {
			scheme, rest, shaped = rest[:i+3], rest[i+3:], true
		}
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		host, port := rest, ""
		if h, p, ok := strings.Cut(rest, ":"); ok {
			if p == "" || strings.Trim(p, "0123456789") != "" {
				continue
			}
			host, port, shaped = h, ":"+p, true
		}
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if host == "" || len(validation.IsDNS1123Subdomain(host)) > 0 {
			continue
		}
		if !shaped && !strings.Contains(host, ".") {
			continue
		}
		if !urlScheme.MatchString(scheme) {
			scheme = ""
		}
		out = append(out, address{host: host, shown: scheme + host + port})
	}
	return out
}

// urlScheme matches a URL's scheme with its "://", including compound ones
// such as jdbc:postgresql://. Anything else before the "://" is left out of
// what's shown, as it could be a key=value pair holding a secret.
var urlScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*(:[a-zA-Z][a-zA-Z0-9+.-]*)*://$`)

type resolved struct {
	key      workloadKey
	headless bool
}

// resolve finds the workloads behind the Service an address names, by the
// forms cluster DNS gives it: service, service.namespace, and either with
// .svc and the cluster domain, and a headless Service's pod as
// pod.service, with the same suffixes.
func resolve(addr address, namespace string, services map[string]map[string]corev1.Service,
	targets targetIndex) []resolved {
	parts := inCluster(strings.Split(addr.host, "."))
	type candidate struct {
		namespace, service string
		pod                bool
	}
	var candidates []candidate
	switch len(parts) {
	case 1:
		candidates = []candidate{{namespace: namespace, service: parts[0]}}
	case 2:
		candidates = []candidate{{namespace: parts[1], service: parts[0]}, {namespace: namespace, service: parts[1], pod: true}}
	case 3:
		candidates = []candidate{{namespace: parts[2], service: parts[1], pod: true}}
	}
	for _, c := range candidates {
		svc, ok := services[c.namespace][c.service]
		if !ok || (c.pod && svc.Spec.ClusterIP != corev1.ClusterIPNone) {
			continue
		}
		return behind(svc, c.pod || svc.Spec.ClusterIP == corev1.ClusterIPNone, targets)
	}
	return nil
}

// inCluster cuts .svc and the cluster domain, whatever it is, off a host's
// labels, when the labels before .svc are service.namespace or
// pod.service.namespace, as only those are cluster DNS names. Any other
// host, such as api.svc.example.com, is left whole, and names no Service.
func inCluster(host []string) []string {
	for _, n := range []int{2, 3} {
		if len(host) > n && host[n] == "svc" {
			return host[:n]
		}
	}
	return host
}

// behind returns the workloads whose pods a Service selects, by name.
func behind(svc corev1.Service, headless bool, targets targetIndex) []resolved {
	if len(svc.Spec.Selector) == 0 {
		return nil
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector)
	var out []resolved
	for _, t := range targets[svc.Namespace] {
		if selector.Matches(labels.Set(t.labels)) {
			out = append(out, resolved{key: t.key, headless: headless})
		}
	}
	return out
}

// addDependency adds d unless the workload already has it, keeping a
// headless address as the evidence, since that's the one only dependsOn
// covers.
func addDependency(deps []Dependency, d Dependency) []Dependency {
	for i, have := range deps {
		if have.Namespace == d.Namespace && have.Kind == d.Kind && have.Name == d.Name {
			if d.Headless && !have.Headless {
				deps[i] = d
			}
			return deps
		}
	}
	return append(deps, d)
}

// learnedFromWakes are the dependencies Hybernate learned from requests the
// workload sent that woke them, which its environment doesn't show.
func learnedFromWakes(mw *v1alpha1.ManagedWorkload, found []Dependency) []Dependency {
	if mw == nil || mw.Status.LearnedDependencies == nil {
		return nil
	}
	var out []Dependency
	for _, d := range mw.Status.LearnedDependencies.Dependencies {
		if d.Source != v1alpha1.LearnedFromWake || slices.ContainsFunc(found, func(f Dependency) bool {
			return f.Namespace == d.Namespace && f.Kind == d.Kind && f.Name == d.Name
		}) {
			continue
		}
		out = append(out, Dependency{Namespace: d.Namespace, Kind: d.Kind, Name: d.Name, Source: SourceWake,
			Declared: declares(mw, mw.Namespace, workloadKey{d.Namespace, d.Kind, d.Name}), Connected: true})
	}
	return out
}

// learned says Hybernate learned the dependency for the workload, so it
// holds and wakes it.
func learned(mw *v1alpha1.ManagedWorkload, target workloadKey) bool {
	if mw == nil || mw.Status.LearnedDependencies == nil {
		return false
	}
	for _, d := range mw.Status.LearnedDependencies.Dependencies {
		if d.Namespace == target.namespace && d.Kind == target.kind && d.Name == target.name {
			return true
		}
	}
	return false
}

func declares(mw *v1alpha1.ManagedWorkload, namespace string, target workloadKey) bool {
	if mw == nil {
		return false
	}
	for _, dep := range mw.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = namespace
		}
		if ns == target.namespace && dep.Kind == target.kind && dep.Name == target.name {
			return true
		}
	}
	return false
}
