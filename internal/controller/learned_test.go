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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			want: []string{"default/postgres DATABASE_URL"}, wantAddress: "postgres://postgres:5432"},
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
					"only scheme, host and port shown")
			}
		})
	}
}

// Learning lists Services and workloads only in the namespaces the
// environment could address, not across the cluster, hourly, for every
// workload. An environment naming more than it's worth listing one by one
// has the cluster listed once.
func TestLearnDependencies_ListsOnlyTheNamespacesNamed(t *testing.T) {
	labels := map[string]string{"app": "pg"}
	pg := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "data"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}}}
	pgService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "data"},
		Spec: corev1.ServiceSpec{Selector: labels}}
	many := make([]string, 0, maxNamedNamespaces+1)
	for i := range maxNamedNamespaces + 1 {
		many = append(many, fmt.Sprintf("cache.team%d:6379", i))
	}
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "an address in another namespace", value: "postgres://app@pg.data.svc.cluster.local:5432/app",
			want: []string{"data", "default"}},
		{name: "words that aren't hosts", value: "pg.data:5432 MODE=api region us-east-1 jdbc:postgresql://pg.data/app",
			want: []string{"data", "default"}},
		{name: "too many words to list one by one", value: "pg.data:5432 " + strings.Join(many, " "),
			want: []string{metav1.NamespaceAll}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := apiWith(nil, corev1.EnvVar{Name: "DATABASE_URL", Value: tt.value})
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			r := depReconciler(t, &stubPauser{}, api, target, pg, pgService)
			listed := map[string]bool{}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.ServiceList); ok {
						o := (&client.ListOptions{}).ApplyOptions(opts)
						listed[o.Namespace] = true
					}
					return c.List(ctx, list, opts...)
				}})

			require.NoError(t, r.learnDependencies(context.Background(), api, target))

			assert.Equal(t, []string{"data/pg DATABASE_URL"}, learnedNames(api))
			assert.Equal(t, tt.want, slices.Sorted(maps.Keys(listed)), "namespaces whose Services were listed")
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
	changed.ResourceVersion += "1" // as the update that changed it would
	require.NoError(t, r.learnDependencies(context.Background(), api, changed))
	assert.Equal(t, []string{"default/redis DATABASE_URL"}, learnedNames(api), "the template changed")
	relearnedAt := api.Status.LearnedDependencies.At

	api.Status.Phase = v1alpha1.PhasePaused
	r.clock = func() time.Time { return fixedTime.Add(2 * time.Hour) }
	require.NoError(t, r.learnDependencies(context.Background(), api, changed))
	assert.Equal(t, relearnedAt, api.Status.LearnedDependencies.At, "paused, nothing it runs reads the environment")

	api.Status.Phase = v1alpha1.PhaseRunning
	require.NoError(t, r.learnDependencies(context.Background(), api, changed))
	assert.True(t, api.Status.LearnedDependencies.At.After(relearnedAt.Time), "an hour on")
}

// Learning dependencies reads ConfigMaps past the cache and lists Services
// and workloads. However it fails, a paused workload a request woke still
// wakes.
func TestLearnDependencies_NeverHoldsUpAWake(t *testing.T) {
	pausedAt := metav1.NewTime(fixedTime.Add(-time.Hour))
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused)
	api.Finalizers = []string{finalizerName}
	api.Status.Pause = &v1alpha1.PauseStatus{PreviousReplicas: 1, PausedAt: &pausedAt,
		WakeAnnotations: &v1alpha1.WakeAnnotations{}}
	api.Annotations = map[string]string{v1alpha1.AnnotationLastActivity: fixedTime.UTC().Format(time.RFC3339)}
	target := apiWith(nil)
	target.Spec.Replicas = ptr.To[int32](0)
	target.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}}}
	pauser := &stubPauser{resumeDone: true}
	r := depReconciler(t, pauser, api, target)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return errors.New("the API server is overloaded")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, err := r.Reconcile(context.Background(), reconcileFor("api"))
	require.NoError(t, err)

	assert.Equal(t, 1, pauser.resumeCalls, "the wake went ahead")
	assert.Equal(t, v1alpha1.PhaseRunning, fetch(t, r, "api").Status.Phase)
}

