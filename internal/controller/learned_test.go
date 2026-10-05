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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// apiWith is default/api with env in its container.
func apiWith(annotations map[string]string, env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Annotations: annotations},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "api:v1", Env: env}}},
		}},
	}
}

// databases are default/postgres and default/redis, each behind a Service.
func databases() []client.Object {
	objs := make([]client.Object, 0, 5)
	for _, name := range []string{"postgres", "redis"} {
		labels := map[string]string{"app": name}
		objs = append(objs,
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: corev1.ServiceSpec{Selector: labels}})
	}
	objs = append(objs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "postgres-hl", Namespace: "default"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "postgres"}, ClusterIP: corev1.ClusterIPNone}})
	return objs
}

func learnedNames(w *v1alpha1.ManagedWorkload) []string {
	if w.Status.LearnedDependencies == nil {
		return nil
	}
	var out []string
	for _, d := range w.Status.LearnedDependencies.Dependencies {
		out = append(out, d.Namespace+"/"+d.Name+" "+d.Via)
	}
	return out
}

func TestLearnDependencies(t *testing.T) {
	databaseURL := corev1.EnvVar{Name: "DATABASE_URL", Value: "postgres://app:secret@postgres:5432/app"}
	redisURL := corev1.EnvVar{Name: "REDIS_URL", Value: "redis://redis:6379"}
	tests := []struct {
		name        string
		target      *appsv1.Deployment
		objs        []client.Object
		mwIgnore    string
		want        []string
		wantAddress string
	}{
		{name: "an address in a variable", target: apiWith(nil, databaseURL),
			want: []string{"default/postgres DATABASE_URL"}, wantAddress: "postgres://***@postgres:5432/app"},
		{name: "a headless address from a ConfigMap", target: func() *appsv1.Deployment {
			d := apiWith(nil)
			d.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
				{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "api-config"}}}}
			return d
		}(), objs: []client.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "api-config", Namespace: "default"},
			Data: map[string]string{"PGHOST": "postgres-0.postgres-hl"}}},
			want: []string{"default/postgres PGHOST"}},
		{name: "ignored on the workload", target: apiWith(
			map[string]string{v1alpha1.AnnotationIgnoreDependencies: "redis"}, databaseURL, redisURL),
			want: []string{"default/postgres DATABASE_URL"}},
		{name: "ignored on the ManagedWorkload, by namespace", target: apiWith(nil, databaseURL, redisURL),
			mwIgnore: "default/postgres", want: []string{"default/redis REDIS_URL"}},
		{name: "nothing to learn", target: apiWith(nil, corev1.EnvVar{Name: "MODE", Value: "postgres"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			if tt.mwIgnore != "" {
				api.Annotations = map[string]string{v1alpha1.AnnotationIgnoreDependencies: tt.mwIgnore}
			}
			objs := append(append(databases(), tt.objs...), api, tt.target)
			r := depReconciler(t, &stubPauser{}, objs...)

			require.NoError(t, r.learnDependencies(context.Background(), api, tt.target))

			assert.Equal(t, tt.want, learnedNames(api))
			if tt.wantAddress != "" {
				assert.Equal(t, tt.wantAddress, api.Status.LearnedDependencies.Dependencies[0].Address,
					"passwords hidden")
			}
		})
	}
}

// Learning reads the cluster again only when what it learns from changes,
// or after an hour, for a ConfigMap edited since.
func TestLearnDependencies_WhenToRelearn(t *testing.T) {
	target := apiWith(nil, corev1.EnvVar{Name: "DATABASE_URL", Value: "postgres:5432"})
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	r := depReconciler(t, &stubPauser{}, append(databases(), api, target)...)
	require.NoError(t, r.learnDependencies(context.Background(), api, target))
	require.Equal(t, []string{"default/postgres DATABASE_URL"}, learnedNames(api))
	learnedAt := api.Status.LearnedDependencies.At

	r.clock = func() time.Time { return fixedTime.Add(30 * time.Minute) }
	require.NoError(t, r.learnDependencies(context.Background(), api, target))
	assert.Equal(t, learnedAt, api.Status.LearnedDependencies.At, "nothing changed")

	changed := target.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Env[0].Value = "redis:6379"
	require.NoError(t, r.learnDependencies(context.Background(), api, changed))
	assert.Equal(t, []string{"default/redis DATABASE_URL"}, learnedNames(api), "the template changed")
	relearnedAt := api.Status.LearnedDependencies.At

	r.clock = func() time.Time { return fixedTime.Add(2 * time.Hour) }
	require.NoError(t, r.learnDependencies(context.Background(), api, changed))
	assert.True(t, api.Status.LearnedDependencies.At.After(relearnedAt.Time), "an hour on")
}

