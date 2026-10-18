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

// Package guard refuses a Helm uninstall, or an upgrade that stops
// Hybernate watching a namespace, while it would leave ManagedWorkloads
// with no operator to run their finalizers: their workloads would stay
// paused at 0 replicas, with their Services routed to the doorman.
package guard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Operation is what the guard is asked to let go ahead.
type Operation string

const (
	// Uninstall removes the operator from every namespace it watches.
	Uninstall Operation = "uninstall"
	// Upgrade replaces the operator with one that watches Guard.Watched,
	// which may leave out namespaces the running one watches.
	Upgrade Operation = "upgrade"
)

// ErrRefused means ManagedWorkloads would be left without an operator. The
// guard has already written which, and what to do, to its Out.
var ErrRefused = errors.New("ManagedWorkloads would be left without an operator")

const (
	pageSize    = 500
	listTimeout = 30 * time.Second
)

// Guard checks whether an operation would strand ManagedWorkloads.
type Guard struct {
	Reader client.Reader
	Out    io.Writer
	// Watched are the namespaces the operator works in once the operation
	// is done, or, for an uninstall, until then. Empty means every one.
	Watched []string
	// Wait is how long ManagedWorkloads being deleted get for the operator
	// to restore their workloads, before they count as left.
	Wait time.Duration
	Poll time.Duration
}

// Check returns nil when the operation strands no ManagedWorkload, and
// ErrRefused, having written them out, when it would. It waits for those
// being deleted first, so what it reports is what's left after the wait.
func (g *Guard) Check(ctx context.Context, op Operation) error {
	if op == Upgrade && len(g.Watched) == 0 {
		return nil
	}
	waited, cancel := context.WithTimeout(ctx, g.Wait)
	defer cancel()
	ticker := time.NewTicker(g.Poll)
	defer ticker.Stop()
	for {
		left, err := g.stranded(ctx, op)
		if meta.IsNoMatchError(err) {
			_, err = fmt.Fprintln(g.Out, "The ManagedWorkload CRD isn't installed, so no ManagedWorkload is left to guard.")
			return err
		}
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(left, deleting) {
			return g.report(op, left)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for ManagedWorkloads being deleted: %w", ctx.Err())
		case <-waited.Done():
			if ctx.Err() != nil {
				return fmt.Errorf("waiting for ManagedWorkloads being deleted: %w", ctx.Err())
			}
			return g.report(op, left)
		case <-ticker.C:
		}
	}
}

// stranded lists the ManagedWorkloads the operation would leave without
// an operator. Those the operator took on carry its finalizer; one it
// never reconciled has no workload to restore. An upgrade strands those
// outside the namespaces it will watch. An uninstall strands every one
// with the finalizer, wherever it is, since Helm guards with the last
// revision's values, which may be a refused upgrade's, and every one it
// watches, which may only be waiting for the operator.
func (g *Guard) stranded(ctx context.Context, op Operation) ([]metav1.PartialObjectMetadata, error) {
	items, err := g.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []metav1.PartialObjectMetadata
	for _, mw := range items {
		watched := len(g.Watched) == 0 || slices.Contains(g.Watched, mw.Namespace)
		held := controllerutil.ContainsFinalizer(&mw, v1alpha1.FinalizerCleanup)
		if (op == Uninstall && (watched || held)) || (op == Upgrade && !watched && held) {
			out = append(out, mw)
		}
	}
	return out, nil
}

// list reads only the ManagedWorkloads' metadata, a page at a time, since
// their status holds each one's forecast state.
func (g *Guard) list(ctx context.Context) ([]metav1.PartialObjectMetadata, error) {
	var out []metav1.PartialObjectMetadata
	var next string
	for {
		var page metav1.PartialObjectMetadataList
		page.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("ManagedWorkloadList"))
		opts := []client.ListOption{client.Limit(pageSize)}
		if next != "" {
			opts = append(opts, client.Continue(next))
		}
		listCtx, cancel := context.WithTimeout(ctx, listTimeout)
		err := g.Reader.List(listCtx, &page, opts...)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("listing ManagedWorkloads: %w", err)
		}
		out = append(out, page.Items...)
		if next = page.GetContinue(); next == "" {
			return out, nil
		}
	}
}

func deleting(mw metav1.PartialObjectMetadata) bool {
	return mw.DeletionTimestamp != nil
}

func (g *Guard) report(op Operation, left []metav1.PartialObjectMetadata) error {
	if len(left) == 0 {
		_, err := fmt.Fprintf(g.Out, "No ManagedWorkload would be left without an operator, so the %s can go ahead.\n", op)
		return err
	}
	stuck := slices.DeleteFunc(slices.Clone(left), func(mw metav1.PartialObjectMetadata) bool { return !deleting(mw) })
	kept := slices.DeleteFunc(slices.Clone(left), deleting)

	var b strings.Builder
	if len(stuck) > 0 {
		fmt.Fprintf(&b, stuckText, g.Wait, help[op])
		writeNames(&b, stuck)
	}
	if len(kept) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(leftText[op])
		writeNames(&b, kept)
		fmt.Fprintf(&b, stopText[op], help[op])
	}
	if _, err := io.WriteString(g.Out, b.String()); err != nil {
		return fmt.Errorf("writing the refusal: %w", err)
	}
	return ErrRefused
}

func writeNames(b *strings.Builder, items []metav1.PartialObjectMetadata) {
	slices.SortFunc(items, func(a, c metav1.PartialObjectMetadata) int {
		return strings.Compare(a.Namespace+"/"+a.Name, c.Namespace+"/"+c.Name)
	})
	for _, mw := range items {
		fmt.Fprintf(b, "  %s/%s\n", mw.Namespace, mw.Name)
	}
}

var help = map[Operation]string{
	Uninstall: "https://okedeji.io/hybernate/getting-started/installation/#uninstall",
	Upgrade:   "https://okedeji.io/hybernate/operations/upgrading/#narrowing-watchnamespaces",
}

const stuckText = `These ManagedWorkloads were being deleted, and the operator still hadn't let go of their
workloads after %s. If it isn't running, nothing will: skip this check with --no-hooks and
restore the workloads by hand, as %s says:
`

var leftText = map[Operation]string{
	Uninstall: `Hybernate refuses to uninstall while it manages workloads. Without the operator, these
ManagedWorkloads' workloads would stay paused at 0 replicas, with their Services still
routed to the doorman being deleted:
`,
	Upgrade: `Hybernate refuses this upgrade, which stops it watching namespaces where it manages
workloads. Without the operator there, these ManagedWorkloads' workloads would stay paused
at 0 replicas, with their Services still routed to the doorman:
`,
}

var stopText = map[Operation]string{
	Uninstall: `Stop managing them first, while the operator runs, so it scales each one back up:
  1. Remove the hybernate.io/managed label from these workloads and their namespaces.
  2. Delete the ManagedWorkloads you created yourself:
       kubectl delete managedworkloads --all --all-namespaces
Then uninstall again: helm uninstall, or delete the Argo CD Application. To uninstall
anyway, and restore the workloads by hand, see
%s
`,
	Upgrade: `Keep their namespaces in watchNamespaces, or stop managing them first, while the operator
still watches them, so it scales each one back up:
  1. Remove the hybernate.io/managed label from these workloads and their namespaces.
  2. Delete the ManagedWorkloads you created yourself, in each of their namespaces:
       kubectl delete managedworkloads --all --namespace <namespace>
Then upgrade again. To upgrade anyway, and restore the workloads by hand, see
%s
`,
}
