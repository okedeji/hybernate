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

package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func TestAddresses(t *testing.T) {
	tests := []struct {
		value string
		want  []string
	}{
		{"postgres://app:secret@postgres.dev:5432/shop?sslmode=disable", []string{"postgres.dev"}},
		{"jdbc:postgresql://db:5432/orders", []string{"db"}},
		{"mongodb://mongo-0.mongo:27017,mongo-1.mongo:27017/app", []string{"mongo-0.mongo", "mongo-1.mongo"}},
		{"redis:6379", []string{"redis"}},
		{"kafka-0.kafka-headless.messaging.svc.cluster.local:9092", []string{"kafka-0.kafka-headless.messaging.svc.cluster.local"}},
		{"postgres.dev", []string{"postgres.dev"}},
		{"https://api.stripe.com/v1", []string{"api.stripe.com"}},
		{"api", nil},
		{"8080", nil},
		{"true", nil},
		{"Hello World", nil},
		{"key=value:abc", nil},
		{"note:important", nil},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			found := addresses(tt.value)
			var hosts []string
			if len(found) > 0 {
				hosts = make([]string, 0, len(found))
			}
			for _, a := range found {
				hosts = append(hosts, a.host)
			}
			assert.Equal(t, tt.want, hosts)
		})
	}
}

// Whatever comes before the last @ can be a credential, whatever its form,
// and so can a value whose key names a secret.
func TestShown(t *testing.T) {
	tests := []struct {
		raw, want string
	}{
		{"postgres://app:secret@postgres:5432/shop?password=x", "postgres://***@postgres:5432/shop"},
		{"postgres://app@postgres:5432", "postgres://***@postgres:5432"},
		{"admin:s3cr3t@redis:6379", "***@redis:6379"},
		{"http://TOKEN@svc:8080", "http://***@svc:8080"},
		{"https://user:p@ss:w0rd@api.internal/v1", "https://***@api.internal/v1"},
		{"redis://:pa?ss@redis:6379", "redis://***@redis:6379"},
		{"redis://:pa/ss@redis:6379/0", "redis://***@redis:6379/0"},
		{"user:secret@[fd00::1]:5432", "***@[fd00::1]:5432"},
		{"postgresql://u:p@[2001:db8::1]:5432/app", "postgresql://***@[2001:db8::1]:5432/app"},
		{"mysql://root:hunter2@tcp(mysql:3306)/app", "mysql://***@tcp(mysql:3306)/app"},
		{"amqp://guest:guest@rabbitmq:5672/vhost", "amqp://***@rabbitmq:5672/vhost"},
		{"mongodb://u:p@mongo-0.mongo:27017,mongo-1.mongo:27017/app?authSource=admin",
			"mongodb://***@mongo-0.mongo:27017,mongo-1.mongo:27017/app"},
		{"http://search:9200?api_key=abc123", "http://search:9200"},
		{"search:9200/x;password=abc", "search:9200/x;password=***"},
		{"host=db user=app password=hunter2 dbname=shop", "host=db user=app password=*** dbname=shop"},
		{"Server=db;User Id=sa;Password=hunter2;", "Server=db;User Id=sa;Password=***;"},
		{"jdbc:postgresql://db:5432/orders?user=app&password=hunter2", "jdbc:postgresql://db:5432/orders"},
		{"redis:6379", "redis:6379"},
		{"keycloak:8080", "keycloak:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := shown(tt.raw)
			assert.Equal(t, tt.want, got)
			for _, secret := range []string{"secret", "s3cr3t", "TOKEN", "p@ss", "w0rd", "pa", "hunter2", "abc"} {
				if !strings.Contains(tt.want, secret) {
					assert.NotContains(t, got, secret)
				}
			}
		})
	}
	assert.Len(t, shown("kafka:9092,"+strings.Repeat("broker:9092,", 20)), maxAddressShown)
}

// What the scan reports is what the addresses it finds show, so a
// credential in any of them stays hidden there too.
func TestScanCluster_NeverShowsCredentials(t *testing.T) {
	objs := []runtime.Object{
		database("redis", testNamespace),
		depService(testNamespace, "redis", false, map[string]string{"app": "redis"}),
		appWithEnv("cache-user", []corev1.EnvVar{
			{Name: "REDIS", Value: "admin:s3cr3t@redis:6379"},
			{Name: "REDIS_URL", Value: "http://TOKEN123@redis:8080"},
		}, nil),
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))

	require.NoError(t, err)
	deps := byName(report)["cache-user"].Dependencies
	require.Len(t, deps, 1)
	assert.Equal(t, "***@redis:6379", deps[0].Address)
	out, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "s3cr3t")
	assert.NotContains(t, string(out), "TOKEN123")
}

func depService(namespace, name string, headless bool, selector map[string]string) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.ServiceSpec{Selector: selector, ClusterIP: "10.0.0.1"}}
	if headless {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
	}
	return svc
}

