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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var statusNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func before(d time.Duration) *metav1.Time {
	t := metav1.NewTime(statusNow.Add(-d))
	return &t
}

func statusWorkloadObj(namespace, name string, kind v1alpha1.TargetKind, phase v1alpha1.WorkloadPhase,
	inPhase time.Duration) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: kind, Name: name}},
		Status:     v1alpha1.ManagedWorkloadStatus{Phase: phase, LastTransitionTime: before(inPhase)},
	}
}

func activity(source v1alpha1.ActivitySource, at time.Duration, pauseIn time.Duration) *v1alpha1.ActivityStatus {
	pauseAt := metav1.NewTime(statusNow.Add(pauseIn))
	return &v1alpha1.ActivityStatus{LastActivityTime: *before(at), LastActivitySource: source, PauseAt: &pauseAt}
}

func condition(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
}

func statusEvent(namespace, workload, reason, message string, at time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: workload + "." + reason + "." + at.String(), Namespace: namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "ManagedWorkload", Namespace: namespace, Name: workload},
		Reason:         reason,
		Message:        message,
		EventTime:      metav1.NewMicroTime(statusNow.Add(-at)),
	}
}

func statusCluster() []client.Object {
	api := statusWorkloadObj("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused, 3*time.Hour)
	api.Status.Doorman = []v1alpha1.DoormanRoute{{Service: "api", DoormanPort: 20000}}
	api.Status.Activity = activity(v1alpha1.ActivitySourceRequest, 4*time.Hour, -3*time.Hour)
	api.Status.Cost = &v1alpha1.CostStatus{SavedThisMonth: "$12.40"}

	postgres := statusWorkloadObj("preview-42", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning,
		5*time.Hour)
	postgres.Status.Activity = activity(v1alpha1.ActivitySourceCPU, 2*time.Hour, -time.Hour)
	postgres.Status.Conditions = []metav1.Condition{
		condition("HeldByDependents", metav1.ConditionTrue, "DependentsAwake", "held awake for preview-42/worker"),
	}
	postgres.Status.Cost = &v1alpha1.CostStatus{SavedThisMonth: "$30.05"}

	web := statusWorkloadObj("preview-7", "web", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, 2*time.Hour)
	web.Spec.DryRun = true
	web.Status.Activity = activity(v1alpha1.ActivitySourceRollout, 15*time.Minute, 45*time.Minute)
	web.Status.Cost = &v1alpha1.CostStatus{SavedThisMonth: "$9.00"}

	worker := statusWorkloadObj("preview-7", "worker", v1alpha1.TargetKindDeployment, v1alpha1.PhaseResuming,
		25*time.Minute)
	worker.Status.Conditions = []metav1.Condition{
		condition("WaitingForDependencies", metav1.ConditionTrue, "DependencyNotReady", "waiting for preview-42/postgres"),
		condition("GitOpsConflict", metav1.ConditionTrue, "PauseUndone", "Argo CD set the replicas from Git"),
		condition("WakeOnRequest", metav1.ConditionFalse, "NoServices", "no Service routes TCP traffic to this workload"),
		condition("MetricsAvailable", metav1.ConditionTrue, "MetricsReported", ""),
	}

	return []client.Object{api, postgres, web, worker,
		statusEvent("preview-42", "api", "Paused", "api: paused", 3*time.Hour),
		statusEvent("preview-42", "api", "Resumed", "api: resumed", 5*time.Hour-time.Second),
		statusEvent("preview-42", "api", "WokeByActivity", "api: a request is waiting, waking", 5*time.Hour),
		statusEvent("preview-42", "api", "WokenByRequest", "request on Service api, waking", 5*time.Hour+time.Second),
		statusEvent("preview-7", "web", "WokeByActivity",
			"[dry-run] web: activity annotation is newer than the pause, waking", time.Hour),
		statusEvent("preview-7", "worker", "ScaledUp",
			"worker: scaled up to 2 replicas by argocd-controller outside Hybernate; counted as activity", 30*time.Minute),
		statusEvent("preview-7", "worker", "Paused", "worker: paused", 50*time.Hour),
		statusEvent("preview-42", "api", "IdleDetected", "api: no activity for 1h", 3*time.Hour+time.Minute),
	}
}

func newStatusClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).
		WithIndex(&corev1.Event{}, "involvedObject.kind", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Kind}
		}).Build()
}

func statusOpts() statusOptions {
	return statusOptions{since: 24 * time.Hour, now: func() time.Time { return statusNow }}
}

func runStatus(t *testing.T, c client.Client, opts statusOptions) string {
	t.Helper()
	result, err := clusterStatus(context.Background(), c, opts)
	require.NoError(t, err)
	result.Cluster = "staging (EKS us-east-1)"
	var out bytes.Buffer
	require.NoError(t, writeStatus(&out, result, "table", statusNow))
	return out.String()
}

func TestStatus(t *testing.T) {
	out := runStatus(t, newStatusClient(t, interceptor.Funcs{}, statusCluster()...), statusOpts())

	//nolint:lll // the expected screen is as wide as the command prints it
	assert.Equal(t, `staging (EKS us-east-1): Hybernate manages 4 workloads.
  1 paused, 2 running, 1 resuming; 1 in dry-run, measured but never paused
  Saved $42.45 this month.

Needs attention:
  preview-7/worker   GitOpsConflict   Argo CD set the replicas from Git
  preview-7/worker   Stuck            resuming for 25m: waiting for preview-42/postgres

  NAMESPACE    WORKLOAD               STATE               FOR   LAST ACTIVITY      NEXT                             SAVED THIS MONTH
  preview-42   deployment/api         paused              3h    request, 4h ago    wakes on a request or activity   $12.40
  preview-42   statefulset/postgres   running             5h    cpu, 2h ago        held awake by its dependents     $30.05
  preview-7    deployment/web         running (dry-run)   2h    rollout, 15m ago   would pause in 45m               -
  preview-7    deployment/worker      resuming            25m   -                  -                                -

Recent pauses and wakes:
  30m ago   preview-7/worker   scaled up to 2 replicas by argocd-controller outside Hybernate; counted as activity
  3h ago    preview-42/api     paused
  5h ago    preview-42/api     request on Service api, waking
`, out)
}

