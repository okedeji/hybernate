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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			ns:       enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
			workload: enableDeployment("api", nil, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "the cluster's default is overridden on the workload", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "an annotation that isn't true or false is overridden", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, map[string]string{v1alpha1.AnnotationDryRun: "yes"}),
			managed:  labelManaged("api", true), wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "a namespace annotation that isn't true or false is overridden on the workload",
			ns:       enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "maybe"}),
			workload: enableDeployment("api", nil, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "already pausing", ns: enableNamespaceObj(nil, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", false),
			wantOut: "isn't in dry-run"},
		{name: "opted in by its own label in an opted-out namespace",
			ns:       enableNamespaceObj(map[string]string{v1alpha1.LabelManaged: "false"}, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
		{name: "a protected namespace allowed anyway",
			ns: enableNamespaceObj(map[string]string{v1alpha1.LabelProtected: "true",
				v1alpha1.LabelAllowProtected: "true"}, nil),
			workload: enableDeployment("api", optedIn, nil), managed: labelManaged("api", true),
			wantValue: "false", wantSet: true, wantOut: "dry-run ended"},
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

// A ManagedWorkload the operator never pauses, by what it reports, is left
// alone, whether written by hand or made from the label: its protection
// may come from protectedNamespaces patterns enable can't see.
func TestEnable_WontPause(t *testing.T) {
	tests := []struct {
		name      string
		condition metav1.Condition
		fromLabel bool
		wantErr   string
	}{
		{name: "protected by a pattern", wantErr: "it's protected: namespace dev matches prod-*",
			condition: metav1.Condition{Type: "Protected", Status: metav1.ConditionTrue, Reason: "ProtectedNamespace",
				Message: "namespace dev matches prod-*"}},
		{name: "an ignored target", wantErr: "labelled hybernate.io/ignore",
			condition: metav1.Condition{Type: "TargetAvailable", Status: metav1.ConditionFalse, Reason: "TargetIgnored"}},
		{name: "a duplicate", wantErr: "another ManagedWorkload manages it",
			condition: metav1.Condition{Type: "DuplicateTarget", Status: metav1.ConditionTrue, Reason: "DuplicateTarget"}},
		{name: "made from the label", fromLabel: true, wantErr: "it's protected",
			condition: metav1.Condition{Type: "Protected", Status: metav1.ConditionTrue, Reason: "HybernateComponent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := &v1alpha1.ManagedWorkload{
				ObjectMeta: metav1.ObjectMeta{Name: "api-mw", Namespace: "dev"},
				Spec: v1alpha1.ManagedWorkloadSpec{
					Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}, DryRun: true},
				Status: v1alpha1.ManagedWorkloadStatus{Conditions: []metav1.Condition{tt.condition}},
			}
			labels := map[string]string(nil)
			if tt.fromLabel {
				mw.Labels = map[string]string{v1alpha1.LabelFromLabel: v1alpha1.True}
				labels = map[string]string{v1alpha1.LabelManaged: "true"}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", labels, measuring(nil)), mw).Build()

			err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &bytes.Buffer{})

			assert.ErrorContains(t, err, tt.wantErr)
			var got v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mw), &got))
			assert.True(t, got.Spec.DryRun, "nothing changed")
		})
	}
}

// Of a ManagedWorkload and its duplicate, enable goes by the one the
// operator manages the workload through, whichever is listed first.
func TestEnable_GoesByTheOneThatIsntADuplicate(t *testing.T) {
	for _, duplicateName := range []string{"aa-api", "zz-api"} {
		t.Run(duplicateName, func(t *testing.T) {
			owner := &v1alpha1.ManagedWorkload{
				ObjectMeta: metav1.ObjectMeta{Name: "api-mw", Namespace: "dev"},
				Spec: v1alpha1.ManagedWorkloadSpec{
					Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}, DryRun: true},
			}
			duplicate := owner.DeepCopy()
			duplicate.Name = duplicateName
			duplicate.Status.Conditions = []metav1.Condition{{Type: "DuplicateTarget", Status: metav1.ConditionTrue,
				Reason: "DuplicateTarget"}}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", nil, nil), owner, duplicate).Build()

			require.NoError(t, enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &bytes.Buffer{}))

			var got v1alpha1.ManagedWorkload
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(owner), &got))
			assert.False(t, got.Spec.DryRun)
		})
	}
}

