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
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SystemNamespaces hold Kubernetes' own components, which are never idle
// workloads to pause.
var SystemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// ErrCantListNamespaces means the user can't list namespaces, so the scan
// needs them named.
var ErrCantListNamespaces = errors.New("can't list namespaces")

// Namespaces returns the namespaces to scan: those requested, each once, or
// every namespace the user can see, less the excluded ones.
func Namespaces(ctx context.Context, c client.Client, requested, exclude []string) ([]string, error) {
	if len(requested) > 0 {
		named := slices.DeleteFunc(slices.Clone(requested), func(ns string) bool { return ns == "" })
		return uniqueSorted(named), nil
	}
	var out []string
	err := listAll(ctx, c, func(list *corev1.NamespaceList) {
		for _, ns := range list.Items {
			if !slices.Contains(exclude, ns.Name) {
				out = append(out, ns.Name)
			}
		}
	})
	if apierrors.IsForbidden(err) {
		return nil, ErrCantListNamespaces
	}
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	slices.Sort(out)
	return out, nil
}

// readProblem is something the scan couldn't read in a namespace.
type readProblem struct {
	namespace string
	what      reading
	err       error
}

// reading is what the scan was reading when it hit a problem.
type reading string

const (
	readingWorkloads  reading = "workloads"
	readingPods       reading = "pods"
	readingMetrics    reading = "pod metrics"
	readingManaged    reading = "ManagedWorkloads"
	readingNamespace  reading = "namespace labels"
	readingServices   reading = "Services"
	readingConfigMaps reading = "ConfigMaps"
	readingHistory    reading = "CPU history"
)

// deniedConsequence says what the scan loses when the user's access doesn't
// let it read each thing.
var deniedConsequence = map[reading]string{
	readingWorkloads: "so they weren't scanned",
	readingPods: "so sidecars added at pod creation aren't priced, and pods are matched to workloads by " +
		"labels alone",
	readingMetrics:    "so CPU there couldn't be measured",
	readingManaged:    "so workloads Hybernate manages there are judged as if it didn't",
	readingNamespace:  "so a protected label on them isn't seen",
	readingServices:   "so dependencies on workloads there aren't found",
	readingConfigMaps: "so dependencies set in them aren't found",
}

// readingOrder is the order notes about each are given in, most limiting
// first.
var readingOrder = []reading{readingWorkloads, readingPods, readingMetrics, readingManaged, readingNamespace,
	readingServices, readingConfigMaps, readingHistory}

// problemNotes sums up what couldn't be read: one line for each thing the
// user's access denies, however many namespaces it's denied in, and one for
// everything that failed for another reason, which leaves the scan
// incomplete. named says the user named the namespaces, so being denied one,
// or one not existing, is a failure to scan what was asked for, not a limit
// of their access or a namespace deleted since it was listed.
func problemNotes(problems []readProblem, named bool) (notes []string, incomplete *Incomplete) {
	denied := map[reading][]string{}
	var failed, missing, deniedNamed, failedIn []string
	for _, p := range problems {
		if p.what == readingNamespace && apierrors.IsNotFound(p.err) {
			if named {
				missing = append(missing, p.namespace)
			}
			continue
		}
		if apierrors.IsForbidden(p.err) && p.what != readingHistory {
			denied[p.what] = append(denied[p.what], p.namespace)
			if named && p.what == readingWorkloads {
				deniedNamed = append(deniedNamed, p.namespace)
			}
			continue
		}
		failedIn = append(failedIn, p.namespace)
		failed = append(failed, fmt.Sprintf("%s in %s (%v)", p.what, p.namespace, p.err))
	}
	for _, what := range readingOrder {
		namespaces := uniqueSorted(denied[what])
		if len(namespaces) == 0 {
			continue
		}
		notes = append(notes, fmt.Sprintf("your access doesn't allow reading %s in %s, %s: %s", what,
			plural(len(namespaces), "namespace", "namespaces"), deniedConsequence[what], listSome(namespaces)))
	}
	if len(missing) > 0 {
		notes = append(notes, fmt.Sprintf("%s to scan doesn't exist: %s",
			plural(len(missing), "namespace named", "namespaces named"), listSome(uniqueSorted(missing))))
	}
	if len(failed) > 0 {
		notes = append(notes, fmt.Sprintf("the scan is incomplete: it couldn't read %s", listSome(failed)))
	}
	if len(missing) == 0 && len(deniedNamed) == 0 && len(failedIn) == 0 {
		return notes, nil
	}
	return notes, &Incomplete{Missing: uniqueSorted(missing), Denied: uniqueSorted(deniedNamed),
		Failed: uniqueSorted(failedIn)}
}

// isUnavailable says an API isn't served at all: its group isn't
// registered, or the server behind it, such as metrics-server, is down.
func isUnavailable(err error) bool {
	return isNotServed(err) || apierrors.IsServiceUnavailable(err)
}

// isNotServed says an API isn't registered, as when the CRD that defines it
// isn't installed.
func isNotServed(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}