// What's learned is announced once it's recorded, so a write that fails
// and is retried doesn't announce it twice.
func TestLearnDependencies_AnnouncedOnceRecorded(t *testing.T) {
	target := apiWith(nil, corev1.EnvVar{Name: "DATABASE_URL", Value: "postgres:5432"})
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	r := depReconciler(t, &stubPauser{}, append(databases(), api, target)...)
	fail := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if fail {
				return errors.New("conflict")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}})
	recorder := r.Recorder.(*events.FakeRecorder)
	learnedEvents := func() int {
		n := 0
		for len(recorder.Events) > 0 {
			if strings.Contains(<-recorder.Events, ReasonDependenciesLearned) {
				n++
			}
		}
		return n
	}

	require.Error(t, r.learnDependencies(context.Background(), api.DeepCopy(), target))
	assert.Zero(t, learnedEvents(), "not announced before it's recorded")

	fail = false
	require.NoError(t, r.learnDependencies(context.Background(), api, target))
	assert.Equal(t, 1, learnedEvents())
	assert.Equal(t, []string{"default/postgres DATABASE_URL"}, learnedNames(fetch(t, r, "api")), "and recorded")

	r.clock = func() time.Time { return fixedTime.Add(2 * time.Hour) }
	require.NoError(t, r.learnDependencies(context.Background(), api, target))
	assert.Zero(t, learnedEvents(), "learning the same again says nothing")
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
		WithStatusSubresource(&v1alpha1.ManagedWorkload{}).WithObjects(objs...).WithReturnManagedFields().
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
	w.ManagedFields = []metav1.ManagedFieldsEntry{stampedBy(v1alpha1.FieldManager)}
	return w
}

// stampedBy is the managed fields entry the API server records for a
// manager that set the wake annotations, as the doorman does.
func stampedBy(manager string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate,
		APIVersion: v1alpha1.GroupVersion.String(), Time: &metav1.Time{Time: fixedTime}, FieldsType: "FieldsV1",
		FieldsV1: metav1.NewFieldsV1(`{"f:metadata":{"f:annotations":{` +
			`"f:hybernate.io/last-request":{},"f:hybernate.io/last-request-from":{}}}}`)}
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
		{name: "an address someone other than the doorman wrote", postgres: func() *v1alpha1.ManagedWorkload {
			w := wokenPostgres()
			w.ManagedFields = []metav1.ManagedFieldsEntry{stampedBy("kubectl-annotate")}
			return w
		}(), objs: []client.Object{podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil)}},
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

// A request from another namespace teaches a dependency only when the
// woken workload's namespace allows dependents from it, and the sender is
// only looked for there.
func TestLearnFromWake_AcrossNamespaces(t *testing.T) {
	tests := []struct {
		name           string
		dependentsFrom string
		wantLearned    bool
	}{
		{name: "not allowed"},
		{name: "allowed", dependentsFrom: "preview-42", wantLearned: true},
		{name: "any allowed", dependentsFrom: "*", wantLearned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
			if tt.dependentsFrom != "" {
				ns.Annotations = map[string]string{v1alpha1.AnnotationDependentsFrom: tt.dependentsFrom}
			}
			sender := podOf("api", false)
			sender.Namespace = "preview-42"
			api := selecting(v1alpha1.TargetKindDeployment, "api", nil)
			api.SetNamespace("preview-42")
			postgres := wokenPostgres()
			var reads podReads
			r := reads.reconciler(t, ns, sender, api, postgres, selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil),
				depWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning))

			require.NoError(t, r.learnFromWake(context.Background(), postgres))

			var got v1alpha1.ManagedWorkload
			require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "preview-42", Name: "api"}, &got))
			if tt.wantLearned {
				assert.Equal(t, []string{"default/postgres "}, learnedNames(&got))
				return
			}
			assert.Empty(t, learnedNames(&got))
			assert.Equal(t, 1, reads.lists, "only its own namespace is searched")
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