func TestResolve(t *testing.T) {
	services := map[string]map[string]corev1.Service{
		"dev": {
			"postgres":    *depService("dev", "postgres", false, map[string]string{"app": "postgres"}),
			"postgres-hl": *depService("dev", "postgres-hl", true, map[string]string{"app": "postgres"}),
			"external":    *depService("dev", "external", false, nil),
		},
		"messaging": {"nats": *depService("messaging", "nats", false, map[string]string{"app": "nats"})},
	}
	sources := map[workloadKey]workloadSource{
		{"dev", v1alpha1.TargetKindStatefulSet, "postgres"}: {template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "postgres", "tier": "db"}}}},
		{"messaging", v1alpha1.TargetKindStatefulSet, "nats"}: {template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nats"}}}},
		// The same labels in another namespace aren't behind dev's Service.
		{"messaging", v1alpha1.TargetKindStatefulSet, "postgres"}: {template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "postgres"}}}},
	}
	postgres := workloadKey{"dev", v1alpha1.TargetKindStatefulSet, "postgres"}
	nats := workloadKey{"messaging", v1alpha1.TargetKindStatefulSet, "nats"}

	tests := []struct {
		host     string
		want     *workloadKey
		headless bool
	}{
		{host: "postgres", want: &postgres},
		{host: "postgres.dev", want: &postgres},
		{host: "postgres.dev.svc", want: &postgres},
		{host: "postgres.dev.svc.cluster.local", want: &postgres},
		{host: "postgres-hl", want: &postgres, headless: true},
		{host: "postgres-0.postgres-hl", want: &postgres, headless: true},
		{host: "postgres-0.postgres-hl.dev.svc.cluster.local", want: &postgres, headless: true},
		{host: "nats.messaging", want: &nats},
		{host: "postgres-0.postgres"},
		{host: "nats"},
		{host: "external"},
		{host: "api.stripe.com"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := resolve(address{host: tt.host}, "dev", services, sources)
			if tt.want == nil {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, *tt.want, got[0].key)
			assert.Equal(t, tt.headless, got[0].headless)
		})
	}
}

func appWithEnv(name string, env []corev1.EnvVar, envFrom []corev1.EnvFromSource) *appsv1.Deployment {
	d := makeDeployment(name, testNamespace, 1, "100m", "128Mi", nil)
	d.Spec.Template.Spec.Containers[0].Env = env
	d.Spec.Template.Spec.Containers[0].EnvFrom = envFrom
	return d
}

func database(name, namespace string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "db"}}},
			},
		},
	}
}

func TestScanCluster_FindsDependencies(t *testing.T) {
	const messaging = "messaging"
	objs := []runtime.Object{
		database("postgres", testNamespace),
		depService(testNamespace, "postgres", false, map[string]string{"app": "postgres"}),
		depService(testNamespace, "postgres-hl", true, map[string]string{"app": "postgres"}),
		database("nats", messaging),
		depService(messaging, "nats", false, map[string]string{"app": "nats"}),
		depService(testNamespace, "checkout-api", false, map[string]string{"app": "checkout-api"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "worker-config", Namespace: testNamespace},
			Data: map[string]string{"PGHOST": "postgres-0.postgres-hl", "LOG_LEVEL": "debug"}},
		appWithEnv("checkout-api", []corev1.EnvVar{
			{Name: "DATABASE_URL", Value: "postgres://shop:hunter2@postgres:5432/shop?sslmode=disable"},
			{Name: "NATS_URL", Value: "nats://nats.messaging:4222"},
			{Name: "PUBLIC_URL", Value: "http://checkout-api:8080"},
			{Name: "STRIPE_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "stripe"}, Key: "key"}}},
		}, nil),
		appWithEnv("worker", nil, []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}}},
		}),
		appWithEnv("plain", []corev1.EnvVar{{Name: "MODE", Value: "postgres"}}, nil),
		appWithEnv("reporter", []corev1.EnvVar{{Name: "PRIMARY", Value: "postgres:5432"}, {Name: "DB", ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}, Key: "PGHOST"}}}}, nil),
		&v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: testNamespace},
			Spec: v1alpha1.ManagedWorkloadSpec{
				Target:    v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "checkout-api"},
				DependsOn: []v1alpha1.DependencyRef{{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}},
			}},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace, messaging))

	require.NoError(t, err)
	got := byName(report)
	assert.Equal(t, []Dependency{
		{Namespace: testNamespace, Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "DATABASE_URL",
			Address: "postgres://***@postgres:5432/shop", Declared: true, Source: SourceEnvironment},
		{Namespace: messaging, Kind: v1alpha1.TargetKindStatefulSet, Name: "nats", Via: "NATS_URL",
			Address: "nats://nats.messaging:4222", Source: SourceEnvironment},
	}, got["checkout-api"].Dependencies,
		"a password is never shown; another namespace is followed; its own Service isn't a dependency")
	assert.Equal(t, []Dependency{
		{Namespace: testNamespace, Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "PGHOST",
			Address: "postgres-0.postgres-hl", Headless: true, Source: SourceEnvironment},
	}, got["worker"].Dependencies, "from a ConfigMap, through a headless Service's pod")
	assert.Empty(t, got["plain"].Dependencies, "a bare word isn't an address")
	require.Len(t, got["reporter"].Dependencies, 1, "two addresses for postgres are one dependency")
	assert.Equal(t, "DB", got["reporter"].Dependencies[0].Via,
		"the headless address, from a ConfigMap key, is the evidence kept")
	assert.True(t, got["reporter"].Dependencies[0].Headless)
	assert.Empty(t, got["postgres"].Dependencies, "its own Service isn't a dependency")
	assert.Contains(t, strings.Join(report.Notes, "\n"),
		"1 workload takes environment variables from Secrets, which the scan doesn't read")
}

