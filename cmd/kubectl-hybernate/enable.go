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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/gitops"
)

// errManagedByGit means the change belongs in Git, where a GitOps tool would
// otherwise put the old value straight back.
var errManagedByGit = errors.New("managed from Git")

// dryRunOff is the hybernate.io/dry-run value that turns dry-run off.
const dryRunOff = "false"

type enableOptions struct {
	all     bool
	force   bool
	timeout time.Duration
}

func enableCmd(kube *kubeFlags) *cobra.Command {
	opts := enableOptions{timeout: time.Minute}
	var namespace string
	cmd := &cobra.Command{
		Use:   "enable [KIND/]NAME",
		Short: "Stop measuring a workload in dry-run and let Hybernate pause it",
		Long: `Enable ends dry-run for a workload, so Hybernate starts pausing it while
idle. For a workload opted in with the hybernate.io/managed label, it sets the
workload's hybernate.io/dry-run annotation to "false", which wins over its
namespace's annotation and the cluster's default. For a ManagedWorkload
written by hand, it sets spec.dryRun to false.

With --all, it ends dry-run for every workload in the namespace. When the
namespace's annotation or the cluster's default keeps any of them in
dry-run, it sets the namespace's annotation to "false" and drops the
workloads' own; otherwise it sets each workload's annotation to "false".
When your access doesn't allow changing the namespace, it leaves the
namespace alone and sets each workload's annotation to "false" instead.
ManagedWorkloads written by hand get spec.dryRun false.

It changes nothing, and fails, for a workload Hybernate won't manage: one in
a protected namespace, one ignored or opted out by its own label, or one no
ManagedWorkload manages yet.

A setting a GitOps tool (Argo CD, Flux, Fleet, kapp or Kluctl) applies from
Git, as the object's managed fields record, isn't changed: the tool would
put the old value back. Enable says what to change there instead. One Helm
set is changed, with a warning that the next helm upgrade puts it back.

Examples:
  # Start pausing a Deployment
  kubectl hybernate enable checkout-api -n preview-42

  # A StatefulSet
  kubectl hybernate enable statefulset/postgres -n preview-42

  # Everything in a namespace
  kubectl hybernate enable --all -n preview-42`,
		Args: func(_ *cobra.Command, args []string) error {
			if opts.all != (len(args) == 0) {
				return errors.New("name one workload, or pass --all for the namespace")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPositive("timeout", opts.timeout); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()
			k8s, at, err := kube.client(ctx)
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}
			if namespace == "" {
				namespace = at.namespace
			}
			if opts.all {
				err = enableNamespace(ctx, k8s, namespace, opts, cmd.OutOrStdout())
			} else {
				err = enableWorkload(ctx, k8s, namespace, args[0], opts, cmd.OutOrStdout())
			}
			return timedOut(ctx, err, opts.timeout)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the workload (defaults to the kubeconfig context's)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Enable every workload in the namespace")
	cmd.Flags().BoolVar(&opts.force, "force", false,
		"Change the cluster even when a GitOps tool applies the setting from Git")
	addTimeoutFlag(cmd, &opts.timeout, "How long to wait for the cluster before giving up")
	return cmd
}

const (
	kindDeployment  = "deployment"
	kindStatefulSet = "statefulset"
)

// workloadArg reads "name", "deployment/name", or "statefulset/name".
func workloadArg(arg string) (kind, name string, err error) {
	kind, name, found := strings.Cut(arg, "/")
	if !found {
		return "", arg, nil
	}
	switch strings.ToLower(kind) {
	case kindDeployment, "deploy", "deployments":
		return kindDeployment, name, nil
	case kindStatefulSet, "sts", "statefulsets":
		return kindStatefulSet, name, nil
	}
	return "", "", fmt.Errorf("%q isn't a deployment or statefulset", kind)
}

func getWorkload(ctx context.Context, c client.Client, namespace, arg string) (client.Object, error) {
	kind, name, err := workloadArg(arg)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	var d appsv1.Deployment
	var s appsv1.StatefulSet
	switch kind {
	case kindDeployment:
		return &d, wrapNotFound(c.Get(ctx, key, &d), kindDeployment, key)
	case kindStatefulSet:
		return &s, wrapNotFound(c.Get(ctx, key, &s), kindStatefulSet, key)
	}
	dErr := c.Get(ctx, key, &d)
	if dErr != nil && !apierrors.IsNotFound(dErr) {
		return nil, wrapNotFound(dErr, kindDeployment, key)
	}
	sErr := c.Get(ctx, key, &s)
	if sErr != nil && !apierrors.IsNotFound(sErr) {
		return nil, wrapNotFound(sErr, kindStatefulSet, key)
	}
	switch {
	case dErr == nil && sErr == nil:
		return nil, fmt.Errorf("%s in %s names both deployment/%s and statefulset/%s; name one by its kind, such as "+
			"statefulset/%s", name, namespace, name, name, name)
	case dErr == nil:
		return &d, nil
	case sErr == nil:
		return &s, nil
	}
	return nil, fmt.Errorf("no Deployment or StatefulSet in %s is named %s: %w", namespace, name, sErr)
}

func wrapNotFound(err error, kind string, key client.ObjectKey) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("getting %s %s: %w", kind, key, err)
}

func enableWorkload(
	ctx context.Context, c client.Client, namespace, arg string, opts enableOptions, out io.Writer,
) error {
	obj, err := getWorkload(ctx, c, namespace, arg)
	if err != nil {
		return err
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("getting namespace %s: %w", namespace, err)
	}
	if err := checkProtected(&ns); err != nil {
		return err
	}
	managed, err := managedByTarget(ctx, c, namespace)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName())
	mw := managed.byTarget[workloadKey(obj)]
	if why := wontPause(mw); why != "" {
		return fmt.Errorf("%s isn't changed: Hybernate never pauses it, as %s", ref, why)
	}
	if mw != nil && mw.Labels[v1alpha1.LabelFromLabel] != v1alpha1.True {
		return enableHandWritten(ctx, c, mw, ref, opts, out)
	}
	if err := checkOptedIn(obj, &ns, ref); err != nil {
		return err
	}
	if managed.readable && mw == nil {
		return notPickedUp(ref, namespace)
	}
	if !inDryRun(mw, obj, &ns) {
		_, _ = fmt.Fprintf(out, "%s isn't in dry-run; Hybernate already pauses it while idle\n", ref)
		return nil
	}
	applied := appliedBy(obj, dryRunAnnotationField)
	if applied.gitOps != "" && !opts.force {
		_, _ = fmt.Fprintf(out, "%s is managed by %s, which would undo a change made here. In its manifest:\n", ref,
			applied.gitOps)
		_, _ = fmt.Fprintf(out, "  set the annotation  %s: \"false\"\n", v1alpha1.AnnotationDryRun)
		_, _ = fmt.Fprintln(out, "Or pass --force to change the cluster anyway.")
		return errManagedByGit
	}
	if err := setAnnotation(ctx, c, obj, v1alpha1.AnnotationDryRun, dryRunOff); err != nil {
		return err
	}
	if managed.readable {
		_, _ = fmt.Fprintf(out, "%s: dry-run ended, Hybernate will pause it while idle\n", ref)
	} else {
		_, _ = fmt.Fprintf(out, "%s: dry-run annotation set to \"false\". %s\n", ref, unreadableNote(namespace))
	}
	if applied.helm {
		_, _ = fmt.Fprintln(out, helmWarning(ref, v1alpha1.AnnotationDryRun+" annotation"))
	}
	return nil
}

// notPickedUp is an opted-in workload the operator hasn't made a
// ManagedWorkload for, so ending its dry-run would change nothing.
func notPickedUp(ref, namespace string) error {
	return fmt.Errorf("%s is opted in, but Hybernate hasn't picked it up: no ManagedWorkload in %s manages it. "+
		"The operator may not be running, may not watch %s (its watchNamespaces), or may protect it; if you "+
		"labelled it just now, run this again in a few seconds", ref, namespace, namespace)
}

// unreadableNote says what enable couldn't check without reading
// ManagedWorkloads.
func unreadableNote(namespace string) string {
	return fmt.Sprintf("Your access doesn't allow listing ManagedWorkloads in %s, so enable went by the "+
		"annotations alone and couldn't check that Hybernate manages it; kubectl hybernate status shows "+
		"whether it does.", namespace)
}

// enableHandWritten ends dry-run for a ManagedWorkload written by hand,
// whose spec, not the workload's annotations, says whether it's in dry-run.
func enableHandWritten(ctx context.Context, c client.Client, mw *v1alpha1.ManagedWorkload, ref string,
	opts enableOptions, out io.Writer) error {
	if !mw.Spec.DryRun {
		_, _ = fmt.Fprintf(out, "%s isn't in dry-run; Hybernate already pauses it while idle\n", ref)
		return nil
	}
	applied := appliedBy(mw, dryRunSpecField)
	if applied.gitOps != "" && !opts.force {
		_, _ = fmt.Fprintf(out, "%s's ManagedWorkload %s is managed by %s, which would undo a change made here. "+
			"In its manifest:\n  set spec.dryRun: false\nOr pass --force to change the cluster anyway.\n",
			ref, mw.Name, applied.gitOps)
		return errManagedByGit
	}
	if err := endHandWrittenDryRun(ctx, c, mw); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s: dry-run ended, Hybernate will pause it while idle\n", ref)
	if applied.helm {
		_, _ = fmt.Fprintln(out, helmWarning("managedworkload/"+mw.Name, "spec.dryRun"))
	}
	return nil
}

func endHandWrittenDryRun(ctx context.Context, c client.Client, mw *v1alpha1.ManagedWorkload) error {
	patch := client.MergeFrom(mw.DeepCopy())
	mw.Spec.DryRun = false
	if err := c.Patch(ctx, mw, patch); err != nil {
		return fmt.Errorf("updating ManagedWorkload %s: %w", mw.Name, err)
	}
	return nil
}

// wontPause says why the operator never pauses what the ManagedWorkload
// manages, from the conditions it set, and "" when it may. Those cover what
// enable can't see from here, such as protectedNamespaces patterns and
// Hybernate's own namespace.
func wontPause(mw *v1alpha1.ManagedWorkload) string {
	if mw == nil {
		return ""
	}
	conditions := mw.Status.Conditions
	if c := meta.FindStatusCondition(conditions, "Protected"); c != nil && c.Status == metav1.ConditionTrue {
		return "it's protected: " + c.Message
	}
	if c := meta.FindStatusCondition(conditions, "TargetAvailable"); c != nil && c.Reason == "TargetIgnored" {
		return "it's labelled " + v1alpha1.LabelIgnore
	}
	if meta.IsStatusConditionTrue(conditions, "DuplicateTarget") {
		return "another ManagedWorkload manages it: " + mw.Name + " is the duplicate"
	}
	return ""
}

// checkProtected refuses a namespace the operator never manages anything
// in. The operator's own namespace and protectedNamespaces patterns aren't
// known here; a workload they protect has no ManagedWorkload, which enable
// reports instead.
func checkProtected(ns *corev1.Namespace) error {
	if !v1alpha1.Protected(ns.Name, ns.Labels, nil) {
		return nil
	}
	return fmt.Errorf("namespace %s is protected, so Hybernate doesn't manage anything in it; label the namespace "+
		"%s=true to let it", ns.Name, v1alpha1.LabelAllowProtected)
}

// checkOptedIn says why the operator doesn't opt the workload in through
// the label, deciding as it does: the workload's own label, "true" or
// anything else, wins over its namespace's, and hybernate.io/ignore over
// both.
func checkOptedIn(obj client.Object, ns *corev1.Namespace, ref string) error {
	labels := obj.GetLabels()
	if labels[v1alpha1.LabelIgnore] == v1alpha1.True {
		return fmt.Errorf("%s is labelled %s=true, so Hybernate doesn't manage it", ref, v1alpha1.LabelIgnore)
	}
	if own, ok := labels[v1alpha1.LabelManaged]; ok {
		if own == v1alpha1.True {
			return nil
		}
		return fmt.Errorf("%s is labelled %s=%q, which opts it out, so Hybernate doesn't manage it", ref,
			v1alpha1.LabelManaged, own)
	}
	if ns.GetLabels()[v1alpha1.LabelManaged] != v1alpha1.True {
		return fmt.Errorf("%s isn't opted in: label it %s=true first", ref, v1alpha1.LabelManaged)
	}
	return nil
}

// inDryRun says whether an opted-in workload is in dry-run. Its
// ManagedWorkload says so for certain, having weighed the annotations with
// the cluster's default, which the plugin can't read. For a user who may
// not read ManagedWorkloads, the annotations are all there is; without
// either, the cluster's default is unknown, so it may be.
func inDryRun(mw *v1alpha1.ManagedWorkload, obj client.Object, ns *corev1.Namespace) bool {
	if mw != nil {
		return mw.Spec.DryRun
	}
	if own, ok := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]; ok {
		return own != dryRunOff
	}
	if inherited, ok := ns.GetAnnotations()[v1alpha1.AnnotationDryRun]; ok {
		return inherited != dryRunOff
	}
	return true
}

