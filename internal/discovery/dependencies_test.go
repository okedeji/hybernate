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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestShown(t *testing.T) {
	assert.Equal(t, "postgres://app:***@postgres:5432/shop", shown("postgres://app:secret@postgres:5432/shop?password=x"))
	assert.Equal(t, "postgres://app@postgres:5432", shown("postgres://app@postgres:5432"), "no password to hide")
	assert.Equal(t, "redis:6379", shown("redis:6379"))
	assert.Len(t, shown("kafka:9092,"+strings.Repeat("broker:9092,", 20)), maxAddressShown)
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
			Address: "postgres://shop:***@postgres:5432/shop", Declared: true, Source: SourceEnvironment},
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
