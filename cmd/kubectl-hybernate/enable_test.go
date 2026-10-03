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
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var optedIn = map[string]string{v1alpha1.LabelManaged: "true"}

func measuring(extra map[string]string) map[string]string {
	a := map[string]string{v1alpha1.AnnotationDryRun: "true"}
	maps.Copy(a, extra)
	return a
}

func enableNamespaceObj(labels, annotations map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Labels: labels, Annotations: annotations}}
}

func enableDeployment(name string, labels, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "sandbox", Labels: labels, Annotations: annotations,
	}}
}

func dryRunOf(t *testing.T, c client.Client, name string) (string, bool) {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "sandbox", Name: name}, &d))
	v, ok := d.Annotations[v1alpha1.AnnotationDryRun]
	return v, ok
}

func TestEnable_Workload(t *testing.T) {
	tests := []struct {
		name      string
		ns        *corev1.Namespace
		workload  *appsv1.Deployment
		wantValue string
		wantSet   bool
		wantOut   string
	}{
		{name: "its own annotation is removed", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, measuring(nil)), wantOut: "dry-run ended"},
		{name: "a measuring namespace is overridden on the workload",
			ns:        enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
			workload:  enableDeployment("api", nil, nil),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "already pausing", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, nil), wantOut: "isn't in dry-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.ns, tt.workload).Build()
			var out bytes.Buffer

			require.NoError(t, enableWorkload(context.Background(), c, "sandbox", "api", enableOptions{}, &out))

			value, set := dryRunOf(t, c, "api")
			assert.Equal(t, tt.wantSet, set)
			assert.Equal(t, tt.wantValue, value)
			assert.Contains(t, out.String(), tt.wantOut)
		})
	}
}

func TestEnable_NotOptedIn(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", nil, measuring(nil))).Build()

	err := enableWorkload(context.Background(), c, "sandbox", "api", enableOptions{}, &bytes.Buffer{})

	assert.ErrorContains(t, err, "label it hybernate.io/managed=true first")
}

// Git owns a GitOps-managed workload's annotations, so enable says what to
// change there instead of changing the cluster behind its back.
func TestEnable_GitOpsManaged(t *testing.T) {
	argo := measuring(map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:sandbox/api"})

	t.Run("prints the change for Git", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo)).Build()
		var out bytes.Buffer

		err := enableWorkload(context.Background(), c, "sandbox", "api", enableOptions{}, &out)

		assert.ErrorIs(t, err, errManagedByGit)
		assert.Contains(t, out.String(), "managed by Argo CD application shop")
		assert.Contains(t, out.String(), `remove the annotation  hybernate.io/dry-run: "true"`)
		value, _ := dryRunOf(t, c, "api")
		assert.Equal(t, "true", value, "the cluster isn't changed")
	})

	t.Run("--force changes it anyway", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo)).Build()

		err := enableWorkload(context.Background(), c, "sandbox", "api", enableOptions{force: true}, &bytes.Buffer{})
		require.NoError(t, err)

		_, set := dryRunOf(t, c, "api")
		assert.False(t, set)
	})
}

func TestEnable_Namespace(t *testing.T) {
	flux := map[string]string{v1alpha1.LabelManaged: "true", "kustomize.toolkit.fluxcd.io/name": "sandboxes"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
		enableDeployment("web", nil, measuring(nil)),
		enableDeployment("worker", nil, nil),
		enableDeployment("api", flux, measuring(nil)),
	).Build()
	var out bytes.Buffer

	err := enableNamespace(context.Background(), c, "sandbox", enableOptions{all: true}, &out)

	assert.ErrorIs(t, err, errManagedByGit, "one workload needs a change in Git")
	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "sandbox"}, &ns))
	assert.NotContains(t, ns.Annotations, v1alpha1.AnnotationDryRun)
	_, set := dryRunOf(t, c, "web")
	assert.False(t, set)
	value, _ := dryRunOf(t, c, "api")
	assert.Equal(t, "true", value)
	assert.Contains(t, out.String(), "1 workload in sandbox: dry-run ended")
	assert.Contains(t, out.String(), "deployment/api (Flux Kustomization sandboxes)")
}

func TestWorkloadArg(t *testing.T) {
	tests := []struct {
		arg, kind, name string
		err             bool
	}{
		{arg: "api", name: "api"},
		{arg: "deployment/api", kind: "deployment", name: "api"},
		{arg: "sts/postgres", kind: "statefulset", name: "postgres"},
		{arg: "cronjob/report", err: true},
	}
	for _, tt := range tests {
		kind, name, err := workloadArg(tt.arg)
		if tt.err {
			assert.Error(t, err, tt.arg)
			continue
		}
		require.NoError(t, err, tt.arg)
		assert.Equal(t, tt.kind, kind, tt.arg)
		assert.Equal(t, tt.name, name, tt.arg)
	}
}