// dryRunSet is what's in dry-run in a namespace: workloads opted in
// with the label, and ManagedWorkloads written by hand.
type dryRunSet struct {
	labelled    []client.Object
	handWritten []*v1alpha1.ManagedWorkload
	// inherits is set when a labelled workload has no annotation of its
	// own, so it takes the namespace's or the cluster's default.
	inherits bool
	// notPickedUp are labelled workloads the operator hasn't made a
	// ManagedWorkload for, which changing their own annotation would do
	// nothing for yet.
	notPickedUp []client.Object
	// wontPause are workloads the operator never pauses, with why.
	wontPause []string
}

// waitingFor are the refs of the workloads Hybernate hasn't picked up whose
// dry-run nothing enable changed ends: all of them, unless it just set the
// namespace's annotation to "false", which ends it for each one without an
// annotation of its own once Hybernate picks it up.
func (s dryRunSet) waitingFor(namespaceEnded bool) []string {
	var refs []string
	for _, obj := range s.notPickedUp {
		if _, own := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]; namespaceEnded && !own {
			continue
		}
		refs = append(refs, fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName()))
	}
	return refs
}

func dryRunIn(workloads []client.Object, managed managedWorkloads, ns *corev1.Namespace) dryRunSet {
	var found dryRunSet
	for _, obj := range workloads {
		mw := managed.byTarget[workloadKey(obj)]
		if why := wontPause(mw); why != "" {
			if mw.Spec.DryRun || mw.Labels[v1alpha1.LabelFromLabel] == v1alpha1.True && inDryRun(mw, obj, ns) {
				found.wontPause = append(found.wontPause, fmt.Sprintf("%s/%s, as %s", kindOf(obj), obj.GetName(), why))
			}
			continue
		}
		if mw != nil && mw.Labels[v1alpha1.LabelFromLabel] != v1alpha1.True {
			if mw.Spec.DryRun {
				found.handWritten = append(found.handWritten, mw)
			}
			continue
		}
		if checkOptedIn(obj, ns, "") != nil {
			continue
		}
		if managed.readable && mw == nil {
			found.notPickedUp = append(found.notPickedUp, obj)
			continue
		}
		if !inDryRun(mw, obj, ns) {
			continue
		}
		found.labelled = append(found.labelled, obj)
		if _, own := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]; !own {
			found.inherits = true
		}
	}
	return found
}

