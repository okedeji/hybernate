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

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// errManagedByGit means the change belongs in Git, where a GitOps tool would
// otherwise put the annotation straight back.
var errManagedByGit = errors.New("managed from Git")

type enableOptions struct {
	all   bool
	force bool
}

func enableCmd() *cobra.Command {
	var opts enableOptions
	var namespace string
	cmd := &cobra.Command{
		Use:   "enable [KIND/]NAME",
		Short: "Stop measuring a workload in dry-run and let Hybernate pause it",
		Long: `Enable ends dry-run for a workload opted in with the hybernate.io/managed
label, so Hybernate starts pausing it while idle. It removes the
hybernate.io/dry-run annotation, or, when the workload's namespace sets it,
overrides it on the workload with "false".

A workload whose manifests come from Argo CD or Flux isn't changed: its
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
			k8s, defaultNamespace, err := buildClient()
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}
			if namespace == "" {
				namespace = defaultNamespace
			}
			if opts.all {
				return enableNamespace(cmd.Context(), k8s, namespace, opts, cmd.OutOrStdout())
			}
			return enableWorkload(cmd.Context(), k8s, namespace, args[0], opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the workload (defaults to the kubeconfig context's)")
	cmd.Flags().BoolVar(&opts.all, "all", false, "Enable every workload in the namespace")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Change the cluster even when Argo CD or Flux manages the workload")
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
			return nil, err
		}
	}
	var s appsv1.StatefulSet
	return &s, wrapNotFound(c.Get(ctx, key, &s), kindStatefulSet, key)
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
	ref := fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName())
	if obj.GetLabels()[v1alpha1.LabelManaged] != v1alpha1.True && ns.GetLabels()[v1alpha1.LabelManaged] != v1alpha1.True {
		return fmt.Errorf("%s isn't opted in: label it %s=true first", ref, v1alpha1.LabelManaged)
	}

	own, ownSet := obj.GetAnnotations()[v1alpha1.AnnotationDryRun]
	fromNamespace := ns.GetAnnotations()[v1alpha1.AnnotationDryRun] == v1alpha1.True
	switch {
	case ownSet && own == v1alpha1.True:
	case !ownSet && fromNamespace:
	default:
		_, _ = fmt.Fprintf(out, "%s isn't in dry-run; Hybernate already pauses it while idle\n", ref)
		return nil
	}

	// Removing the workload's own annotation would leave the namespace's in
	// force, so the workload overrides it instead.
	value := ""
	if fromNamespace {
		value = "false"
	}
	if tool := gitOpsTool(obj); tool != "" && !opts.force {
		_, _ = fmt.Fprintf(out, "%s is managed by %s, which would undo a change made here. In its manifest:\n", ref, tool)
		if value == "" {
			_, _ = fmt.Fprintf(out, "  remove the annotation  %s: \"true\"\n", v1alpha1.AnnotationDryRun)
		} else {
			_, _ = fmt.Fprintf(out, "  add the annotation     %s: \"false\"\n", v1alpha1.AnnotationDryRun)
		}
		_, _ = fmt.Fprintln(out, "Or pass --force to change the cluster anyway.")
		return errManagedByGit
	}
	if err := setDryRun(ctx, c, obj, value); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s: dry-run ended, Hybernate will pause it while idle\n", ref)
	return nil
}

func enableNamespace(ctx context.Context, c client.Client, namespace string, opts enableOptions, out io.Writer) error {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("getting namespace %s: %w", namespace, err)
	}
	var workloads []client.Object
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deployments.Items {
		workloads = append(workloads, &deployments.Items[i])
	}
	var statefulSets appsv1.StatefulSetList
	if err := c.List(ctx, &statefulSets, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range statefulSets.Items {
		workloads = append(workloads, &statefulSets.Items[i])
	}

	var inGit []string
	enabled := 0
	for _, obj := range workloads {
		if obj.GetAnnotations()[v1alpha1.AnnotationDryRun] != v1alpha1.True {
			continue
		}
		ref := fmt.Sprintf("%s/%s", kindOf(obj), obj.GetName())
		if tool := gitOpsTool(obj); tool != "" && !opts.force {
			inGit = append(inGit, fmt.Sprintf("%s (%s)", ref, tool))
			continue
		}
		if err := setDryRun(ctx, c, obj, ""); err != nil {
			return err
		}
		enabled++
	}
	if ns.GetAnnotations()[v1alpha1.AnnotationDryRun] == v1alpha1.True {
		patch := client.MergeFrom(ns.DeepCopy())
		delete(ns.Annotations, v1alpha1.AnnotationDryRun)
		if err := c.Patch(ctx, &ns, patch); err != nil {
			return fmt.Errorf("removing dry-run from namespace %s: %w", namespace, err)
		}
		_, _ = fmt.Fprintf(out, "namespace %s: dry-run ended\n", namespace)
	}
	noun := "workloads"
	if enabled == 1 {
		noun = "workload"
	}
	_, _ = fmt.Fprintf(out, "%d %s in %s: dry-run ended, Hybernate will pause them while idle\n", enabled, noun, namespace)
	if len(inGit) > 0 {
		_, _ = fmt.Fprintf(out, "Not changed, because their manifests come from Git; remove %s from them there:\n",
			v1alpha1.AnnotationDryRun)
		for _, ref := range inGit {
			_, _ = fmt.Fprintf(out, "  %s\n", ref)
		}
		return errManagedByGit
	}
	return nil
}

// setDryRun sets the workload's dry-run annotation to value, or removes it
// when value is empty.
func setDryRun(ctx context.Context, c client.Client, obj client.Object, value string) error {
	original := obj.DeepCopyObject().(client.Object)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value == "" {
		delete(annotations, v1alpha1.AnnotationDryRun)
	} else {
		annotations[v1alpha1.AnnotationDryRun] = value
	}
	obj.SetAnnotations(annotations)
	if err := c.Patch(ctx, obj, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("updating %s: %w", obj.GetName(), err)
	}
	return nil
}

// gitOpsTool names the GitOps tool that applies the workload, from the
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
