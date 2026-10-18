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
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// operator stands in for Hybernate: once the plugin has read the
// ManagedWorkload to find it, every later read shows answer applied, as
// the operator would have by then.
func operator(answer func(w *v1alpha1.ManagedWorkload)) interceptor.Funcs {
	reads := 0
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if w, ok := obj.(*v1alpha1.ManagedWorkload); ok {
			reads++
			if reads > 1 {
				answer(w)
			}
		}
		return nil
	}}
}

// pauses answers a request the way the operator does when it pauses the
// workload.
func pauses(w *v1alpha1.ManagedWorkload) {
	w.Status.Phase = v1alpha1.PhasePaused
	answer(w, metav1.ConditionTrue, "Pausing", "pausing as requested")
}

// refuses answers a request the way the operator does when it won't pause
// the workload, leaving it in phase.
func refuses(phase v1alpha1.WorkloadPhase, reason, message string) func(*v1alpha1.ManagedWorkload) {
	return func(w *v1alpha1.ManagedWorkload) {
		w.Status.Phase = phase
		answer(w, metav1.ConditionFalse, reason, message)
	}
}

func answer(w *v1alpha1.ManagedWorkload, status metav1.ConditionStatus, reason, message string) {
	if token := w.Annotations[v1alpha1.AnnotationPauseRequested]; token != "" {
		w.Status.LastPauseRequest = token
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: "PauseRequest", Status: status,
		Reason: reason, Message: message})
}

func pauseRequested(t *testing.T, c client.Client) string {
	t.Helper()
	return annotations(t, c)[v1alpha1.AnnotationPauseRequested]
}

// Whatever phase the workload is in, pause says what happens and waits
// until it's Paused. One paused or pausing already isn't asked again.
func TestPause_FromEachPhase(t *testing.T) {
	const paused = "preview-42/api is Paused after 0s; a request to it, or kubectl hybernate wake, wakes it\n"
	tests := []struct {
		phase       v1alpha1.WorkloadPhase
		want        string
		wantRequest bool
	}{
		{phase: v1alpha1.PhaseRunning, want: "pausing preview-42/api...\n" + paused, wantRequest: true},
		{phase: v1alpha1.PhaseIdle, want: "preview-42/api was idle; pausing it now...\n" + paused, wantRequest: true},
		{phase: v1alpha1.PhaseResuming, want: "preview-42/api is waking; it pauses once it's up...\n" + paused,
			wantRequest: true},
		{phase: v1alpha1.PhasePausing, want: "preview-42/api is already pausing...\n" + paused},
		{phase: v1alpha1.PhasePaused,
			want: "preview-42/api is already paused; a request to it, or kubectl hybernate wake, wakes it\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			c := newClient(t, operator(pauses), managedWorkload(tt.phase))
			before := time.Now()

			out, err := runCLI(t, c, "pause", "api")

			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
			token := pauseRequested(t, c)
			if !tt.wantRequest {
				assert.Empty(t, token, "nothing is asked of a workload that's pausing already")
				return
			}
			requested, err := time.Parse(time.RFC3339Nano, token)
			require.NoError(t, err, "the token is the time of the request")
			assert.False(t, requested.Before(before.Truncate(time.Second)))
		})
	}
}

// Each request gets a token of its own, so Hybernate acts on a pause asked
// for again after the last one woke.
func TestPause_EachRequestIsNew(t *testing.T) {
	w := managedWorkload(v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: "2026-10-01T18:00:00Z"}
	w.Status.LastPauseRequest = "2026-10-01T18:00:00Z"
	c := newClient(t, interceptor.Funcs{}, w)

	_, err := runCLI(t, c, "pause", "api", "--wait=false")

	require.NoError(t, err)
	assert.NotEqual(t, "2026-10-01T18:00:00Z", pauseRequested(t, c))
}

func TestPause_NoWait(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhaseRunning))

	out, err := runCLI(t, c, "pause", "api", "--wait=false")

	require.NoError(t, err)
	assert.Equal(t, "pausing preview-42/api\n", out)
	assert.NotEmpty(t, pauseRequested(t, c))
}

