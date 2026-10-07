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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/gitops"
)

// errNotPaused means Hybernate answered the pause request without pausing
// the workload, and said why.
var errNotPaused = errors.New("wasn't paused")

// errNotPausedYet means the pause was asked for, but the workload wasn't
// Paused when the wait ran out.
var errNotPausedYet = errors.New("isn't Paused yet")

// demandExpected is Hybernate declining a pause because the forecast
// expects demand within the hour, which the person asking can override.
type demandExpected struct{ msg string }

func (e demandExpected) Error() string { return e.msg }
func (e demandExpected) Unwrap() error { return errNotPaused }

const gitOpsGuide = "https://okedeji.io/hybernate/guides/gitops/"

// What the operator's PauseRequest condition says came of a request.
const (
	conditionPauseRequest = "PauseRequest"
	reasonDryRun          = "DryRun"
	reasonScaledToZero    = "ScaledToZero"

	reasonForecastExpectsDemand = "ForecastExpectsDemand"
)

type pauseOptions struct {
	wait         bool
	timeout      time.Duration
	pollInterval time.Duration
	now          func() time.Time

	// overrideForecast pauses even when the forecast expects demand
	// within the hour, without asking.
	overrideForecast bool
	// confirm asks the person running the command a yes-or-no question,
	// or is nil when there's no one to ask.
	confirm func(question string) (bool, error)
}

func pauseCmd(kube *kubeFlags) *cobra.Command {
	opts := pauseOptions{wait: true, timeout: 5 * time.Minute, pollInterval: 2 * time.Second, now: time.Now}
	var namespace string

	cmd := &cobra.Command{
		Use:   "pause NAME",
		Short: "Pause a workload now, to wake on its next request",
		Long: `Pause asks Hybernate to pause a workload now, rather than when its idle clock
runs out, such as a preview environment at the end of the day. It's an
ordinary pause: a request to the workload, activity, or autoResume wakes it,
as kubectl hybernate wake does. By default it waits until the workload is
Paused, or until Hybernate says why it won't pause it.

Hybernate pauses it as it would an idle workload, but sooner: it doesn't
pause a workload that awake workloads depend on, one an active-until
annotation holds awake, one in a protected namespace, or one in dry-run,
where it counts a would-be pause instead. Asking overrides the hour
Hybernate otherwise waits after Argo CD or Flux undid a pause.

When the forecast expects the workload to be busy within the hour, Hybernate
says when, and pause asks whether to pause it anyway, since it would likely
be woken straight back. --yes pauses it without asking, as a script needs.

NAME is the workload, as status shows it, such as api or statefulset/postgres,
or its ManagedWorkload's name.

Examples:
  # Pause a preview environment for the night
  kubectl hybernate pause api -n preview-42

  # Pause it even if the forecast expects it to be busy soon
  kubectl hybernate pause api -n preview-42 --yes

  # Return as soon as the pause is requested
  kubectl hybernate pause api -n preview-42 --wait=false`,
		Args: cobra.ExactArgs(1),
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
			if isTerminal(cmd.InOrStdin()) {
				opts.confirm = askYesNo(cmd.InOrStdin(), cmd.ErrOrStderr())
			}
			err = pause(ctx, k8s, client.ObjectKey{Namespace: namespace, Name: args[0]}, opts, cmd.OutOrStdout(),
				cmd.ErrOrStderr())
			if errors.Is(err, errNotPausedYet) || errors.Is(err, errNotPaused) {
				return err
			}
			return timedOut(ctx, err, opts.timeout)
		},
	}

	cmd.Flags().StringVarP(&namespace, "namespace", "n", "",
		"Namespace of the workload (defaults to the kubeconfig context's)")
	cmd.Flags().BoolVar(&opts.wait, "wait", true, "Wait until the workload is Paused, or Hybernate says why it isn't")
	cmd.Flags().BoolVarP(&opts.overrideForecast, "yes", "y", false,
		"Pause it even if the forecast expects it to be busy within the hour, without asking")
	addTimeoutFlag(cmd, &opts.timeout, "How long to wait")
	return cmd
}