// enable --all leaves out, and lists, what the operator never pauses.
func TestEnable_AllLeavesOutWhatWontPause(t *testing.T) {
	mw := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api-mw", Namespace: "dev"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}, DryRun: true},
		Status: v1alpha1.ManagedWorkloadStatus{Conditions: []metav1.Condition{{Type: "Protected",
			Status: metav1.ConditionTrue, Reason: "ProtectedNamespace", Message: "namespace dev matches dev*"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", nil, nil), mw).Build()
	var out bytes.Buffer

	require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{}, &out))

	var got v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mw), &got))
	assert.True(t, got.Spec.DryRun)
	assert.NotContains(t, out.String(), "Nothing in dev is in dry-run")
	assert.Contains(t, out.String(), "Not changed, because Hybernate never pauses them:\n"+
		"  deployment/api, as it's protected: namespace dev matches dev*\n")
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

// enable refuses, changing nothing, a workload Hybernate won't manage: one
// in a protected namespace, one its own label opts out, and one opted in
// that no ManagedWorkload manages, such as outside watchNamespaces.
func TestEnable_NotManaged(t *testing.T) {
	protected := map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelProtected: "true"}
	tests := []struct {
		name     string
		ns       *corev1.Namespace
		workload *appsv1.Deployment
		wantErr  string
	}{
		{name: "labelled protected", ns: enableNamespaceObj(protected, nil),
			workload: enableDeployment("api", nil, measuring(nil)),
			wantErr:  "namespace dev is protected, so Hybernate doesn't manage anything in it"},
		{name: "kube-system", ns: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
			workload: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "kube-system",
				Labels: optedIn, Annotations: measuring(nil)}},
			wantErr: "namespace kube-system is protected"},
		{name: "opted out in a managed namespace", ns: enableNamespaceObj(optedIn, nil),
			workload: enableDeployment("api", map[string]string{v1alpha1.LabelManaged: "false"}, measuring(nil)),
			wantErr:  `deployment/api is labelled hybernate.io/managed="false", which opts it out`},
		{name: "opted in, but not picked up", ns: enableNamespaceObj(optedIn, nil),
			workload: enableDeployment("api", nil, measuring(nil)),
			wantErr:  "deployment/api is opted in, but Hybernate hasn't picked it up: no ManagedWorkload in dev manages it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.ns, tt.workload).Build()
			var out bytes.Buffer

			err := enableWorkload(context.Background(), c, tt.ns.Name, "api", enableOptions{}, &out)

			assert.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, out.String(), "will pause")
			var d appsv1.Deployment
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tt.workload), &d))
			assert.Equal(t, "true", d.Annotations[v1alpha1.AnnotationDryRun], "nothing is changed")
		})
	}
}

// Without the CRD, nothing is installed to pause the workload, so enable
// says so rather than that Hybernate will pause it.
func TestEnable_NotInstalled(t *testing.T) {
	noCRD := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*v1alpha1.ManagedWorkloadList); ok {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "hybernate.io", Kind: "ManagedWorkload"}}
		}
		return c.List(ctx, list, opts...)
	}}
	c := newClient(t, noCRD, enableNamespaceObj(optedIn, nil), enableDeployment("api", nil, measuring(nil)))

	t.Run("one workload", func(t *testing.T) {
		err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &bytes.Buffer{})
		assert.ErrorIs(t, err, errNotInstalled)
	})
	t.Run("--all", func(t *testing.T) {
		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &bytes.Buffer{})
		assert.ErrorIs(t, err, errNotInstalled)
	})
	value, _ := dryRunOf(t, c, "api")
	assert.Equal(t, "true", value, "nothing is changed")
}