// A dependency learned from a wake is forgotten once no request has
// confirmed it for two weeks, so a one-off request doesn't tie two
// workloads together for good.
func TestLearnDependencies_ForgetsWhatWakesNoLongerConfirm(t *testing.T) {
	tests := []struct {
		name     string
		lastSeen *metav1.Time
		want     []string
	}{
		{name: "confirmed recently", lastSeen: &metav1.Time{Time: fixedTime.Add(-13 * 24 * time.Hour)},
			want: []string{"default/redis "}},
		{name: "unconfirmed for over two weeks", lastSeen: &metav1.Time{Time: fixedTime.Add(-15 * 24 * time.Hour)}},
		{name: "learned before it was tracked", want: []string{"default/redis "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := apiWith(nil)
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			api.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
				{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "redis",
					Source: v1alpha1.LearnedFromWake, LastSeen: tt.lastSeen}}}
			r := depReconciler(t, &stubPauser{}, append(databases(), api, target)...)

			require.NoError(t, r.learnDependencies(context.Background(), api, target))

			got := fetch(t, r, "api")
			assert.Equal(t, tt.want, learnedNames(got))
			if tt.want != nil {
				lastSeen := got.Status.LearnedDependencies.Dependencies[0].LastSeen
				require.NotNil(t, lastSeen, "the time it was last confirmed is recorded")
			}
		})
	}
}

// Each request that would teach a dependency already learned from a wake
// confirms it, written at most once a day.
func TestLearnFromWake_ConfirmsWhatItLearned(t *testing.T) {
	tests := []struct {
		name     string
		lastSeen time.Time
		want     time.Time
	}{
		{name: "confirmed days ago", lastSeen: fixedTime.Add(-3 * 24 * time.Hour), want: fixedTime},
		{name: "confirmed within the day", lastSeen: fixedTime.Add(-time.Hour), want: fixedTime.Add(-time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			postgres := wokenPostgres()
			api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
			api.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: []v1alpha1.LearnedDependency{
				{Namespace: "default", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
					Source: v1alpha1.LearnedFromWake, LastSeen: &metav1.Time{Time: tt.lastSeen}}}}
			r := wakeReconciler(t, podOf("api", false), selecting(v1alpha1.TargetKindDeployment, "api", nil), api,
				postgres, selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))

			require.NoError(t, r.learnFromWake(context.Background(), postgres))

			got := fetch(t, r, "api").Status.LearnedDependencies.Dependencies
			require.Len(t, got, 1)
			assert.True(t, tt.want.Equal(got[0].LastSeen.Time), "last seen %s, want %s", got[0].LastSeen, tt.want)
		})
	}
}

// In a dual-stack cluster a request can come from a pod's second address,
// its IPv6 one where pods are IPv4 first, which isn't its status.podIP.
func TestLearnFromWake_DualStack(t *testing.T) {
	const senderIPv6 = "fd00:10:244:1::7"
	api := depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning)
	postgres := wokenPostgres()
	postgres.Annotations[v1alpha1.AnnotationLastRequestFrom] = senderIPv6
	sender := podOf("api", false)
	sender.Status.PodIPs = []corev1.PodIP{{IP: senderIP}, {IP: senderIPv6}}
	r := wakeReconciler(t, sender, selecting(v1alpha1.TargetKindDeployment, "api", nil), api, postgres,
		selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))

	require.NoError(t, r.learnFromWake(context.Background(), postgres))

	assert.Equal(t, []string{"default/postgres "}, learnedNames(fetch(t, r, "api")))
}

// podReads counts the pod lists learning from a wake makes, and the pages
// of a search by address among them, and notes the time it had left. With
// more set, every page of the search goes on to another.
type podReads struct {
	lists, pages int
	deadline     time.Duration
	more         bool
}

func (p *podReads) reconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	r := wakeReconciler(t, objs...)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); !ok {
				return c.List(ctx, list, opts...)
			}
			p.lists++
			if deadline, ok := ctx.Deadline(); ok {
				p.deadline = time.Until(deadline)
			}
			var o client.ListOptions
			o.ApplyOptions(opts)
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if o.Limit == podPageSize {
				p.pages++
				if p.more {
					list.(*corev1.PodList).Continue = "more"
				}
			}
			return nil
		},
	})
	return r
}

