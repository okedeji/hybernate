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
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
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

With --all, it sets the namespace's annotation to "false" instead, and drops
the workloads' own.

Anything whose manifest comes from Argo CD or Flux isn't changed: its
annotations live in Git, where the tool would put the old value back. Enable
says what to change there instead.

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
	cmd.Flags().BoolVar(&opts.force, "force", false, "Change the cluster even when Argo CD or Flux manages the object")
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
	if kind != kindStatefulSet {
		var d appsv1.Deployment
		err := c.Get(ctx, key, &d)
		if err == nil || kind == kindDeployment {
			return &d, wrapNotFound(err, kindDeployment, key)
		}
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("getting deployment %s: %w", key, err)
		}
	}
	var s appsv1.StatefulSet
	err = c.Get(ctx, key, &s)
	if kind == "" && apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("no Deployment or StatefulSet in %s is named %s: %w", namespace, name, err)
	}
	return &s, wrapNotFound(err, kindStatefulSet, key)
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
	managed, err := managedByTarget(ctx, c, namespace)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName())
	mw := managed[workloadKey(obj)]
	if mw != nil && mw.Labels[v1alpha1.LabelFromLabel] != v1alpha1.True {
		return enableHandWritten(ctx, c, mw, ref, opts, out)
	}
	if err := checkOptedIn(obj, &ns, ref); err != nil {
		return err
	}
	if !inDryRun(mw, obj, &ns) {
		_, _ = fmt.Fprintf(out, "%s isn't in dry-run; Hybernate already pauses it while idle\n", ref)
		return nil
	}
	if tool := gitOpsTool(obj); tool != "" && !opts.force {
		_, _ = fmt.Fprintf(out, "%s is managed by %s, which would undo a change made here. In its manifest:\n", ref, tool)
		_, _ = fmt.Fprintf(out, "  set the annotation  %s: \"false\"\n", v1alpha1.AnnotationDryRun)
		_, _ = fmt.Fprintln(out, "Or pass --force to change the cluster anyway.")
		return errManagedByGit
	}
	if err := setAnnotation(ctx, c, obj, v1alpha1.AnnotationDryRun, dryRunOff); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s: dry-run ended, Hybernate will pause it while idle\n", ref)
	return nil
}

// enableHandWritten ends dry-run for a ManagedWorkload written by hand,
// whose spec, not the workload's annotations, says whether it's in dry-run.
func enableHandWritten(ctx context.Context, c client.Client, mw *v1alpha1.ManagedWorkload, ref string,
	opts enableOptions, out io.Writer) error {
	if !mw.Spec.DryRun {
		_, _ = fmt.Fprintf(out, "%s isn't in dry-run; Hybernate already pauses it while idle\n", ref)
		return nil
	}
	if tool := gitOpsTool(mw); tool != "" && !opts.force {
		_, _ = fmt.Fprintf(out, "%s's ManagedWorkload %s is managed by %s, which would undo a change made here. "+
			"In its manifest:\n  set spec.dryRun: false\nOr pass --force to change the cluster anyway.\n",
			ref, mw.Name, tool)
		return errManagedByGit
	}
	if err := endHandWrittenDryRun(ctx, c, mw); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s: dry-run ended, Hybernate will pause it while idle\n", ref)
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

func checkOptedIn(obj client.Object, ns *corev1.Namespace, ref string) error {
	if obj.GetLabels()[v1alpha1.LabelIgnore] == v1alpha1.True {
		return fmt.Errorf("%s is labelled %s=true, so Hybernate doesn't manage it", ref, v1alpha1.LabelIgnore)
	}
	if obj.GetLabels()[v1alpha1.LabelManaged] != v1alpha1.True && ns.GetLabels()[v1alpha1.LabelManaged] != v1alpha1.True {
		return fmt.Errorf("%s isn't opted in: label it %s=true first", ref, v1alpha1.LabelManaged)
	}
	return nil
}

// inDryRun says whether an opted-in workload is in dry-run. Its
// ManagedWorkload says so for certain, having weighed the annotations with
// the cluster's default, which the plugin can't read. Before the operator
// has made one, the annotations are all there is; without either, the
// cluster's default is unknown, so it may be.
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
}