// A user who may edit workloads but not read ManagedWorkloads still ends
// dry-run by the annotations, and is told that's all enable went by.
func TestEnable_CannotReadManagedWorkloads(t *testing.T) {
	forbidden := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*v1alpha1.ManagedWorkloadList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: "hybernate.io", Resource: "managedworkloads"},
				"", nil)
		}
		return c.List(ctx, list, opts...)
	}}
	c := newClient(t, forbidden, enableNamespaceObj(optedIn, nil), enableDeployment("api", nil, measuring(nil)))
	var out bytes.Buffer

	require.NoError(t, enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &out))

	value, _ := dryRunOf(t, c, "api")
	assert.Equal(t, "false", value)
	assert.Contains(t, out.String(), `deployment/api: dry-run annotation set to "false". Your access doesn't allow `+
		"listing ManagedWorkloads in dev, so enable went by the annotations alone")
	assert.NotContains(t, out.String(), "will pause")
}

// --all refuses a protected namespace, and leaves alone, and fails on, the
// workloads no ManagedWorkload manages.
func TestEnable_NamespaceNotManaged(t *testing.T) {
	t.Run("protected", func(t *testing.T) {
		ns := enableNamespaceObj(map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelProtected: "true"},
			map[string]string{v1alpha1.AnnotationDryRun: "true"})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, enableDeployment("api", nil, nil)).Build()
		var out bytes.Buffer

		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

		assert.ErrorContains(t, err, "namespace dev is protected")
		value, _ := namespaceDryRun(t, c)
		assert.Equal(t, "true", value, "the namespace isn't changed")
	})

	t.Run("some not picked up", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(enableNamespaceObj(optedIn, nil),
			enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
			enableDeployment("api", nil, measuring(nil)),
		).Build()
		var out bytes.Buffer

		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

		assert.EqualError(t, err, "1 opted-in workload in dev has no ManagedWorkload")
		value, _ := dryRunOf(t, c, "web")
		assert.Equal(t, "false", value)
		value, _ = dryRunOf(t, c, "api")
		assert.Equal(t, "true", value, "a workload Hybernate hasn't picked up isn't changed")
		assert.Equal(t, "1 workload in dev: dry-run ended, Hybernate will pause it while idle\n"+
			"Not changed, because Hybernate hasn't picked them up: no ManagedWorkload manages them. The operator may "+
			"not be running, may not watch dev (its watchNamespaces), or may protect it:\n"+
			"  deployment/api\n", out.String())
	})
}

// A bare name a Deployment and a StatefulSet share could mean either, so
// enable asks for the kind instead of picking the Deployment; the kind
// settles it.
func TestEnable_NameBothKindsShare(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "dev", Labels: optedIn,
		Annotations: measuring(nil)}}
	mw := labelManaged("db-statefulset", true)
	mw.Spec.Target = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindStatefulSet, Name: "db"}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(enableNamespaceObj(nil, nil), enableDeployment("db", nil, nil), sts, mw).Build()

	err := enableWorkload(context.Background(), c, "dev", "db", enableOptions{}, &bytes.Buffer{})

	assert.EqualError(t, err, "db in dev names both deployment/db and statefulset/db; name one by its kind, "+
		"such as statefulset/db")

	var out bytes.Buffer
	require.NoError(t, enableWorkload(context.Background(), c, "dev", "statefulset/db", enableOptions{}, &out))
	assert.Equal(t, "statefulset/db: dry-run ended, Hybernate will pause it while idle\n", out.String())
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
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo), labelManaged("api", true)).Build()
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
			WithObjects(enableNamespaceObj(nil, nil), enableDeployment("api", optedIn, argo), labelManaged("api", true)).Build()

		err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{force: true}, &bytes.Buffer{})
		require.NoError(t, err)

		value, _ := dryRunOf(t, c, "api")
		assert.Equal(t, "false", value)
	})
}

// appliedAs has the API server record manager as owning fields of every
// Deployment read, as it does for whatever applied them. The fake client
// doesn't keep managed fields.
func appliedAs(manager, fields string) interceptor.Funcs {
	set := func(d *appsv1.Deployment) {
		d.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: manager, Operation: metav1.ManagedFieldsOperationApply,
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(fields)}}
	}
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if d, ok := obj.(*appsv1.Deployment); ok {
				set(d)
			}
			return nil
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if l, ok := list.(*appsv1.DeploymentList); ok {
				for i := range l.Items {
					set(&l.Items[i])
				}
			}
			return nil
		},
	}
}