// Hybernate's answer when it doesn't pause: dry-run and a workload off
// already aren't failures; a reason it won't pause is.
func TestPause_Answers(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		answer   func(*v1alpha1.ManagedWorkload)
		want     string
		wantErr  string
		wantStep string
	}{
		{
			name: "dry-run", dryRun: true,
			answer: refuses(v1alpha1.PhaseIdle, "DryRun",
				"not paused: dry-run is on, so Hybernate counts a would-be pause and leaves the workload running"),
			wantStep: "preview-42/api is in dry-run, so Hybernate counts a would-be pause and leaves it running...\n",
			want: "preview-42/api wasn't paused: dry-run is on, so Hybernate counts a would-be pause and leaves " +
				"the workload running\n",
		},
		{
			name: "scaled to zero",
			answer: refuses(v1alpha1.PhaseRunning, "ScaledToZero",
				"not paused: it's scaled to zero outside Hybernate, so it's off already"),
			wantStep: "pausing preview-42/api...\n",
			want:     "preview-42/api is scaled to zero outside Hybernate, so it's off already\n",
		},
		{
			name: "protected namespace",
			answer: refuses(v1alpha1.PhaseRunning, "Protected",
				"not paused: namespace preview-42 is protected, so Hybernate doesn't pause workloads in it"),
			wantStep: "pausing preview-42/api...\n",
			wantErr: "preview-42/api wasn't paused: namespace preview-42 is protected, so Hybernate doesn't pause " +
				"workloads in it",
		},
		{
			name:     "awake dependents",
			answer:   refuses(v1alpha1.PhaseRunning, "HeldByDependents", "not paused: kept awake for preview-42/web"),
			wantStep: "pausing preview-42/api...\n",
			wantErr:  "preview-42/api wasn't paused: kept awake for preview-42/web",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := managedWorkload(v1alpha1.PhaseRunning)
			w.Spec.DryRun = tt.dryRun
			c := newClient(t, operator(tt.answer), w)

			out, err := runCLI(t, c, "pause", "api")

			if tt.wantErr != "" {
				require.ErrorIs(t, err, errNotPaused)
				assert.EqualError(t, err, tt.wantErr)
				assert.Equal(t, tt.wantStep, out[:len(tt.wantStep)])
				assert.NotContains(t, out, "Usage:")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantStep+tt.want, out)
		})
	}
}

// An earlier request's answer, still in status, isn't taken for this one's.
func TestPause_WaitsForThisRequestsAnswer(t *testing.T) {
	w := managedWorkload(v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: "an-earlier-request"}
	w.Status.LastPauseRequest = "an-earlier-request"
	answer(w, metav1.ConditionFalse, "Protected", "not paused: namespace preview-42 is protected")
	c := newClient(t, interceptor.Funcs{}, w)

	_, err := runCLI(t, c, "pause", "api", "--timeout", "300ms")

	require.ErrorIs(t, err, errNotPausedYet)
	assert.NotErrorIs(t, err, errNotPaused)
}

func TestPause_TimesOut(t *testing.T) {
	c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhaseRunning))
	start := time.Now()

	_, err := runCLI(t, c, "pause", "api", "--timeout", "300ms")

	require.ErrorIs(t, err, errNotPausedYet)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "preview-42/api isn't Paused yet: it's still Running")
	assert.Contains(t, err.Error(), "kubectl describe managedworkload api -n preview-42")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// The wait running out during a poll's read is the workload not being
// Paused yet, with the phase last seen, not the API server failing to
// answer.
func TestPause_TimesOutDuringARead(t *testing.T) {
	c := newClient(t, slowReads(), managedWorkload(v1alpha1.PhaseRunning))

	_, err := runCLI(t, c, "pause", "api", "--timeout", "100ms")

	require.ErrorIs(t, err, errNotPausedYet)
	assert.Contains(t, err.Error(), "preview-42/api isn't Paused yet: it's still Running")
	assert.NotContains(t, err.Error(), "API server")
}

func TestPause_NotFound(t *testing.T) {
	_, err := runCLI(t, newClient(t, interceptor.Funcs{}), "pause", "api")

	assert.True(t, apierrors.IsNotFound(err))
}