func dryRunIn(workloads []client.Object, managed map[targetKey]*v1alpha1.ManagedWorkload,
	ns *corev1.Namespace) dryRunSet {
	var found dryRunSet
	for _, obj := range workloads {
		mw := managed[workloadKey(obj)]
		if mw != nil && mw.Labels[v1alpha1.LabelFromLabel] != v1alpha1.True {
			if mw.Spec.DryRun {
				found.handWritten = append(found.handWritten, mw)
			}
			continue
		}
		if checkOptedIn(obj, ns, "") != nil || !inDryRun(mw, obj, ns) {
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
// changed and what it left for Git.
type namespaceEnabler struct {
	c       client.Client
	opts    enableOptions
	enabled int
	git     []inGit
}

func enableNamespace(ctx context.Context, c client.Client, namespace string, opts enableOptions, out io.Writer) error {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("getting namespace %s: %w", namespace, err)
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
	nsValue, nsSet := ns.GetAnnotations()[v1alpha1.AnnotationDryRun]
	nsLive := nsSet && nsValue == dryRunOff
	if len(found.labelled) == 0 && len(found.handWritten) == 0 && (!nsSet || nsLive) {
		_, _ = fmt.Fprintf(out, "Nothing in %s is in dry-run; Hybernate already pauses its workloads while idle\n",
			namespace)
		return nil
	}

	e := &namespaceEnabler{c: c, opts: opts}
	if !nsLive && (nsSet || found.inherits) {
		if nsLive, err = e.namespace(ctx, &ns); err != nil {
			return err
		}
		if nsLive {
			_, _ = fmt.Fprintf(out, "namespace %s: dry-run ended\n", namespace)
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

	_, _ = fmt.Fprintf(out, "%s in %s: dry-run ended, Hybernate will pause %s while idle\n",
		countOf(e.enabled, "workload"), namespace, pronounObject(e.enabled))
	if len(e.git) > 0 {
		_, _ = fmt.Fprintln(out, "Not changed, because their manifests come from Git; change them there:")
		for _, g := range e.git {
			_, _ = fmt.Fprintf(out, "  %s (%s): %s\n", g.ref, g.tool, g.change)
		}
		return errManagedByGit
	}
	return nil
}

// namespace sets the namespace's dry-run annotation to "false", unless Git
// owns it, and says whether the namespace is now live.
func (e *namespaceEnabler) namespace(ctx context.Context, ns *corev1.Namespace) (bool, error) {
	if tool := gitOpsTool(ns); tool != "" && !e.opts.force {
		e.git = append(e.git, inGit{"namespace " + ns.Name, tool,
			fmt.Sprintf(`set the annotation %s: "false"`, v1alpha1.AnnotationDryRun)})
		return false, nil
	}
	if err := setAnnotation(ctx, e.c, ns, v1alpha1.AnnotationDryRun, dryRunOff); err != nil {
		return false, err
	}
	return true, nil
}

// labelled ends dry-run for a workload opted in with the label. With the
// namespace live, the workload's own annotation is only in the way; without
// it, the workload needs its own "false".
func (e *namespaceEnabler) labelled(ctx context.Context, obj client.Object, nsLive bool) error {
	if _, own := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]; !own {
		if nsLive {
			e.enabled++
		}
		return nil
	}
	value, change := dryRunOff, fmt.Sprintf(`set the annotation %s: "false"`, v1alpha1.AnnotationDryRun)
	if nsLive {
		value, change = "", "remove the annotation "+v1alpha1.AnnotationDryRun
	}
	if tool := gitOpsTool(obj); tool != "" && !e.opts.force {
		e.git = append(e.git, inGit{fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName()), tool, change})
		return nil
	}
	if err := setAnnotation(ctx, e.c, obj, v1alpha1.AnnotationDryRun, value); err != nil {
		return err
	}
	e.enabled++
	return nil
}

func (e *namespaceEnabler) handWritten(ctx context.Context, mw *v1alpha1.ManagedWorkload) error {
	if tool := gitOpsTool(mw); tool != "" && !e.opts.force {
		e.git = append(e.git, inGit{"managedworkload/" + mw.Name, tool, "set spec.dryRun: false"})
		return nil
	}
	if err := endHandWrittenDryRun(ctx, e.c, mw); err != nil {
		return err
	}
	e.enabled++
	return nil
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

// managedByTarget maps the namespace's workloads to their ManagedWorkloads.
// Enable works from annotations alone when it can't read them, so it can
// still end dry-run before the operator is installed, or for a user who may
// only edit workloads.
func managedByTarget(ctx context.Context, c client.Client, namespace string) (
	map[targetKey]*v1alpha1.ManagedWorkload, error) {
	var list v1alpha1.ManagedWorkloadList
	err := c.List(ctx, &list, client.InNamespace(namespace))
	if meta.IsNoMatchError(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing ManagedWorkloads in %s: %w", namespace, err)
	}
	out := make(map[targetKey]*v1alpha1.ManagedWorkload, len(list.Items))
	for i := range list.Items {
		out[targetOfManaged(&list.Items[i])] = &list.Items[i]
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

// gitOpsTool names the GitOps tool that applies an object, from the
// tracking metadata each one adds, or "" if none does.
func gitOpsTool(obj client.Object) string {
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

func kindOf(obj client.Object) string {
	if _, ok := obj.(*appsv1.StatefulSet); ok {
		return kindStatefulSet
	}
	return kindDeployment
}