const ownsDryRun = `{"f:metadata":{"f:annotations":{".":{},"f:hybernate.io/dry-run":{}}}}`

// Who applies the dry-run annotation decides, by the field managers the API
// server records: Argo CD or Flux would put it back, so enable leaves it for
// Git, whatever tracking metadata the workload carries; Helm would on its
// next upgrade, so enable changes it and warns; a tool that applies the
// workload but not the annotation leaves a change to it alone.
func TestEnable_WhoAppliesTheAnnotation(t *testing.T) {
	tests := []struct {
		name, manager, fields string
		labels                map[string]string
		wantGit               string
		wantWarning           bool
	}{
		{name: "Argo CD, tracking by its default label", manager: "argocd-controller", fields: ownsDryRun,
			labels: map[string]string{"app.kubernetes.io/instance": "shop"}, wantGit: "Argo CD application shop"},
		{name: "Argo CD, without tracking metadata", manager: "argocd-application-controller", fields: ownsDryRun,
			wantGit: "Argo CD"},
		{name: "Flux", manager: "kustomize-controller", fields: ownsDryRun, wantGit: "Flux"},
		{name: "Rancher Fleet", manager: "fleetagent", fields: ownsDryRun, wantGit: "Fleet"},
		{name: "Helm", manager: "helm", fields: ownsDryRun, wantWarning: true},
		{name: "Argo CD, applying other fields", manager: "argocd-controller",
			fields: `{"f:spec":{"f:replicas":{}}}`, labels: map[string]string{"app.kubernetes.io/instance": "shop"}},
		{name: "kubectl", manager: "kubectl-client-side-apply", fields: ownsDryRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := map[string]string{v1alpha1.LabelManaged: "true"}
			maps.Copy(labels, tt.labels)
			c := newClient(t, appliedAs(tt.manager, tt.fields), enableNamespaceObj(nil, nil),
				enableDeployment("api", labels, measuring(nil)), labelManaged("api", true))
			var out bytes.Buffer

			err := enableWorkload(context.Background(), c, "dev", "api", enableOptions{}, &out)

			value, _ := dryRunOf(t, c, "api")
			if tt.wantGit != "" {
				assert.ErrorIs(t, err, errManagedByGit)
				assert.Contains(t, out.String(), "deployment/api is managed by "+tt.wantGit+", which would undo")
				assert.Equal(t, "true", value, "the cluster isn't changed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "false", value)
			assert.Equal(t, tt.wantWarning, strings.Contains(out.String(),
				"Warning: Helm set deployment/api's hybernate.io/dry-run annotation, so the next helm upgrade puts "+
					"the chart's value back"), out.String())
		})
	}

	t.Run("--all", func(t *testing.T) {
		c := newClient(t, appliedAs("argocd-controller", ownsDryRun), enableNamespaceObj(optedIn, nil),
			enableDeployment("api", nil, measuring(nil)), labelManaged("api", true))
		var out bytes.Buffer

		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

		assert.ErrorIs(t, err, errManagedByGit)
		assert.Contains(t, out.String(), `deployment/api (Argo CD): set the annotation hybernate.io/dry-run: "false"`)
	})
}

func TestEnable_Namespace(t *testing.T) {
	flux := map[string]string{v1alpha1.LabelManaged: "true", "kustomize.toolkit.fluxcd.io/name": "previews"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
		enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
		enableDeployment("worker", nil, nil), labelManaged("worker", true),
		enableDeployment("api", flux, measuring(nil)), labelManaged("api", true),
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
	assert.Equal(t, "namespace dev: dry-run ended for the workloads that take its annotation, including ones "+
		"Hybernate picks up later\n"+`2 workloads in dev: dry-run ended, Hybernate will pause them while idle
Not changed, because their manifests come from Git; change them there:
  deployment/api (Flux Kustomization previews): remove the annotation hybernate.io/dry-run
`, out.String())
}

// The namespace's "false" ends dry-run for a workload Hybernate hasn't
// picked up yet, once it does, so --all doesn't call that one unchanged or
// fail on it; one whose own annotation keeps it in dry-run still fails.
func TestEnable_NamespaceCoversWorkloadsNotPickedUp(t *testing.T) {
	t.Run("without their own annotation", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
			enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
			enableDeployment("api", nil, nil),
		).Build()
		var out bytes.Buffer

		require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

		value, _ := namespaceDryRun(t, c)
		assert.Equal(t, "false", value)
		assert.Equal(t, "namespace dev: dry-run ended for the workloads that take its annotation, including ones "+
			"Hybernate picks up later\n1 workload in dev: dry-run ended, Hybernate will pause it while idle\n",
			out.String())
	})

	t.Run("with their own annotation", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
			enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
			enableDeployment("api", nil, measuring(nil)),
		).Build()
		var out bytes.Buffer

		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out)

		assert.EqualError(t, err, "1 opted-in workload in dev has no ManagedWorkload")
		assert.Contains(t, out.String(), "  deployment/api\n")
	})
}