func TestDependencyRefs(t *testing.T) {
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
		v1alpha1.DependencyRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", WaitForReady: true})
	api.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
		{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"},
		{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats"},
	}}

	assert.Equal(t, []v1alpha1.DependencyRef{
		{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", WaitForReady: true},
		{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats"},
	}, dependencyRefs(api), "dependsOn first, learned ones it doesn't already name")
}

// A learned dependency is held awake and woken like a declared one.
func TestLearnedDependencies_HoldAndWake(t *testing.T) {
	learnedPostgres := &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
		{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "PGHOST"}}}

	t.Run("held", func(t *testing.T) {
		postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning)
		api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
		api.Status.LearnedDependencies = learnedPostgres
		pauser := &stubPauser{pauseDone: true}
		r := depReconciler(t, pauser, postgres, api)

		_, err := r.reconcileAutomation(context.Background(), postgres, postgresTarget(1, 1))
		require.NoError(t, err)

		assert.Zero(t, pauser.pauseCalls)
		assert.True(t, meta.IsStatusConditionTrue(postgres.Status.Conditions, conditionHeldByDependents))
	})
	t.Run("woken", func(t *testing.T) {
		pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
		api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused)
		api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
		api.Status.LearnedDependencies = learnedPostgres
		postgres := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
		r := depReconciler(t, &stubPauser{resumeDone: true}, api, postgres, postgresTarget(1, 1))

		_, err := r.handleResume(context.Background(), api, nil)
		require.NoError(t, err)

		assert.Equal(t, fixedTime.UTC().Format(time.RFC3339),
			fetch(t, r, "postgres").Annotations[v1alpha1.AnnotationLastActivity])
	})
}

// wakeReconciler is a cluster where a request from senderIP woke postgres.
func wakeReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	r := depReconciler(t, &stubPauser{})
	r.Client = fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, podIPField, func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Status.PodIP}
		}).Build()
	return r
}

const senderIP = "10.244.1.7"

// podOf is a pod of app at senderIP.
func podOf(app string, hostNetwork bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: app + "-7d9f-x2", Namespace: "default", Labels: map[string]string{"app": app}},
		Spec:       corev1.PodSpec{HostNetwork: hostNetwork},
		Status:     corev1.PodStatus{PodIP: senderIP},
	}
}

func selecting(kind v1alpha1.TargetKind, name string, annotations map[string]string) client.Object {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	objectMeta := metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations}
	if kind == v1alpha1.TargetKindStatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: objectMeta, Spec: appsv1.StatefulSetSpec{Selector: selector}}
	}
	return &appsv1.Deployment{ObjectMeta: objectMeta, Spec: appsv1.DeploymentSpec{Selector: selector}}
}

// wokenPostgres is postgres, paused an hour ago and just woken by a request
// from senderIP.
func wokenPostgres() *v1alpha1.ManagedWorkload {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	w := depWorkload("default", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused)
	w.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt}
	w.Annotations = map[string]string{
		v1alpha1.AnnotationLastRequest:     fixedTime.Add(-time.Second).UTC().Format(time.RFC3339),
		v1alpha1.AnnotationLastRequestFrom: senderIP,
	}
	return w
}

