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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// errNotWakeable means the activity annotation can't wake the workload, so
// stamping it would only wait out the timeout.
var errNotWakeable = errors.New("can't be woken by activity")

// errNotRunningYet means the wake was asked for, but the workload wasn't
// Running when the wait ran out.
var errNotRunningYet = errors.New("isn't Running yet")

type wakeOptions struct {
	keepAwake    time.Duration
	wait         bool
	timeout      time.Duration
	pollInterval time.Duration
	now          func() time.Time
}

func wakeCmd(kube *kubeFlags) *cobra.Command {
	opts := wakeOptions{timeout: 5 * time.Minute, pollInterval: 2 * time.Second, now: time.Now}
	var namespace string

	cmd := &cobra.Command{
		Use:   "wake NAME",
		Short: "Wake a paused workload and wait until it's Running",
		Long: `Wake marks a workload as active now, which wakes it if it's paused and
restarts its idle clock if it's running. Workloads it depends on wake too.
By default it waits until the workload is Running.

NAME is the workload, as status shows it, such as api or statefulset/postgres,
or its ManagedWorkload's name.

Examples:
  # Wake a workload and wait for it
  kubectl hybernate wake api -n preview-42

  # Keep it awake for the next two hours, for a demo
  kubectl hybernate wake api -n preview-42 --for 2h

  # Return as soon as the wake is requested
  kubectl hybernate wake api -n preview-42 --wait=false`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPositive("timeout", opts.timeout); err != nil {
				return err
			}
			if opts.keepAwake < 0 {
				return errors.New("--for can't be negative")
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
			err = wake(ctx, k8s, client.ObjectKey{Namespace: namespace, Name: args[0]}, opts, cmd.OutOrStdout())
			if errors.Is(err, errNotRunningYet) {
				return err
			}
			return timedOut(ctx, err, opts.timeout)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the workload (defaults to the kubeconfig context's)")
	cmd.Flags().DurationVar(&opts.keepAwake, "for", 0, "Also keep the workload awake for this long, e.g. 2h")
	cmd.Flags().BoolVar(&opts.wait, "wait", true, "Wait until the workload is Running")
	addTimeoutFlag(cmd, &opts.timeout, "How long to wait")
	return cmd
}

// wake stamps the workload's activity annotations and, if asked, waits until
// it's Running. key names the workload or its ManagedWorkload.
func wake(ctx context.Context, c client.Client, key client.ObjectKey, opts wakeOptions, out io.Writer) error {
	w, err := findManaged(ctx, c, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	if err := checkWakeable(w); err != nil {
		return err
	}
	key = client.ObjectKeyFromObject(w)
	ref := key.Namespace + "/" + w.Spec.Target.Name

	start := opts.now()
	patch := client.MergeFrom(w.DeepCopy())
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[v1alpha1.AnnotationLastActivity] = start.UTC().Format(time.RFC3339)
	if opts.keepAwake > 0 {
		w.Annotations[v1alpha1.AnnotationActiveUntil] = start.Add(opts.keepAwake).UTC().Format(time.RFC3339)
	}
	if err := c.Patch(ctx, w, patch); err != nil {
		return fmt.Errorf("marking %s active: %w", ref, err)
	}

	switch phase := w.Status.Phase; {
	case w.Spec.DryRun && phase != v1alpha1.PhasePaused && phase != v1alpha1.PhasePausing:
		_, _ = fmt.Fprintf(out, "%s is in dry-run, so Hybernate never paused it; its idle clock restarts now\n", ref)
		return nil
	case phase == v1alpha1.PhaseRunning:
		_, _ = fmt.Fprintf(out, "%s is already running; its idle clock restarts now\n", ref)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s\n", wakeMessage(ref, w.Status.Phase, opts.wait))
	if !opts.wait {
		return nil
	}

	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()
	for {
		if err := c.Get(ctx, key, w); err != nil {
			return fmt.Errorf("waiting for %s: %w", ref, err)
		}
		if w.Status.Phase == v1alpha1.PhaseRunning {
			_, _ = fmt.Fprintf(out, "%s is Running after %s\n", ref, opts.now().Sub(start).Round(time.Second))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %w: it's still %s (%w); kubectl describe managedworkload %s -n %s shows why",
				ref, errNotRunningYet, w.Status.Phase, ctx.Err(), key.Name, key.Namespace)
		case <-ticker.C:
		}
	}
}

// wakeMessage says what the wake does from the phase the workload is in.
func wakeMessage(ref string, phase v1alpha1.WorkloadPhase, wait bool) string {
	var msg string
	switch phase {
	case v1alpha1.PhaseIdle:
		msg = ref + " was idle; marked active, it stays up"
	case v1alpha1.PhasePausing:
		msg = ref + " is pausing; it wakes once the pause finishes"
	case v1alpha1.PhaseResuming:
		msg = ref + " is already waking"
	default:
		msg = "waking " + ref
	}
	if wait {
		msg += "..."
	}
	return msg
}

// checkWakeable rejects the workloads the operator won't wake on activity, so
// the user gets the reason instead of a timeout.
func checkWakeable(w *v1alpha1.ManagedWorkload) error {
	ref := w.Namespace + "/" + w.Spec.Target.Name
	if d := w.Spec.DesiredState; d != nil && *d != v1alpha1.DesiredStateRunning {
		return fmt.Errorf("%s %w: spec.desiredState is %s; remove it or set it to Running", ref, errNotWakeable, *d)
	}
	conditions := w.Status.Conditions
	if c := meta.FindStatusCondition(conditions, "TargetAvailable"); c != nil && c.Status == metav1.ConditionFalse {
		return fmt.Errorf("%s %w: Hybernate isn't managing it: %s", ref, errNotWakeable, oneLine(c.Message))
	}
	if c := meta.FindStatusCondition(conditions, "DuplicateTarget"); c != nil && c.Status == metav1.ConditionTrue {
		return fmt.Errorf("%s %w: %s", ref, errNotWakeable, oneLine(c.Message))
	}
	return nil
}