// inGit is something enable left alone because Git owns it, and the change
// to make there.
type inGit struct {
	ref, tool, change string
}

// namespaceEnabler ends dry-run across a namespace, keeping what it
// changed, what it left for Git, and what Helm will put back.
type namespaceEnabler struct {
	c        client.Client
	opts     enableOptions
	enabled  int
	git      []inGit
	warnings []string
	// ownAnnotations says the user may not change the namespace's
	// annotation, so every workload gets its own.
	ownAnnotations bool
}

func enableNamespace(ctx context.Context, c client.Client, namespace string, opts enableOptions, out io.Writer) error {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("getting namespace %s: %w", namespace, err)
	}
	if err := checkProtected(&ns); err != nil {
		return err
	}
	workloads, err := listWorkloads(ctx, c, namespace)
	if err != nil {
		return err
	}
	managed, err := managedByTarget(ctx, c, namespace)
	if err != nil {
		return err
	}
	found := dryRunIn(workloads, managed, &ns)
	if !managed.readable {
		_, _ = fmt.Fprintln(out, unreadableNote(namespace))
	}
	nsValue, nsSet := ns.GetAnnotations()[v1alpha1.AnnotationDryRun]
	nsLive := nsSet && nsValue == dryRunOff
	reportWontPause(out, found.wontPause)
	if done, err := nothingToEnd(out, namespace, found, nsSet && !nsLive); done {
		return err
	}

	e := &namespaceEnabler{c: c, opts: opts}
	namespaceEnded := false
	if !nsLive && (nsSet || found.inherits) {
		namespaceEnded, err = e.namespace(ctx, &ns)
		switch {
		case apierrors.IsForbidden(err) && len(found.labelled) == 0 && len(found.handWritten) == 0:
			return fmt.Errorf("namespace %s's %s annotation is all that's in dry-run, for the workloads Hybernate "+
				"picks up later, and your access doesn't allow changing it; ask an admin to set it to \"false\": %w",
				namespace, v1alpha1.AnnotationDryRun, err)
		case apierrors.IsForbidden(err):
			e.ownAnnotations = true
			_, _ = fmt.Fprintf(out, "namespace %s: your access doesn't allow changing its %s annotation, so it was "+
				"left alone; each workload gets its own \"false\" instead, which wins over it\n", namespace,
				v1alpha1.AnnotationDryRun)
		case err != nil:
			return err
		}
		nsLive = namespaceEnded
		if namespaceEnded {
			_, _ = fmt.Fprintf(out, "namespace %s: dry-run ended for the workloads that take its annotation, "+
				"including ones Hybernate picks up later\n", namespace)
		}
	}
	for _, obj := range found.labelled {
		if err := e.labelled(ctx, obj, nsLive); err != nil {
			return err
		}
	}
	for _, mw := range found.handWritten {
		if err := e.handWritten(ctx, mw); err != nil {
			return err
		}
	}

	if e.enabled > 0 {
		_, _ = fmt.Fprintf(out, "%s in %s: dry-run ended, Hybernate will pause %s while idle\n",
			countOf(e.enabled, "workload"), namespace, pronounObject(e.enabled))
	}
	for _, w := range e.warnings {
		_, _ = fmt.Fprintln(out, w)
	}
	var errs []error
	if len(e.git) > 0 {
		_, _ = fmt.Fprintln(out, "Not changed, because their manifests come from Git; change them there:")
		for _, g := range e.git {
			_, _ = fmt.Fprintf(out, "  %s (%s): %s\n", g.ref, g.tool, g.change)
		}
		errs = append(errs, errManagedByGit)
	}
	if waiting := found.waitingFor(namespaceEnded); len(waiting) > 0 {
		errs = append(errs, reportNotPickedUp(out, namespace, waiting))
	}
	return errors.Join(errs...)
}