func TestLearnFromWake(t *testing.T) {
	learnedPostgres := []string{"default/postgres "}
	tests := []struct {
		name     string
		postgres *v1alpha1.ManagedWorkload
		objs     []client.Object
		api      func(*v1alpha1.ManagedWorkload)
		want     []string
	}{
		{name: "a request from a workload Hybernate manages", postgres: wokenPostgres(),
			objs: []client.Object{podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)},
			want: learnedPostgres},
		{name: "already learned", postgres: wokenPostgres(),
			objs: []client.Object{podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)},
			api: func(w *v1alpha1.ManagedWorkload) {
				w.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
					{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
						Source: v1alpha1.LearnedFromEnvironment, Via: "PGHOST"}}}
			},
			want: []string{"default/postgres PGHOST"}},
		{name: "ignored", postgres: wokenPostgres(), objs: []client.Object{podOf("api", false),
			selecting(v1alpha1.TargetKindDeployment, "api", map[string]string{v1alpha1.AnnotationIgnoreDependencies: "postgres"})}},
		{name: "from a pod Hybernate doesn't manage, such as an ingress controller", postgres: wokenPostgres(),
			objs: []client.Object{podOf("ingress-nginx", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)}},
		{name: "from its own pod", postgres: wokenPostgres(),
			objs: []client.Object{podOf("postgres", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)}},
		{name: "from the node's network", postgres: wokenPostgres(),
			objs: []client.Object{podOf("api", true), selecting(v1alpha1.TargetKindDeployment, "api", nil)}},
		{name: "not woken by a request", postgres: func() *v1alpha1.ManagedWorkload {
			w := wokenPostgres()
			w.Annotations[v1alpha1.AnnotationLastRequest] = fixedTime.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
			return w
		}(), objs: []client.Object{podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			if tt.api != nil {
				tt.api(api)
			}
			r := wakeReconciler(t, append(tt.objs, api, tt.postgres, selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))...)

			require.NoError(t, r.learnFromWake(context.Background(), tt.postgres))

			got := fetch(t, r, "api")
			assert.Equal(t, tt.want, learnedNames(got))
			assert.Empty(t, learnedNames(fetch(t, r, "postgres")), "a workload never depends on itself")
			if tt.want != nil && tt.api == nil {
				assert.Equal(t, v1alpha1.LearnedFromWake, got.Status.LearnedDependencies.Dependencies[0].Source)
			}
		})
	}
}

// Learning from the environment again keeps what wakes taught.
func TestLearnDependencies_KeepsWhatWakesTaught(t *testing.T) {
	target := apiWith(nil, corev1.EnvVar{Name: "DATABASE_URL", Value: "postgres:5432"})
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	api.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
		{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "redis", Source: v1alpha1.LearnedFromWake}}}
	r := depReconciler(t, &stubPauser{}, append(databases(), api, target)...)

	require.NoError(t, r.learnDependencies(context.Background(), api, target))

	assert.Equal(t, []string{"default/postgres DATABASE_URL", "default/redis "}, learnedNames(api))
}

// Installed for some namespaces only, Hybernate can't list pods across the
// cluster, so it looks for the sender in each namespace it watches.
func TestLearnFromWake_WatchedNamespaces(t *testing.T) {
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	postgres := wokenPostgres()
	r := wakeReconciler(t, podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil), api, postgres,
		selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))
	r.WatchNamespaces = []string{"preview-1", "default"}

	require.NoError(t, r.learnFromWake(context.Background(), postgres))

	assert.Equal(t, []string{"default/postgres "}, learnedNames(fetch(t, r, "api")))
}

// The first dependency a workload learns may come from a wake, before it
// has learned any from its environment. What's written then must pass the
// CRD's validation, which the fake client doesn't check.
func TestLearnFromWake_FirstLearnedIsValid(t *testing.T) {
	cfg := startEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scheme := testScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	labels := map[string]string{"app": "api"}
	container := corev1.Container{Name: "app", Image: "api:v1"}
	require.NoError(t, c.Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}},
	}))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f-x2", Namespace: "default", Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}
	require.NoError(t, c.Create(ctx, pod))
	pod.Status.PodIP = senderIP
	pod.Status.PodIPs = []corev1.PodIP{{IP: senderIP}}
	require.NoError(t, c.Status().Update(ctx, pod))
	api := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85}},
	}
	require.NoError(t, c.Create(ctx, api))

	r := &Reconciler{Client: c, PodReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(20),
		clock: func() time.Time { return fixedTime }}
	require.NoError(t, r.learnFromWake(ctx, wokenPostgres()))

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(api), api))
	assert.Equal(t, []string{"default/postgres "}, learnedNames(api))
}
