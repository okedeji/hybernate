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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dev", Labels: labels, Annotations: annotations}}
}

func enableDeployment(name string, labels, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "dev", Labels: labels, Annotations: annotations,
	}}
}

func dryRunOf(t *testing.T, c client.Client, name string) (string, bool) {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "dev", Name: name}, &d))
	v, ok := d.Annotations[v1alpha1.AnnotationDryRun]
	return v, ok
}

func namespaceDryRun(t *testing.T, c client.Client) (string, bool) {
	t.Helper()
	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "dev"}, &ns))
	v, ok := ns.Annotations[v1alpha1.AnnotationDryRun]
	return v, ok
}

// labelManaged is the ManagedWorkload the operator makes for a labelled
// workload, with the dry-run it settled on.
func labelManaged(name string, dryRun bool) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dev",
			Labels: map[string]string{v1alpha1.LabelFromLabel: v1alpha1.True}},
		Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
			DryRun: dryRun},
	}
}

func TestEnable_Workload(t *testing.T) {
	tests := []struct {
		name      string
		ns        *corev1.Namespace
		workload  *appsv1.Deployment
		managed   *v1alpha1.ManagedWorkload
		wantValue string
		wantSet   bool
		wantOut   string
	}{
		{name: "its own annotation is overridden", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, measuring(nil)), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "a measuring namespace is overridden on the workload",
			ns:        enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
			workload:  enableDeployment("api", nil, nil),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "the cluster's default is overridden on the workload", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "an annotation that isn't true or false is overridden", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, map[string]string{v1alpha1.AnnotationDryRun: "yes"}),
			managed:  labelManaged("api", true), wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "a namespace annotation that isn't true or false is overridden on the workload",
			ns:        enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "maybe"}),
			workload:  enableDeployment("api", nil, nil),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "already pausing", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", false),
			wantOut: "isn't in dry-run"},
		{name: "already pausing, before the operator has seen it", ns: enableNamespaceObj(nil, nil),
			workload:  enableDeployment("api", optedIn, map[string]string{v1alpha1.AnnotationDryRun: "false"}),
			wantValue: "false", wantSet: true, wantOut: "isn't in dry-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.ns, tt.workload}
			if tt.managed != nil {
				objs = append(objs, tt.managed)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			var out bytes.Buffer

			require.NoError(t, enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &out))

			value, set := dryRunOf(t, c, "api")
			assert.Equal(t, tt.wantSet, set)
			assert.Equal(t, tt.wantValue, value)
			assert.Contains(t, out.String(), tt.wantOut)
		})
	}
}

// A ManagedWorkload written by hand says in its spec whether it's in
// dry-run; the workload's annotations don't count.
func TestEnable_HandWritten(t *testing.T) {
	mw := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api-mw", Namespace: "dev"},
		Spec: v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DryRun: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", nil, nil), mw).Build()
	var out bytes.Buffer

	require.NoError(t, enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &out))

	var got v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mw), &got))
	assert.False(t, got.Spec.DryRun)
	_, set := dryRunOf(t, c, "api")
	assert.False(t, set, "the workload isn't touched")
	assert.Equal(t, "deployment/api: dry-run ended, Hybernate will pause it while idle\n", out.String())
}

func TestEnable_NotOptedIn(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{name: "no label", wantErr: "label it hybernate.io/managed=true first"},
		{name: "ignored", labels: map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelIgnore: "true"},
			wantErr: "labelled hybernate.io/ignore=true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", tt.labels, measuring(nil))).Build()

			err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &bytes.Buffer{})

			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// A bare name that's neither kind says so, rather than that no StatefulSet
