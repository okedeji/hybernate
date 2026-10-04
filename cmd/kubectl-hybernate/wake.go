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
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// errNotWakeable means the activity annotation can't wake the workload, so
// stamping it would only wait out the timeout.
var errNotWakeable = errors.New("can't be woken by activity")

type wakeOptions struct {
	keepAwake    time.Duration
	wait         bool
	timeout      time.Duration
	pollInterval time.Duration
	now          func() time.Time
}

func wakeCmd() *cobra.Command {
	opts := wakeOptions{pollInterval: 2 * time.Second, now: time.Now}
	var namespace string

	cmd := &cobra.Command{
		Use:   "wake NAME",
		Short: "Wake a paused workload and wait until it's Running",
		Long: `Wake marks a ManagedWorkload as active now, which wakes it if it's paused
and restarts its idle clock if it's running. Workloads it depends on wake
too. By default it waits until the workload is Running.

Examples:
  # Wake a workload and wait for it
  kubectl hybernate wake api -n preview-42

  # Keep it awake for the next two hours, for a demo
  kubectl hybernate wake api -n preview-42 --for 2h

  # Return as soon as the wake is requested
  kubectl hybernate wake api -n preview-42 --wait=false`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8s, defaultNamespace, err := buildClient()
			if err != nil {
				return fmt.Errorf("building kubernetes client: %w", err)
			}
			if namespace == "" {
				namespace = defaultNamespace
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()
			return wake(ctx, k8s, client.ObjectKey{Namespace: namespace, Name: args[0]}, opts, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the ManagedWorkload (defaults to the kubeconfig context's)")
	cmd.Flags().DurationVar(&opts.keepAwake, "for", 0, "Also keep the workload awake for this long, e.g. 2h")
	cmd.Flags().BoolVar(&opts.wait, "wait", true, "Wait until the workload is Running")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 5*time.Minute, "How long to wait")
	return cmd
}

// wake stamps the workload's activity annotations and, if asked, waits until
// it's Running.
func wake(ctx context.Context, c client.Client, key client.ObjectKey, opts wakeOptions, out io.Writer) error {
	var w v1alpha1.ManagedWorkload
	if err := c.Get(ctx, key, &w); err != nil {
		return fmt.Errorf("getting ManagedWorkload %s: %w", key, err)
	}
	if err := checkWakeable(&w); err != nil {
		return err
	}

	start := opts.now()
	patch := client.MergeFrom(w.DeepCopy())
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[v1alpha1.AnnotationLastActivity] = start.UTC().Format(time.RFC3339)
	if opts.keepAwake > 0 {
		w.Annotations[v1alpha1.AnnotationActiveUntil] = start.Add(opts.keepAwake).UTC().Format(time.RFC3339)
	}
	if err := c.Patch(ctx, &w, patch); err != nil {
		return fmt.Errorf("marking %s active: %w", key, err)
	}

	if w.Status.Phase == v1alpha1.PhaseRunning {
		_, _ = fmt.Fprintf(out, "%s is already running; its idle clock restarts now\n", key)
		return nil
	}
	if !opts.wait {
		_, _ = fmt.Fprintf(out, "waking %s\n", key)
		return nil
	}

	_, _ = fmt.Fprintf(out, "waking %s...\n", key)
	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()
	for {
		if err := c.Get(ctx, key, &w); err != nil {
			return fmt.Errorf("waiting for %s: %w", key, err)
		}
		if w.Status.Phase == v1alpha1.PhaseRunning {
			_, _ = fmt.Fprintf(out, "%s is Running after %s\n", key, opts.now().Sub(start).Round(time.Second))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s is still %s: %w; kubectl describe managedworkload %s -n %s shows why",
				key, w.Status.Phase, ctx.Err(), key.Name, key.Namespace)
		case <-ticker.C:
		}
	}
}

// checkWakeable rejects the workloads the operator won't wake on activity, so
// the user gets the reason instead of a timeout.
func checkWakeable(w *v1alpha1.ManagedWorkload) error {
	if d := w.Spec.DesiredState; d != nil && *d != v1alpha1.DesiredStateRunning {
		return fmt.Errorf("%s/%s %w: spec.desiredState is %s; remove it or set it to Running",
			w.Namespace, w.Name, errNotWakeable, *d)
	}
	if w.Status.Phase == v1alpha1.PhaseDestroyed || w.Status.Phase == v1alpha1.PhaseDestroying {
		return fmt.Errorf("%s/%s %w: it's %s, so its %s was deleted; redeploy it to bring it back",
			w.Namespace, w.Name, errNotWakeable, w.Status.Phase, w.Spec.Target.Kind)
	}
	return nil
}
