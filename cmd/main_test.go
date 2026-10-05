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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/controller"
	"github.com/okedeji/hybernate/internal/doorman"
	"github.com/okedeji/hybernate/internal/gitops"
)

func TestValidatePrometheusURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "empty is allowed", url: ""},
		{name: "in-cluster service", url: "http://prometheus.monitoring.svc:9090"},
		{name: "https with path prefix", url: "https://mimir.example.com/prometheus"},
		{name: "missing scheme", url: "prometheus.monitoring.svc:9090", wantErr: true},
		{name: "unsupported scheme", url: "ftp://prometheus:9090", wantErr: true},
		{name: "no host", url: "http://", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePrometheusURL(tt.url)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateOptInDefaults(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*controller.OptInDefaults)
		wantErr bool
	}{
		{name: "built-in defaults", mutate: func(*controller.OptInDefaults) {}},
		{name: "threshold of 100", mutate: func(d *controller.OptInDefaults) { d.CPUThreshold = 100 }},
		{name: "threshold of 1", mutate: func(d *controller.OptInDefaults) { d.CPUThreshold = 1 }},
		{name: "threshold above the CRD's maximum", mutate: func(d *controller.OptInDefaults) { d.CPUThreshold = 150 },
			wantErr: true},
		{name: "threshold of 0, which the CRD turns into 10",
			mutate: func(d *controller.OptInDefaults) { d.CPUThreshold = 0 }, wantErr: true},
		{name: "negative threshold", mutate: func(d *controller.OptInDefaults) { d.CPUThreshold = -5 }, wantErr: true},
		{name: "zero idleAfter", mutate: func(d *controller.OptInDefaults) { d.IdleAfter = 0 }, wantErr: true},
		{name: "negative idleAfter", mutate: func(d *controller.OptInDefaults) { d.IdleAfter = -time.Minute },
			wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := controller.DefaultOptInDefaults
			tt.mutate(&d)
			err := validateOptInDefaults(d)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func managedFields(t *testing.T) []metav1.ManagedFieldsEntry {
	t.Helper()
	at := func(minute int) *metav1.Time {
		return &metav1.Time{Time: time.Date(2026, 10, 5, 9, minute, 0, 0, time.UTC)}
	}
	return []metav1.ManagedFieldsEntry{
		{Manager: "argocd-controller", Operation: metav1.ManagedFieldsOperationApply, Time: at(1),
			FieldsV1: &metav1.FieldsV1{
				Raw: []byte(`{"f:metadata":{"f:labels":{}},"f:spec":{"f:replicas":{},"f:template":{}}}`),
			}},
		{Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: at(5),
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:replicas":{},"f:readyReplicas":{}}}`)}},
		{Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, Time: at(3),
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:template":{"f:spec":{}}}}`)}},
	}
}

func TestTrimForCacheKeepsWhoSetReplicas(t *testing.T) {
	fields := managedFields(t)
	wantWriter, wantFound := gitops.ReplicasWriter(fields)
	require.True(t, wantFound)

	for _, obj := range []client.Object{&appsv1.Deployment{}, &appsv1.StatefulSet{}} {
		obj.SetManagedFields(managedFields(t))
		obj.SetAnnotations(map[string]string{corev1.LastAppliedConfigAnnotation: "{}", "team": "payments"})

		trimmed, err := trimForCache(obj)
		require.NoError(t, err)
		got := trimmed.(client.Object)

		writer, found := gitops.ReplicasWriter(got.GetManagedFields())
		assert.True(t, found)
		assert.Equal(t, wantWriter, writer)
		assert.Len(t, got.GetManagedFields(), 1, "only the entry that set spec.replicas is kept")
		assert.Equal(t, map[string]string{"team": "payments"}, got.GetAnnotations())
	}
}

func TestTrimForCacheDropsManagedFieldsAndLastApplied(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		ManagedFields: managedFields(t),
		Annotations:   map[string]string{corev1.LastAppliedConfigAnnotation: "{}"},
	}}

	trimmed, err := trimForCache(svc)
	require.NoError(t, err)

	got := trimmed.(*corev1.Service)
	assert.Empty(t, got.ManagedFields)
	assert.Empty(t, got.Annotations)
}

func TestTrimForCacheKeepsManagedWorkloadsLastApplied(t *testing.T) {
	mw := &v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{
		ManagedFields: managedFields(t),
		Annotations:   map[string]string{corev1.LastAppliedConfigAnnotation: "{}"},
	}}

	trimmed, err := trimForCache(mw)
	require.NoError(t, err)

	got := trimmed.(*v1alpha1.ManagedWorkload)
	assert.Empty(t, got.ManagedFields)
	assert.Contains(t, got.Annotations, corev1.LastAppliedConfigAnnotation,
		"the operator updates ManagedWorkloads whole, which would delete kubectl's record")
}

func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	env := &envtest.Environment{}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		dirs, _ := filepath.Glob(filepath.Join("..", "bin", "k8s", "*"))
		if len(dirs) > 0 {
			env.BinaryAssetsDirectory = dirs[0]
		}
	}
	cfg, err := env.Start()
	require.NoError(t, err, "starting envtest; run make setup-envtest")
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	return cfg
}