// has it, which reads as though a Deployment would have been found.
func TestEnable_NotFound(t *testing.T) {
	tests := []struct {
		arg, want string
	}{
		{arg: "nope", want: "no Deployment or StatefulSet in dev is named nope"},
		{arg: "deployment/nope", want: "getting deployment dev/nope"},
		{arg: "statefulset/nope", want: "getting statefulset dev/nope"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(enableNamespaceObj(nil, nil)).Build()

			err := enableWorkload(context.Background(), c, "dev", tt.arg, enableOptions{}, &bytes.Buffer{})

			require.Error(t, err)
			assert.True(t, apierrors.IsNotFound(err))
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// Git owns a GitOps-managed workload's annotations, so enable says what to
// change there instead of changing the cluster behind its back.
func TestEnable_GitOpsManaged(t *testing.T) {
	argo := measuring(map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:dev/api"})

	t.Run("prints the change for Git", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo)).Build()
		var out bytes.Buffer

		err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &out)

		assert.ErrorIs(t, err, errManagedByGit)
		assert.Contains(t, out.String(), "managed by Argo CD application shop")
		assert.Contains(t, out.String(), `set the annotation  hybernate.io/dry-run: "false"`)
		value, _ := dryRunOf(t, c, "api")
		assert.Equal(t, "true", value, "the cluster isn't changed")
	})

	t.Run("--force changes it anyway", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo)).Build()

		err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{force: true}, &bytes.Buffer{})
		require.NoError(t, err)

		value, _ := dryRunOf(t, c, "api")
		assert.Equal(t, "false", value)
	})
}

func TestEnable_Namespace(t *testing.T) {
	flux := map[string]string{v1alpha1.LabelManaged: "true", "kustomize.toolkit.fluxcd.io/name": "previews"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
		enableDeployment("web", nil, measuring(nil)),
		enableDeployment("worker", nil, nil),
		enableDeployment("api", flux, measuring(nil)),
	).Build()
	var out bytes.Buffer

	err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

	assert.ErrorIs(t, err, errManagedByGit, "one workload needs a change in Git")
	value, _ := namespaceDryRun(t, c)
	assert.Equal(t, "false", value)
	_, set := dryRunOf(t, c, "web")
	assert.False(t, set)
	_, set = dryRunOf(t, c, "worker")
	assert.False(t, set, "the namespace's annotation ends it for a workload without its own")
	value, _ = dryRunOf(t, c, "api")
	assert.Equal(t, "true", value)
	assert.Equal(t, `namespace dev: dry-run ended
2 workloads in dev: dry-run ended, Hybernate will pause them while idle
Not changed, because their manifests come from Git; change them there:
  deployment/api (Flux Kustomization previews): remove the annotation hybernate.io/dry-run
`, out.String())
}

// With the cluster's default dry-run on, the namespace has no annotation
// to remove; enable sets it to "false".
func TestEnable_NamespaceWithClusterDefault(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, nil),
		enableDeployment("web", nil, nil), labelManaged("web", true),
		enableDeployment("api", nil, nil), labelManaged("api", true),
	).Build()
	var out bytes.Buffer

	require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

	value, _ := namespaceDryRun(t, c)
	assert.Equal(t, "false", value)
	assert.Contains(t, out.String(), "2 workloads in dev: dry-run ended")
}

func TestEnable_NamespaceNothingInDryRun(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, nil), enableDeployment("web", nil, nil), labelManaged("web", false),
	).Build()
	var out bytes.Buffer

	require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

	_, set := namespaceDryRun(t, c)
	assert.False(t, set)
	assert.Equal(t, "Nothing in dev is in dry-run; Hybernate already pauses its workloads while idle\n", out.String())
}

// A namespace whose manifest comes from Git is left alone like a workload
// is: the GitOps tool would put the annotation back.
func TestEnable_NamespaceFromGit(t *testing.T) {
	ns := enableNamespaceObj(map[string]string{v1alpha1.LabelManaged: "true", "argocd.argoproj.io/instance": "platform"},
		map[string]string{v1alpha1.AnnotationDryRun: "true"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns,
		enableDeployment("web", nil, measuring(nil)),
		enableDeployment("worker", nil, nil),
	).Build()
	var out bytes.Buffer

	err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

	assert.ErrorIs(t, err, errManagedByGit)
	value, _ := namespaceDryRun(t, c)
	assert.Equal(t, "true", value, "the namespace isn't changed")
	value, _ = dryRunOf(t, c, "web")
	assert.Equal(t, "false", value, "a workload with its own annotation is overridden")
	assert.Equal(t, `1 workload in dev: dry-run ended, Hybernate will pause it while idle
Not changed, because their manifests come from Git; change them there:
  namespace dev (Argo CD application platform): set the annotation hybernate.io/dry-run: "false"
`, out.String())
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