// A dependency Hybernate learned for a workload it manages is connected:
// it's held and woken with the workload.
func TestScanCluster_LearnedDependenciesAreConnected(t *testing.T) {
	objs := []runtime.Object{
		database("postgres", testNamespace),
		depService(testNamespace, "postgres", false, map[string]string{"app": "postgres"}),
		database("redis", testNamespace),
		depService(testNamespace, "redis", false, map[string]string{"app": "redis"}),
		appWithEnv("api", []corev1.EnvVar{{Name: "PGHOST", Value: "postgres:5432"}, {Name: "REDIS", Value: "redis:6379"}}, nil),
		&v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: testNamespace},
			Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
			Status: v1alpha1.ManagedWorkloadStatus{LearnedDependencies: &v1alpha1.LearnedDependencies{
				Dependencies: []v1alpha1.LearnedDependency{
					{Namespace: testNamespace, Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", Via: "PGHOST"}}}}},
	}

	got := byName(scanWorkloads(t, objs...))["api"].Dependencies

	require.Len(t, got, 2)
	assert.True(t, got[0].Connected, "learned")
	assert.False(t, got[1].Connected, "ignored, so not connected")
}

// A dependency learned from a wake, which no address in the environment
// shows, is listed too, as connected.
func TestScanCluster_DependenciesLearnedFromWakes(t *testing.T) {
	objs := []runtime.Object{
		database("postgres", testNamespace),
		appWithEnv("api", nil, nil),
		&v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: testNamespace},
			Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}},
			Status: v1alpha1.ManagedWorkloadStatus{LearnedDependencies: &v1alpha1.LearnedDependencies{
				Dependencies: []v1alpha1.LearnedDependency{{Namespace: testNamespace, Kind: v1alpha1.TargetKindStatefulSet,
					Name: "postgres", Source: v1alpha1.LearnedFromWake}}}}},
	}

	got := byName(scanWorkloads(t, objs...))["api"].Dependencies

	assert.Equal(t, []Dependency{{Namespace: testNamespace, Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
		Source: SourceWake, Connected: true}}, got)
}

// The dependency pass reads only the ConfigMaps workloads take variables
// from, never every ConfigMap in a namespace.
func TestScanCluster_ReadsOnlyReferencedConfigMaps(t *testing.T) {
	objs := []runtime.Object{
		database("postgres", testNamespace),
		depService(testNamespace, "postgres", false, map[string]string{"app": "postgres"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "worker-config", Namespace: testNamespace},
			Data: map[string]string{"PGHOST": "postgres:5432"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dashboards", Namespace: testNamespace},
			Data: map[string]string{"big.json": strings.Repeat("x", 1<<20)}},
		appWithEnv("worker", nil, []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}}},
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "optional-missing"}}},
		}),
	}
	var read []string
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					t.Error("ConfigMaps listed")
				}
				return c.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					read = append(read, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"optional-missing", "worker-config"}, read)
	require.Len(t, byName(report)["worker"].Dependencies, 1)
	assert.Empty(t, report.Incomplete, "a referenced ConfigMap that doesn't exist isn't a failure")
}

// ConfigMaps and Services that can't be read are said so, not silently
// dropped.
func TestScanCluster_DependencyReadsThatFail(t *testing.T) {
	objs := []runtime.Object{
		appWithEnv("worker", nil, []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "worker-config"}}},
		}),
	}
	tests := []struct {
		name           string
		err            error
		wantNote       string
		wantIncomplete *Incomplete
	}{
		{name: "denied", err: forbidden("configmaps"),
			wantNote: "your access doesn't allow reading ConfigMaps in 1 namespace, so dependencies set in them " +
				"aren't found: " + testNamespace},
		{name: "failed", err: errors.New("connection reset"),
			wantNote:       "the scan is incomplete: it couldn't read ConfigMaps in " + testNamespace,
			wantIncomplete: &Incomplete{Failed: []string{testNamespace}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithRuntimeObjects(objs...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.ConfigMap); ok {
							return tt.err
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			report, err := NewScanner(c, c).ScanCluster(context.Background(), scanOptions(testNamespace))

			require.NoError(t, err)
			assert.Contains(t, strings.Join(report.Notes, "\n"), tt.wantNote)
			assert.Equal(t, tt.wantIncomplete, report.Incomplete)
		})
	}
}
