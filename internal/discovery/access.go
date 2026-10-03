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

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SystemNamespaces hold Kubernetes' own components, which are never idle
// workloads to pause.
var SystemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// ErrCantListNamespaces means the user can't list namespaces, so the scan
// needs them named.
var ErrCantListNamespaces = errors.New("can't list namespaces")

// Namespaces returns the namespaces to scan: those requested, or every
// namespace the user can see, less the excluded ones.
func Namespaces(ctx context.Context, c client.Client, requested, exclude []string) ([]string, error) {
	if len(requested) > 0 {
		return requested, nil
	}
	var list corev1.NamespaceList
	if err := c.List(ctx, &list); err != nil {
		if apierrors.IsForbidden(err) {
			return nil, ErrCantListNamespaces
		}
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	var out []string
	for _, ns := range list.Items {
		if !slices.Contains(exclude, ns.Name) {
			out = append(out, ns.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// AccessNotes checks, before scanning, what the user can't read that the
// scan would use, and says how each gap limits it. Anything it can't check
// is left for the scan itself to report.
func AccessNotes(ctx context.Context, c client.Client, namespace string) []string {
	checks := []struct {
		group, resource, note string
	}{
		{"", "pods", "can't read pods, so sidecars added at pod creation aren't priced"},
		{"metrics.k8s.io", "pods", "can't read pod metrics, so CPU can't be measured"},
	}
	var notes []string
	for _, check := range checks {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: "list", Group: check.group, Resource: check.resource,
			},
		}}
		if err := c.Create(ctx, review); err != nil {
			continue
		}
		if !review.Status.Allowed {
			notes = append(notes, check.note)
		}
	}
	return notes
}