// nothingToEnd reports a namespace where enable has nothing to change, and
// true then: nothing in it is in dry-run, the namespace's own annotation
// included, or only what Hybernate hasn't picked up or never pauses is.
func nothingToEnd(out io.Writer, namespace string, found dryRunSet, nsInDryRun bool) (bool, error) {
	if len(found.labelled) > 0 || len(found.handWritten) > 0 {
		return false, nil
	}
	if len(found.notPickedUp) > 0 {
		return true, reportNotPickedUp(out, namespace, found.waitingFor(false))
	}
	if nsInDryRun {
		return false, nil
	}
	if len(found.wontPause) == 0 {
		_, _ = fmt.Fprintf(out, "Nothing in %s is in dry-run; Hybernate already pauses its workloads while idle\n",
			namespace)
	}
	return true, nil
}

// reportWontPause lists the workloads in dry-run that ending it would do
// nothing for, since the operator never pauses them.
func reportWontPause(out io.Writer, refs []string) {
	if len(refs) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "Not changed, because Hybernate never pauses them:")
	for _, ref := range refs {
		_, _ = fmt.Fprintf(out, "  %s\n", ref)
	}
}

// reportNotPickedUp lists the labelled workloads the operator hasn't made a
// ManagedWorkload for, and fails: ending their dry-run changes nothing.
func reportNotPickedUp(out io.Writer, namespace string, refs []string) error {
	_, _ = fmt.Fprintf(out, "Not changed, because Hybernate hasn't picked them up: no ManagedWorkload manages them. "+
		"The operator may not be running, may not watch %s (its watchNamespaces), or may protect it:\n", namespace)
	for _, ref := range refs {
		_, _ = fmt.Fprintf(out, "  %s\n", ref)
	}
	have := "have"
	if len(refs) == 1 {
		have = "has"
	}
	return fmt.Errorf("%s in %s %s no ManagedWorkload", countOf(len(refs), "opted-in workload"), namespace, have)
}

