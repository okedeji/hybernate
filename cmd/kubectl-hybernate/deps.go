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

package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func depsCmd(kube *kubeFlags) *cobra.Command {
	var namespace string
	timeout := time.Minute
	cmd := &cobra.Command{
		Use:   "deps NAME",
		Short: "Show what a workload depends on, and what depends on it",
		Long: `Deps shows the dependencies Hybernate holds and wakes with a ManagedWorkload,
in both directions: the workloads it depends on, and the ones that depend
on it. Each says where the link comes from, a dependsOn or what Hybernate
learned from the workload's environment, and what the other workload is
doing now.

NAME is the workload, as status shows it, such as api or statefulset/postgres,
or its ManagedWorkload's name.

Examples:
  # What postgres is kept awake for, and what api needs
  kubectl hybernate deps postgres -n preview-42
  kubectl hybernate deps api -n preview-42`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPositive("timeout", timeout); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			k8s, at, err := kube.client(ctx)
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}
			if namespace == "" {
				namespace = at.namespace
			}
			err = deps(ctx, k8s, client.ObjectKey{Namespace: namespace, Name: args[0]}, cmd.OutOrStdout())
			return timedOut(ctx, err, timeout)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the workload (defaults to the kubeconfig context's)")
	addTimeoutFlag(cmd, &timeout, "How long to wait for the cluster before giving up")
	return cmd
}

// depRef is a Deployment or StatefulSet, across namespaces.
type depRef struct {
	namespace string
	kind      v1alpha1.TargetKind
	name      string
}

func (r depRef) String() string { return r.namespace + "/" + r.name }

// link is one dependency, and where it comes from.
type link struct {
	ref     depRef
	from    string
	learned bool
}

// links are what a ManagedWorkload depends on: its dependsOn, then what
// Hybernate learned that dependsOn doesn't already name.
func links(w *v1alpha1.ManagedWorkload) []link {
	var out []link
	for _, d := range w.Spec.DependsOn {
		namespace := d.Namespace
		if namespace == "" {
			namespace = w.Namespace
		}
		from := "declared in dependsOn"
		if d.WaitForReady {
			from += ", waitForReady"
		}
		out = append(out, link{ref: depRef{namespace, d.Kind, d.Name}, from: from})
	}
	if l := w.Status.LearnedDependencies; l != nil {
		for _, d := range l.Dependencies {
			ref := depRef{d.Namespace, d.Kind, d.Name}
			if slices.ContainsFunc(out, func(have link) bool { return have.ref == ref }) {
				continue
			}
			from := "learned from " + d.Via
			if d.Source == v1alpha1.LearnedFromWake {
				from = "learned from a wake"
			}
			out = append(out, link{ref: ref, from: from, learned: true})
		}
	}
	return out
}

func targetOf(w *v1alpha1.ManagedWorkload) depRef {
	return depRef{w.Namespace, w.Spec.Target.Kind, w.Spec.Target.Name}
}

// deps prints what a workload depends on and what depends on it. key names
// the workload or its ManagedWorkload. Dependents can be in any namespace,
// so every ManagedWorkload is read; without access to them all, only the
// workload's own namespace is.
func deps(ctx context.Context, c client.Client, key client.ObjectKey, out io.Writer) error {
	w, err := findManaged(ctx, c, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	var all v1alpha1.ManagedWorkloadList
	partial := false
	if err := c.List(ctx, &all); apierrors.IsForbidden(err) {
		partial = true
		if err := c.List(ctx, &all, client.InNamespace(key.Namespace)); err != nil {
			return fmt.Errorf("listing ManagedWorkloads in %s: %w", key.Namespace, err)
		}
	} else if err != nil {
		return fmt.Errorf("listing ManagedWorkloads: %w", err)
	}
	byTarget := map[depRef]*v1alpha1.ManagedWorkload{}
	for i := range all.Items {
		byTarget[targetOf(&all.Items[i])] = &all.Items[i]
	}
	phase := func(ref depRef) string {
		m, ok := byTarget[ref]
		switch {
		case !ok:
			return "not managed"
		case m.Status.Phase == "":
			return "-"
		}
		return string(m.Status.Phase)
	}
	anyLearned := false

	self := targetOf(w)
	tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s (%s, %s)\n", self, self.kind, cmp.Or(string(w.Status.Phase), "-"))
	_, _ = fmt.Fprintln(tw, "Depends on:")
	needs := links(w)
	if len(needs) == 0 {
		_, _ = fmt.Fprintln(tw, "  nothing")
	}
	for _, l := range needs {
		anyLearned = anyLearned || l.learned
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", l.ref, l.from, phase(l.ref))
	}

	_, _ = fmt.Fprintln(tw, "Depended on by:")
	var dependents []string
	for i := range all.Items {
		d := &all.Items[i]
		if d.UID == w.UID {
			continue
		}
		for _, l := range links(d) {
			if l.ref == self {
				anyLearned = anyLearned || l.learned
				dependents = append(dependents, fmt.Sprintf("  %s\t%s\t%s", targetOf(d), l.from, phase(targetOf(d))))
			}
		}
	}
	slices.Sort(dependents)
	if len(dependents) == 0 {
		_, _ = fmt.Fprintln(tw, "  nothing")
	}
	for _, line := range dependents {
		_, _ = fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if partial {
		_, _ = fmt.Fprintf(out, "Only %s was read: your access doesn't allow listing ManagedWorkloads in every "+
			"namespace, so dependents elsewhere aren't shown.\n", key.Namespace)
	}
	if anyLearned {
		_, _ = fmt.Fprintln(out, "Learned links come from the dependent's environment or its requests; "+
			"hybernate.io/ignore-dependencies drops one.")
	}
	return nil
}