// When the namespace's annotation is all that keeps anything in dry-run,
// enable says it changed the namespace, not that it ended dry-run for no
// workloads.
func TestEnable_NamespaceAnnotationOnly(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, map[string]string{v1alpha1.AnnotationDryRun: "true"}),
		enableDeployment("web", nil, map[string]string{v1alpha1.AnnotationDryRun: "false"}),
		labelManaged("web", false),
	).Build()
	var out bytes.Buffer

	require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

	value, _ := namespaceDryRun(t, c)
	assert.Equal(t, "false", value)
	assert.Equal(t, "namespace dev: dry-run ended for the workloads that take its annotation, including ones "+
		"Hybernate picks up later\n", out.String())
}

// When every workload in dry-run is there by its own annotation, the
// namespace isn't touched: each workload's annotation is set to "false".
func TestEnable_NamespaceOnlyOwnAnnotations(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		enableNamespaceObj(optedIn, nil),
		enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
		enableDeployment("api", nil, measuring(nil)), labelManaged("api", true),
	).Build()
	var out bytes.Buffer

	require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

	_, set := namespaceDryRun(t, c)
	assert.False(t, set)
	for _, name := range []string{"web", "api"} {
		value, _ := dryRunOf(t, c, name)
		assert.Equal(t, "false", value, name)
	}
	assert.Equal(t, "2 workloads in dev: dry-run ended, Hybernate will pause them while idle\n", out.String())
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
		enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
		enableDeployment("worker", nil, nil), labelManaged("worker", true),
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

// A user who may edit the namespace's workloads but not the namespace, as
// with edit or admin bound in it, still ends dry-run for all of them: each
// gets its own "false", which wins over the namespace's annotation, left
// alone. With nothing but the namespace to change, that's refused.
func TestEnable_NamespaceNotEditable(t *testing.T) {
	denied := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
		patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*corev1.Namespace); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, obj.GetName(), nil)
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
	measuringNamespace := map[string]string{v1alpha1.AnnotationDryRun: "true"}

	t.Run("workloads to change", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(denied).WithObjects(
			enableNamespaceObj(optedIn, measuringNamespace),
			enableDeployment("web", nil, measuring(nil)), labelManaged("web", true),
			enableDeployment("worker", nil, nil), labelManaged("worker", true),
		).Build()
		var out bytes.Buffer

		require.NoError(t, enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &out))

		value, _ := namespaceDryRun(t, c)
		assert.Equal(t, "true", value, "the namespace is left alone")
		for _, name := range []string{"web", "worker"} {
			value, _ := dryRunOf(t, c, name)
			assert.Equal(t, "false", value, name)
		}
		assert.Equal(t, "namespace dev: your access doesn't allow changing its hybernate.io/dry-run annotation, so "+
			"it was left alone; each workload gets its own \"false\" instead, which wins over it\n"+
			"2 workloads in dev: dry-run ended, Hybernate will pause them while idle\n", out.String())
	})

	t.Run("only the namespace to change", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(denied).WithObjects(
			enableNamespaceObj(optedIn, measuringNamespace),
			enableDeployment("web", nil, map[string]string{v1alpha1.AnnotationDryRun: "false"}),
			labelManaged("web", false),
		).Build()

		err := enableNamespace(context.Background(), c, "dev", enableOptions{all: true}, &bytes.Buffer{})

		assert.True(t, apierrors.IsForbidden(err), "%v", err)
	})
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