func TestNextFor(t *testing.T) {
	paused := v1alpha1.DesiredStatePaused
	tests := []struct {
		name   string
		mutate func(*v1alpha1.ManagedWorkload)
		want   string
	}{
		{name: "pauses after idleAfter", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Status.Activity = activity(v1alpha1.ActivitySourceCPU, 0, 90*time.Minute)
		}, want: "pauses in 1h"},
		{name: "idle, about to pause", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Status.Phase = v1alpha1.PhaseIdle
			w.Status.Activity = activity(v1alpha1.ActivitySourceCPU, 2*time.Hour, -time.Hour)
		}, want: "pauses now"},
		{name: "kept by desiredState", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Spec.DesiredState = &paused
		}, want: "kept paused by desiredState"},
		{name: "held by active-until", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Annotations = map[string]string{v1alpha1.AnnotationActiveUntil: statusNow.Add(2 * time.Hour).Format(time.RFC3339)}
			w.Status.Activity = activity(v1alpha1.ActivitySourceCPU, 2*time.Hour, -time.Hour)
		}, want: "held awake for 2h"},
		{name: "an expired active-until doesn't hold", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Annotations = map[string]string{v1alpha1.AnnotationActiveUntil: statusNow.Add(-time.Hour).Format(time.RFC3339)}
			w.Status.Activity = activity(v1alpha1.ActivitySourceCPU, 0, 10*time.Minute)
		}, want: "pauses in 10m"},
		{name: "paused without the doorman", mutate: func(w *v1alpha1.ManagedWorkload) {
			w.Status.Phase = v1alpha1.PhasePaused
		}, want: "wakes on activity"},
		{name: "no activity clock yet", mutate: func(*v1alpha1.ManagedWorkload) {}, want: "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := statusWorkloadObj("ns", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, time.Hour)
			tt.mutate(w)

			assert.Equal(t, tt.want, nextFor(w, statusNow))
		})
	}
}

// A pause or wake that's taking a few minutes isn't a problem yet.
func TestProblemsOf_NotStuckYet(t *testing.T) {
	w := statusWorkloadObj("ns", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePausing, 2*time.Minute)

	assert.Empty(t, problemsOf(w, statusNow))
}

func TestStatus_NothingManaged(t *testing.T) {
	out := runStatus(t, newStatusClient(t, interceptor.Funcs{}), statusOpts())

	assert.Equal(t, "staging (EKS us-east-1): Hybernate manages no workloads here.\n"+
		"kubectl hybernate scan finds idle ones; label one hybernate.io/managed=true for Hybernate to manage it.\n", out)
}

func TestStatus_Namespaces(t *testing.T) {
	opts := statusOpts()
	opts.namespaces = []string{"preview-7"}

	out := runStatus(t, newStatusClient(t, interceptor.Funcs{}, statusCluster()...), opts)

	assert.Contains(t, out, "Hybernate manages 2 workloads.")
	assert.NotContains(t, out, "deployment/api")
	assert.NotContains(t, out, "preview-42/api")
}

func TestStatus_Since(t *testing.T) {
	opts := statusOpts()
	opts.since = time.Hour

	out := runStatus(t, newStatusClient(t, interceptor.Funcs{}, statusCluster()...), opts)

	assert.Contains(t, out, "30m ago   preview-7/worker")
	assert.NotContains(t, out, "3h ago")
}

func TestStatus_EventsForbidden(t *testing.T) {
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*corev1.EventList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", nil)
		}
		return c.List(ctx, list, opts...)
	}}

	out := runStatus(t, newStatusClient(t, funcs, statusCluster()...), statusOpts())

	assert.Contains(t, out, "Recent pauses and wakes aren't shown: your access doesn't allow reading events.")
}

func TestStatus_JSON(t *testing.T) {
	result, err := clusterStatus(context.Background(), newStatusClient(t, interceptor.Funcs{}, statusCluster()...),
		statusOpts())
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, writeStatus(&out, result, "json", statusNow))

	var got statusResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Len(t, got.Workloads, 4)
	assert.Len(t, got.Problems, 2)
	assert.Len(t, got.Recent, 3)
	assert.InDelta(t, 42.45, got.SavedThisMonth, 0.001)
}

// A workload opted in with its label has a ManagedWorkload named after its
// kind; status names it by the workload itself everywhere.
func TestStatus_NamedByTarget(t *testing.T) {
	w := statusWorkloadObj("shop", "deployment-api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, time.Hour)
	w.Spec.Target.Name = "api"
	w.Status.Conditions = []metav1.Condition{
		condition("MetricsAvailable", metav1.ConditionFalse, "MetricsUnavailable", "metrics-server isn't answering"),
	}

	out := runStatus(t, newStatusClient(t, interceptor.Funcs{}, w,
		statusEvent("shop", "deployment-api", "Paused", "api: paused", time.Minute)), statusOpts())

	assert.Contains(t, out, "shop/api   MetricsAvailable   metrics-server isn't answering")
	assert.Contains(t, out, "1m ago   shop/api   paused")
	assert.NotContains(t, out, "deployment-api")
}