func endpointSlice(namespace, name string, labels map[string]string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
}

func sliceNames(t *testing.T, ctx context.Context, c client.Reader, opts ...client.ListOption) []string {
	t.Helper()
	var list discoveryv1.EndpointSliceList
	require.NoError(t, c.List(ctx, &list, opts...))
	names := make([]string, 0, len(list.Items))
	for _, s := range list.Items {
		names = append(names, s.Namespace+"/"+s.Name)
	}
	return names
}

func TestOperatorCacheSeesOnlyDoormanSlices(t *testing.T) {
	cfg := startEnvtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	for _, ns := range []string{"hybernate-system", "shop", "other"} {
		require.NoError(t, direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	routed := map[string]string{discoveryv1.LabelManagedBy: doorman.ManagedBy, discoveryv1.LabelServiceName: "web"}
	for _, s := range []*discoveryv1.EndpointSlice{
		endpointSlice("hybernate-system", "hybernate-doorman-abc",
			map[string]string{discoveryv1.LabelServiceName: "hybernate-doorman"}),
		endpointSlice("shop", "web-hybernate-doorman", routed),
		endpointSlice("shop", "web-xyz", map[string]string{discoveryv1.LabelServiceName: "web"}),
		endpointSlice("other", "api-hybernate-doorman", routed),
		endpointSlice("other", "api-xyz", map[string]string{discoveryv1.LabelServiceName: "api"}),
	} {
		require.NoError(t, direct.Create(ctx, s))
	}

	tests := []struct {
		name    string
		watched []string
		want    map[string][]string
	}{
		{
			name:    "watching some namespaces",
			watched: []string{"shop"},
			want: map[string][]string{
				"hybernate-system": {"hybernate-system/hybernate-doorman-abc"},
				"shop":             {"shop/web-hybernate-doorman"},
			},
		},
		{
			name: "watching every namespace",
			want: map[string][]string{
				"hybernate-system": {"hybernate-system/hybernate-doorman-abc"},
				"shop":             {"shop/web-hybernate-doorman"},
				"other":            {"other/api-hybernate-doorman"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := operatorCacheOptions(tt.watched, "hybernate-system")
			opts.Scheme = scheme
			c, err := cache.New(cfg, opts)
			require.NoError(t, err)
			cacheCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- c.Start(cacheCtx) }()
			defer func() {
				stop()
				assert.NoError(t, <-done)
			}()

			for ns, want := range tt.want {
				require.Eventually(t, func() bool {
					var list discoveryv1.EndpointSliceList
					return c.List(ctx, &list, client.InNamespace(ns)) == nil && len(list.Items) == len(want)
				}, 30*time.Second, 100*time.Millisecond, "namespace %s", ns)
				assert.ElementsMatch(t, want, sliceNames(t, ctx, c, client.InNamespace(ns)))
			}
			assert.ElementsMatch(t, []string{"hybernate-system/hybernate-doorman-abc"},
				sliceNames(t, ctx, c, client.InNamespace("hybernate-system"),
					client.MatchingLabels{discoveryv1.LabelServiceName: "hybernate-doorman"}),
				"the operator finds the doorman's pods through these")
		})
	}
}