// Where pods' IPv6 addresses are their status.podIP, an IPv6-only cluster
// or one whose pods are IPv6 first, an address no pod has as its
// status.podIP is no pod's, such as a node's. Reading every pod in the
// cluster to find it, on every wake, held the wake up and found nothing.
func TestLearnFromWake_IPv6FromNoPod(t *testing.T) {
	const nodeIPv6 = "fd00:10:0:1::1"
	postgres := wokenPostgres()
	postgres.Annotations[v1alpha1.AnnotationLastRequestFrom] = nodeIPv6
	tests := []struct {
		name      string
		ips       []corev1.PodIP
		wantPages bool
	}{
		{name: "IPv6 only", ips: []corev1.PodIP{{IP: "fd00:10:244:1::7"}}},
		{name: "dual-stack, IPv6 first", ips: []corev1.PodIP{{IP: "fd00:10:244:1::7"}, {IP: senderIP}}},
		{name: "dual-stack, IPv4 first", ips: []corev1.PodIP{{IP: senderIP}, {IP: "fd00:10:244:1::7"}}, wantPages: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := podOf("api", false)
			pod.Status.PodIP, pod.Status.PodIPs = tt.ips[0].IP, tt.ips
			var reads podReads
			r := reads.reconciler(t, pod, selecting(v1alpha1.TargetKindDeployment, "api", nil),
				depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning), postgres,
				selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))

			require.NoError(t, r.learnFromWake(context.Background(), postgres))

			assert.Equal(t, tt.wantPages, reads.pages > 0, "pods read a page at a time")
			assert.Empty(t, learnedNames(fetch(t, r, "api")))
		})
	}
}

// Learning runs as the workload wakes, with the request that woke it
// waiting, so it's bounded: in time, and in how many pods a search by
// address reads.
func TestLearnFromWake_Bounded(t *testing.T) {
	postgres := wokenPostgres()
	postgres.Annotations[v1alpha1.AnnotationLastRequestFrom] = "fd00:10:244:9::9"
	pod := podOf("api", false)
	pod.Status.PodIPs = []corev1.PodIP{{IP: senderIP}, {IP: "fd00:10:244:1::7"}}
	reads := podReads{more: true}
	r := reads.reconciler(t, pod, selecting(v1alpha1.TargetKindDeployment, "api", nil),
		depWorkload("default", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning), postgres,
		selecting(v1alpha1.TargetKindStatefulSet, "postgres", nil))

	err := r.learnFromWake(context.Background(), postgres)

	require.ErrorContains(t, err, "not among the first 2000 pods")
	assert.Equal(t, maxPodPages, reads.pages)
	assert.LessOrEqual(t, reads.deadline, wakeLearnTimeout)
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
	postgres := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85}},
	}
	require.NoError(t, c.Create(ctx, postgres))
	stamp := func(manager string) {
		t.Helper()
		patch := client.MergeFrom(postgres.DeepCopy())
		postgres.Annotations = wokenPostgres().Annotations
		require.NoError(t, c.Patch(ctx, postgres, patch, client.FieldOwner(manager)))
	}
	r := &Reconciler{Client: c, PodReader: c, Scheme: scheme, Recorder: events.NewFakeRecorder(20),
		clock: func() time.Time { return fixedTime }}

	stamp("kubectl-annotate")
	require.NoError(t, r.learnFromWake(ctx, wokenPostgres()))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(api), api))
	assert.Empty(t, learnedNames(api), "an address someone else wrote isn't the doorman's")

	postgres.Annotations = nil
	require.NoError(t, c.Update(ctx, postgres))
	stamp(v1alpha1.FieldManager)
	require.NoError(t, r.learnFromWake(ctx, wokenPostgres()))

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(api), api))
	assert.Equal(t, []string{"default/postgres "}, learnedNames(api))
}