// namespace sets the namespace's dry-run annotation to "false", unless Git
// owns it, and says whether the namespace is now live.
func (e *namespaceEnabler) namespace(ctx context.Context, ns *corev1.Namespace) (bool, error) {
	applied := appliedBy(ns, dryRunAnnotationField)
	if applied.gitOps != "" && !e.opts.force {
		e.git = append(e.git, inGit{"namespace " + ns.Name, applied.gitOps,
			fmt.Sprintf(`set the annotation %s: "false"`, v1alpha1.AnnotationDryRun)})
		return false, nil
	}
	if err := setAnnotation(ctx, e.c, ns, v1alpha1.AnnotationDryRun, dryRunOff); err != nil {
		return false, err
	}
	e.warnIfHelm(applied, "namespace "+ns.Name, v1alpha1.AnnotationDryRun+" annotation")
	return true, nil
}

// labelled ends dry-run for a workload opted in with the label. With the
// namespace live, the workload's own annotation is only in the way; without
// it, the workload needs its own "false". One that takes the namespace's
// annotation is left to the namespace, unless the user may not change it.
func (e *namespaceEnabler) labelled(ctx context.Context, obj client.Object, nsLive bool) error {
	if _, own := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]; !own && (nsLive || !e.ownAnnotations) {
		if nsLive {
			e.enabled++
		}
		return nil
	}
	value, change := dryRunOff, fmt.Sprintf(`set the annotation %s: "false"`, v1alpha1.AnnotationDryRun)
	if nsLive {
		value, change = "", "remove the annotation "+v1alpha1.AnnotationDryRun
	}
	ref := fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName())
	applied := appliedBy(obj, dryRunAnnotationField)
	if applied.gitOps != "" && !e.opts.force {
		e.git = append(e.git, inGit{ref, applied.gitOps, change})
		return nil
	}
	if err := setAnnotation(ctx, e.c, obj, v1alpha1.AnnotationDryRun, value); err != nil {
		return err
	}
	e.warnIfHelm(applied, ref, v1alpha1.AnnotationDryRun+" annotation")
	e.enabled++
	return nil
}