// Pause takes the same names as wake: the workload, kind and workload, or
// the ManagedWorkload, and refuses a bare name that means more than one.
func TestPause_Names(t *testing.T) {
	handWritten := managedWorkload(v1alpha1.PhaseRunning)
	handWritten.Name = "api-mw"
	postgres := managedWorkload(v1alpha1.PhaseRunning)
	postgres.Name = "db"
	postgres.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}

	for arg, want := range map[string]string{"api": "api-mw", "statefulset/postgres": "db", "db": "db"} {
		t.Run(arg, func(t *testing.T) {
			c := newClient(t, interceptor.Funcs{}, handWritten.DeepCopy(), postgres.DeepCopy())

			_, err := runCLI(t, c, "pause", arg, "--wait=false")

			require.NoError(t, err)
			var w v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: want}, &w))
			assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationPauseRequested])
		})
	}

	t.Run("ambiguous", func(t *testing.T) {
		statefulSet := managedWorkload(v1alpha1.PhaseRunning)
		statefulSet.Name = "api-sts"
		statefulSet.Spec.Target.Kind = v1alpha1.TargetKindStatefulSet
		c := newClient(t, interceptor.Funcs{}, handWritten.DeepCopy(), statefulSet)

		_, err := runCLI(t, c, "pause", "api")

		require.ErrorIs(t, err, errAmbiguous)
		assert.ErrorContains(t, err, "name one by its workload's kind and name, such as deployment/api")
	})
}

// A workload whose replicas Argo CD or Flux set from Git is paused anyway,
// with a warning that the tool will likely undo it, and the one-time fix.
// One a person scaled last gets no warning.
func TestPause_WarnsWhenGitOpsWillUndoIt(t *testing.T) {
	tests := []struct {
		manager, tool, fix string
	}{
		{manager: "argocd-controller", tool: "Argo CD", fix: "resource.customizations.ignoreDifferences.apps_Deployment"},
		{manager: "kustomize-controller", tool: "Flux", fix: "Leave replicas out of the workload's manifest in Git"},
		{manager: "kubectl-edit"},
	}
	for _, tt := range tests {
		t.Run(tt.manager, func(t *testing.T) {
			replicasSetBy := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if d, ok := obj.(*appsv1.Deployment); ok {
					d.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: tt.manager,
						Operation: metav1.ManagedFieldsOperationApply, FieldsType: "FieldsV1",
						FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:replicas":{}}}`)}}
				}
				return nil
			}}
			api := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "preview-42"}}
			c := newClient(t, replicasSetBy, managedWorkload(v1alpha1.PhaseRunning), api)

			out, err := runCLI(t, c, "pause", "api", "--wait=false")

			require.NoError(t, err)
			assert.NotEmpty(t, pauseRequested(t, c), "it's paused anyway")
			if tt.tool == "" {
				assert.Equal(t, "pausing preview-42/api\n", out)
				return
			}
			assert.Contains(t, out, "Warning: "+tt.tool+" set preview-42/api's replicas last, as "+tt.manager+
				", so it will likely scale it back up on its next sync and undo this pause.")
			assert.Contains(t, out, tt.fix)
			assert.Contains(t, out, "See https://okedeji.io/hybernate/guides/gitops/")
		})
	}
}

// expectsDemand answers a request the way the operator does when the
// forecast expects the workload to be busy within the hour: it declines one
// that doesn't override the forecast, and pauses one that does.
func expectsDemand(w *v1alpha1.ManagedWorkload) {
	if w.Annotations[v1alpha1.AnnotationPauseOverridesForecast] == v1alpha1.True {
		pauses(w)
		return
	}
	refuses(v1alpha1.PhaseRunning, "ForecastExpectsDemand", "not paused: the forecast expects demand at 80% of "+
		"requests in the hour from 09:00 UTC; ask again overriding the forecast to pause anyway")(w)
}