// pause asks Hybernate to pause the workload now, with a fresh
// hybernate.io/pause-requested token, and, if asked, waits for its answer.
// key names the workload or its ManagedWorkload. Warnings go to errOut.
func pause(ctx context.Context, c client.Client, key client.ObjectKey, opts pauseOptions, out, errOut io.Writer) error {
	w, err := findManaged(ctx, c, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	key = client.ObjectKeyFromObject(w)
	ref := key.Namespace + "/" + w.Spec.Target.Name
	start := opts.now()

	switch w.Status.Phase {
	case v1alpha1.PhasePaused:
		_, _ = fmt.Fprintf(out, "%s is already paused; a request to it, or kubectl hybernate wake, wakes it\n", ref)
		return nil
	case v1alpha1.PhasePausing:
		_, _ = fmt.Fprintf(out, "%s is already pausing%s\n", ref, ellipsis(opts.wait))
		if !opts.wait {
			return nil
		}
		return waitForPause(ctx, c, key, ref, "", start, opts, out)
	}

	warnIfGitOpsUndoes(ctx, c, w, ref, errOut)
	token, err := requestPause(ctx, c, w, ref, opts.overrideForecast, opts.now)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s\n", pauseMessage(ref, w, opts.wait))
	if !opts.wait {
		return nil
	}
	err = waitForPause(ctx, c, key, ref, token, start, opts, out)

	var declined demandExpected
	if !errors.As(err, &declined) {
		return err
	}
	if opts.confirm == nil {
		return fmt.Errorf("%w; pass --yes to pause it anyway", declined)
	}
	yes, cerr := opts.confirm(declined.Error() + "\nPause it anyway? [y/N] ")
	if cerr != nil {
		return fmt.Errorf("asking whether to pause anyway: %w", cerr)
	}
	if !yes {
		_, _ = fmt.Fprintf(out, "left %s running\n", ref)
		return nil
	}
	if err := c.Get(ctx, key, w); err != nil {
		return fmt.Errorf("getting ManagedWorkload %s: %w", key, err)
	}
	if token, err = requestPause(ctx, c, w, ref, true, opts.now); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s\n", pauseMessage(ref, w, true))
	return waitForPause(ctx, c, key, ref, token, start, opts, out)
}

// requestPause asks Hybernate to pause the workload with a fresh
// hybernate.io/pause-requested token, and returns it. Every request says
// whether it overrides the forecast, so an override doesn't outlive the
// request it was asked with.
func requestPause(ctx context.Context, c client.Client, w *v1alpha1.ManagedWorkload, ref string,
	overrideForecast bool, now func() time.Time) (string, error) {
	token := now().UTC().Format(time.RFC3339Nano)
	patch := client.MergeFrom(w.DeepCopy())
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[v1alpha1.AnnotationPauseRequested] = token
	if overrideForecast {
		w.Annotations[v1alpha1.AnnotationPauseOverridesForecast] = v1alpha1.True
	} else {
		delete(w.Annotations, v1alpha1.AnnotationPauseOverridesForecast)
	}
	if err := c.Patch(ctx, w, patch); err != nil {
		return "", fmt.Errorf("requesting a pause of %s: %w", ref, err)
	}
	return token, nil
}

// pauseMessage says what the request does from the state the workload is in.
func pauseMessage(ref string, w *v1alpha1.ManagedWorkload, wait bool) string {
	var msg string
	switch {
	case w.Spec.DryRun:
		msg = ref + " is in dry-run, so Hybernate counts a would-be pause and leaves it running"
	case w.Status.Phase == v1alpha1.PhaseIdle:
		msg = ref + " was idle; pausing it now"
	case w.Status.Phase == v1alpha1.PhaseResuming:
		msg = ref + " is waking; it pauses once it's up"
	default:
		msg = "pausing " + ref
	}
	return msg + ellipsis(wait)
}

func ellipsis(wait bool) string {
	if wait {
		return "..."
	}
	return ""
}

// waitForPause waits until Hybernate has answered the request token, and
// the workload is Paused, or Hybernate has said why it won't be. An empty
// token waits for a pause already under way.
func waitForPause(ctx context.Context, c client.Client, key client.ObjectKey, ref, token string, start time.Time,
	opts pauseOptions, out io.Writer) error {
	ticker := time.NewTicker(opts.pollInterval)
	defer ticker.Stop()
	var w v1alpha1.ManagedWorkload
	for {
		if err := c.Get(ctx, key, &w); err != nil {
			return fmt.Errorf("waiting for %s: %w", ref, err)
		}
		if token == "" || w.Status.LastPauseRequest == token {
			done, err := pauseAnswered(&w, ref, token != "", out)
			if done || err != nil {
				return err
			}
			if w.Status.Phase == v1alpha1.PhasePaused {
				_, _ = fmt.Fprintf(out, "%s is Paused after %s; a request to it, or kubectl hybernate wake, wakes it\n",
					ref, opts.now().Sub(start).Round(time.Second))
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %w: it's still %s (%w); kubectl describe managedworkload %s -n %s shows why",
				ref, errNotPausedYet, w.Status.Phase, ctx.Err(), key.Name, key.Namespace)
		case <-ticker.C:
		}
	}
}

// pauseAnswered reports whether Hybernate's answer to the request ends the
// wait without a pause: dry-run, or a workload off already, which aren't
// failures, or a reason it won't pause, which is.
func pauseAnswered(w *v1alpha1.ManagedWorkload, ref string, requested bool, out io.Writer) (bool, error) {
	c := meta.FindStatusCondition(w.Status.Conditions, conditionPauseRequest)
	if !requested || c == nil || c.Status != metav1.ConditionFalse {
		return false, nil
	}
	why := strings.TrimPrefix(oneLine(c.Message), "not paused: ")
	switch c.Reason {
	case reasonDryRun:
		_, _ = fmt.Fprintf(out, "%s wasn't paused: %s\n", ref, why)
		return true, nil
	case reasonScaledToZero:
		_, _ = fmt.Fprintf(out, "%s is scaled to zero outside Hybernate, so it's off already\n", ref)
		return true, nil
	case reasonForecastExpectsDemand:
		// The operator's message also says how to override it through the
		// annotation, which the plugin asks about instead.
		why, _, _ = strings.Cut(why, ";")
		return true, demandExpected{msg: fmt.Sprintf("%s %s: %s", ref, errNotPaused, why)}
	}
	return true, fmt.Errorf("%s %w: %s", ref, errNotPaused, why)
}

// warnIfGitOpsUndoes warns when a GitOps tool last set the workload's
// replicas: it sets them from Git again on its next sync, which undoes the
// pause, as the operator finds from the same managed fields. The pause is
// asked for anyway. A workload that can't be read goes without the warning.
func warnIfGitOpsUndoes(ctx context.Context, c client.Client, w *v1alpha1.ManagedWorkload, ref string, out io.Writer) {
	var target client.Object = &appsv1.Deployment{}
	if w.Spec.Target.Kind == v1alpha1.TargetKindStatefulSet {
		target = &appsv1.StatefulSet{}
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Spec.Target.Name}, target); err != nil {
		return
	}
	writer, found := gitops.ReplicasWriter(target.GetManagedFields())
	if !found || !writer.FromGit() {
		return
	}
	_, _ = fmt.Fprintf(out, "Warning: %s set %s's replicas last, as %s, so it will likely scale it back up on its "+
		"next sync and undo this pause. To have %s leave replicas to Hybernate, once:\n",
		writer.Tool, ref, writer.Manager, writer.Tool)
	for _, line := range gitops.Fix(writer.Tool) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}
	_, _ = fmt.Fprintf(out, "See %s\n", gitOpsGuide)
}

// askYesNo asks a question on out and reads the answer from in, taking
// only y or yes as yes.
func askYesNo(in io.Reader, out io.Writer) func(string) (bool, error) {
	reader := bufio.NewReader(in)
	return func(question string) (bool, error) {
		_, _ = fmt.Fprint(out, question)
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}