func (e *namespaceEnabler) handWritten(ctx context.Context, mw *v1alpha1.ManagedWorkload) error {
	ref := "managedworkload/" + mw.Name
	applied := appliedBy(mw, dryRunSpecField)
	if applied.gitOps != "" && !e.opts.force {
		e.git = append(e.git, inGit{ref, applied.gitOps, "set spec.dryRun: false"})
		return nil
	}
	if err := endHandWrittenDryRun(ctx, e.c, mw); err != nil {
		return err
	}
	e.warnIfHelm(applied, ref, "spec.dryRun")
	e.enabled++
	return nil
}

func (e *namespaceEnabler) warnIfHelm(applied applier, ref, setting string) {
	if applied.helm {
		e.warnings = append(e.warnings, helmWarning(ref, setting))
	}
}

func pronounObject(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func listWorkloads(ctx context.Context, c client.Client, namespace string) ([]client.Object, error) {
	var workloads []client.Object
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deployments.Items {
		workloads = append(workloads, &deployments.Items[i])
	}
	var statefulSets appsv1.StatefulSetList
	if err := c.List(ctx, &statefulSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range statefulSets.Items {
		workloads = append(workloads, &statefulSets.Items[i])
	}
	return workloads, nil
}

// managedWorkloads are a namespace's ManagedWorkloads by their target.
// readable is false when the user may not list them, and byTarget is then
// empty.
type managedWorkloads struct {
	byTarget map[targetKey]*v1alpha1.ManagedWorkload
	readable bool
}

// managedByTarget reads the namespace's ManagedWorkloads. Enable works from
// annotations alone for a user who may only edit workloads, but not without
// the CRD: with nothing installed to pause the workload, ending its dry-run
// would mean nothing.
func managedByTarget(ctx context.Context, c client.Client, namespace string) (managedWorkloads, error) {
	var list v1alpha1.ManagedWorkloadList
	err := c.List(ctx, &list, client.InNamespace(namespace))
	switch {
	case meta.IsNoMatchError(err):
		return managedWorkloads{}, errNotInstalled
	case apierrors.IsForbidden(err):
		return managedWorkloads{}, nil
	case err != nil:
		return managedWorkloads{}, fmt.Errorf("listing ManagedWorkloads in %s: %w", namespace, err)
	}
	out := managedWorkloads{byTarget: make(map[targetKey]*v1alpha1.ManagedWorkload, len(list.Items)), readable: true}
	// Of two for one target, the operator manages it through the one it
	// doesn't mark a duplicate.
	for i := range list.Items {
		mw := &list.Items[i]
		key := targetOfManaged(mw)
		if have := out.byTarget[key]; have != nil && meta.IsStatusConditionTrue(mw.Status.Conditions, "DuplicateTarget") {
			continue
		}
		out.byTarget[key] = mw
	}
	return out, nil
}

func workloadKey(obj client.Object) targetKey {
	kind := v1alpha1.TargetKindDeployment
	if kindOf(obj) == kindStatefulSet {
		kind = v1alpha1.TargetKindStatefulSet
	}
	return targetKey{obj.GetNamespace(), kind, obj.GetName()}
}

// setAnnotation sets an annotation on obj, or removes it when value is empty.
func setAnnotation(ctx context.Context, c client.Client, obj client.Object, key, value string) error {
	original := obj.DeepCopyObject().(client.Object)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value == "" {
		delete(annotations, key)
	} else {
		annotations[key] = value
	}
	obj.SetAnnotations(annotations)
	if err := c.Patch(ctx, obj, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("updating %s: %w", obj.GetName(), err)
	}
	return nil
}

// The managed-fields paths of the settings enable changes.
var (
	dryRunAnnotationField = []string{"f:metadata", "f:annotations", "f:" + v1alpha1.AnnotationDryRun}
	dryRunSpecField       = []string{"f:spec", "f:dryRun"}
)

// helmManager is the field manager Helm applies a chart's objects as.
const helmManager = "helm"

// applier is what sets a field from outside the cluster: a GitOps tool,
// which puts back what enable changes on its next sync, or Helm, whose
// next upgrade does.
type applier struct {
	// gitOps names the tool, and the application it applies the object
	// for when its tracking metadata says.
	gitOps string
	helm   bool
}

// appliedBy finds who sets the field at path on obj from the field managers
// the API server records owning it. A GitOps tool that applies the object
// but not this field, such as an annotation Git doesn't have, leaves a
// change to it alone. An object with no managed fields recorded, such as
// one created before there were any, goes by the tracking metadata each tool
// adds to what it applies.
func appliedBy(obj client.Object, path []string) applier {
	entries := obj.GetManagedFields()
	if len(entries) == 0 {
		return applier{gitOps: trackedBy(obj)}
	}
	var found applier
	for _, entry := range entries {
		if !ownsField(entry.FieldsV1.GetRawBytes(), path) {
			continue
		}
		if tool := gitops.ToolOf(entry.Manager); tool != "" {
			return applier{gitOps: gitOpsName(tool, obj)}
		}
		found.helm = found.helm || entry.Manager == helmManager
	}
	return found
}

// gitOpsName is the tool that applies obj, with the application when its
// tracking metadata names one. With the manager known to be Argo CD, the
// app.kubernetes.io/instance label it tracks by default names the
// application.
func gitOpsName(tool gitops.Tool, obj client.Object) string {
	if tracked := trackedBy(obj); tracked != "" {
		return tracked
	}
	if app := obj.GetLabels()["app.kubernetes.io/instance"]; tool == gitops.ArgoCD && app != "" {
		return "Argo CD application " + app
	}
	return string(tool)
}

func ownsField(raw []byte, path []string) bool {
	for _, name := range path {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return false
		}
		var ok bool
		if raw, ok = fields[name]; !ok {
			return false
		}
	}
	return true
}

// trackedBy names the GitOps tool and application that apply an object,
// from the tracking metadata each one adds, or "" if none does. Argo CD
// tracks by the app.kubernetes.io/instance label by default, which Helm
// charts set too, so that label alone doesn't say Argo CD applies it.
func trackedBy(obj client.Object) string {
	annotations, labels := obj.GetAnnotations(), obj.GetLabels()
	switch {
	case annotations["argocd.argoproj.io/tracking-id"] != "":
		app, _, _ := strings.Cut(annotations["argocd.argoproj.io/tracking-id"], ":")
		return "Argo CD application " + app
	case labels["argocd.argoproj.io/instance"] != "":
		return "Argo CD application " + labels["argocd.argoproj.io/instance"]
	case labels["kustomize.toolkit.fluxcd.io/name"] != "":
		return "Flux Kustomization " + labels["kustomize.toolkit.fluxcd.io/name"]
	case labels["helm.toolkit.fluxcd.io/name"] != "":
		return "Flux HelmRelease " + labels["helm.toolkit.fluxcd.io/name"]
	}
	return ""
}

// helmWarning says Helm will put back the setting enable just changed.
func helmWarning(ref, setting string) string {
	return fmt.Sprintf("Warning: Helm set %s's %s, so the next helm upgrade puts the chart's value back; "+
		"set it to false in the chart too", ref, setting)
}

func kindOf(obj client.Object) string {
	if _, ok := obj.(*appsv1.StatefulSet); ok {
		return kindStatefulSet
	}
	return kindDeployment
}