// When the forecast expects the workload to be busy soon, pause says when
// and asks; it pauses only if the answer is yes, and without anyone to ask,
// says how to pause it anyway.
func TestPause_AsksWhenTheForecastExpectsDemand(t *testing.T) {
	const declined = "preview-42/api wasn't paused: the forecast expects demand at 80% of requests in the hour " +
		"from 09:00 UTC"
	tests := []struct {
		name         string
		confirm      func(question string) (bool, error)
		wantErr      string
		wantOut      string
		wantPaused   bool
		wantOverride bool
	}{
		{name: "no one to ask", wantErr: declined + "; pass --yes to pause it anyway"},
		{name: "answered no", confirm: func(string) (bool, error) { return false, nil },
			wantOut: "left preview-42/api running\n"},
		{name: "answered yes", confirm: func(string) (bool, error) { return true, nil },
			wantOut: "preview-42/api is Paused after", wantPaused: true, wantOverride: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, operator(expectsDemand), managedWorkload(v1alpha1.PhaseRunning))
			var asked string
			opts := pauseOptions{wait: true, pollInterval: time.Millisecond, now: time.Now}
			if tt.confirm != nil {
				opts.confirm = func(question string) (bool, error) {
					asked = question
					return tt.confirm(question)
				}
			}
			var out bytes.Buffer

			err := pause(stepsOn(c, 5*time.Second), apiKey, opts, &out, io.Discard)

			if tt.wantErr != "" {
				require.ErrorIs(t, err, errNotPaused)
				assert.EqualError(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Contains(t, out.String(), tt.wantOut)
			}
			if tt.confirm != nil {
				assert.Equal(t, declined+"\nPause it anyway? [y/N] ", asked)
			}
			got := annotations(t, c)[v1alpha1.AnnotationPauseOverridesForecast] == v1alpha1.True
			assert.Equal(t, tt.wantOverride, got, "the forecast is overridden only when asked to")
			assert.Equal(t, tt.wantPaused, strings.Contains(out.String(), "is Paused"))
		})
	}
}

// stepsOn runs each step against c, within timeout.
func stepsOn(c client.Client, timeout time.Duration) clusterSteps {
	return clusterSteps{parent: context.Background(), timeout: timeout,
		connect: func(context.Context) (client.Client, error) { return c, nil }}
}

// failsOnceDone stands in for a real client, which fails every call made
// with a context that's done.
func failsOnceDone(funcs interceptor.Funcs) interceptor.Funcs {
	get := funcs.Get
	funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return get(ctx, c, key, obj, opts...)
	}
	return funcs
}

// The time taken to answer isn't the cluster's: an answer that comes after
// --timeout has passed still pauses the workload, within a --timeout of its
// own.
func TestPause_AnswerAfterTheTimeout(t *testing.T) {
	c := newClient(t, failsOnceDone(operator(expectsDemand)), managedWorkload(v1alpha1.PhaseRunning))
	const timeout = 200 * time.Millisecond
	opts := pauseOptions{wait: true, pollInterval: time.Millisecond, now: time.Now,
		confirm: func(string) (bool, error) { time.Sleep(2 * timeout); return true, nil }}
	var out bytes.Buffer

	err := pause(stepsOn(c, timeout), apiKey, opts, &out, io.Discard)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "preview-42/api is Paused after")
}

// An active-until hold on the ManagedWorkload, as wake --for sets, would
// have Hybernate refuse the pause. pause asks whether to clear it, clears it
// without asking with --clear-hold, and without anyone to ask says how. A
// hold that has ended is no hold.
func TestPause_HeldAwake(t *testing.T) {
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name      string
		until     string
		args      []string
		confirm   func(string) (bool, error)
		wantErr   string
		wantOut   string
		wantHold  bool
		wantPause bool
	}{
		{name: "no one to ask", until: until, wantHold: true,
			wantErr: "preview-42/api wasn't paused: the hybernate.io/active-until annotation on its ManagedWorkload " +
				"keeps it awake until " + until + "; pass --clear-hold to clear it and pause now"},
		{name: "--clear-hold", until: until, args: []string{"--clear-hold"}, wantPause: true,
			wantOut: "cleared the hybernate.io/active-until hold on preview-42/api\npausing preview-42/api...\n"},
		{name: "answered yes", until: until, confirm: func(string) (bool, error) { return true, nil },
			wantPause: true, wantOut: "cleared the hybernate.io/active-until hold on preview-42/api\n"},
		{name: "answered no", until: until, confirm: func(string) (bool, error) { return false, nil },
			wantHold: true, wantOut: "left preview-42/api running\n"},
		{name: "a hold that has ended", until: "2026-01-01T00:00:00Z", wantHold: true, wantPause: true,
			wantOut: "pausing preview-42/api...\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := managedWorkload(v1alpha1.PhaseRunning)
			w.Annotations = map[string]string{v1alpha1.AnnotationActiveUntil: tt.until}
			c := newClient(t, operator(pauses), w)
			var out string
			var err error
			if tt.confirm != nil {
				var buf bytes.Buffer
				opts := pauseOptions{wait: true, pollInterval: time.Millisecond, now: time.Now, confirm: tt.confirm}
				err = pause(stepsOn(c, 5*time.Second), apiKey, opts, &buf, io.Discard)
				out = buf.String()
			} else {
				out, err = runCLI(t, c, append([]string{"pause", "api"}, tt.args...)...)
			}

			if tt.wantErr != "" {
				require.ErrorIs(t, err, errNotPaused)
				assert.EqualError(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Contains(t, out, tt.wantOut)
			}
			got := annotations(t, c)
			_, held := got[v1alpha1.AnnotationActiveUntil]
			assert.Equal(t, tt.wantHold, held)
			assert.Equal(t, tt.wantPause, got[v1alpha1.AnnotationPauseRequested] != "")
		})
	}
}

// The operator also honours an active-until hold on the workload itself, so
// pause finds that one too, and clears whichever holds it.
func TestPause_HeldAwakeByTheWorkload(t *testing.T) {
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name      string
		onManaged bool
		args      []string
		wantErr   string
	}{
		{name: "no one to ask", wantErr: "preview-42/api wasn't paused: the hybernate.io/active-until annotation on " +
			"deployment api keeps it awake until " + until + "; pass --clear-hold to clear it and pause now"},
		{name: "held by both", onManaged: true, wantErr: "preview-42/api wasn't paused: the hybernate.io/active-until " +
			"annotation on its ManagedWorkload and deployment api keeps it awake until " + until +
			"; pass --clear-hold to clear it and pause now"},
		{name: "--clear-hold", onManaged: true, args: []string{"--clear-hold"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := managedWorkload(v1alpha1.PhaseRunning)
			if tt.onManaged {
				w.Annotations = map[string]string{v1alpha1.AnnotationActiveUntil: until}
			}
			api := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: apiKey.Namespace,
				Annotations: map[string]string{v1alpha1.AnnotationActiveUntil: until}}}
			c := newClient(t, operator(pauses), w, api)

			out, err := runCLI(t, c, append([]string{"pause", "api"}, tt.args...)...)

			var got appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(api), &got))
			_, workloadHeld := got.Annotations[v1alpha1.AnnotationActiveUntil]
			_, managedHeld := annotations(t, c)[v1alpha1.AnnotationActiveUntil]
			if tt.wantErr != "" {
				require.ErrorIs(t, err, errNotPaused)
				assert.EqualError(t, err, tt.wantErr)
				assert.True(t, workloadHeld, "nothing is cleared without asking")
				assert.Empty(t, pauseRequested(t, c))
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out, "cleared the hybernate.io/active-until hold on preview-42/api\n")
			assert.Contains(t, out, "preview-42/api is Paused after")
			assert.False(t, workloadHeld)
			assert.False(t, managedHeld)
		})
	}
}

// --yes overrides the forecast with the first request, without asking.
func TestPause_YesOverridesTheForecast(t *testing.T) {
	c := newClient(t, operator(expectsDemand), managedWorkload(v1alpha1.PhaseRunning))

	out, err := runCLI(t, c, "pause", "api", "--yes")

	require.NoError(t, err)
	assert.NotContains(t, out, "Pause it anyway?")
	assert.Contains(t, out, "preview-42/api is Paused after")
	assert.Equal(t, v1alpha1.True, annotations(t, c)[v1alpha1.AnnotationPauseOverridesForecast])
}

// An override asked for with an earlier request doesn't carry over to the
// next one, which asks again.
func TestPause_OverrideIsForOneRequest(t *testing.T) {
	w := managedWorkload(v1alpha1.PhaseRunning)
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseOverridesForecast: v1alpha1.True}
	c := newClient(t, operator(expectsDemand), w)

	_, err := runCLI(t, c, "pause", "api")

	require.ErrorIs(t, err, errNotPaused)
	assert.NotContains(t, annotations(t, c), v1alpha1.AnnotationPauseOverridesForecast)
}

func TestAskYesNo(t *testing.T) {
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, " yes \n": true, "n\n": false,
		"\n": false, "": false, "sure\n": false} {
		var out bytes.Buffer
		got, err := askYesNo(strings.NewReader(answer), &out)("Pause it anyway? [y/N] ")
		require.NoError(t, err)
		assert.Equal(t, want, got, "answer %q", answer)
		assert.Equal(t, "Pause it anyway? [y/N] ", out.String())
	}
}
