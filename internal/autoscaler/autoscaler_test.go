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

package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var scaledObject = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}

func hpa(name, target string, minReplicas *int32, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: target, APIVersion: "apps/v1"},
			MinReplicas:    minReplicas, MaxReplicas: maxReplicas,
		},
	}
}

func keda(name string, spec map[string]any) *unstructured.Unstructured {
	so := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	so.SetGroupVersionKind(scaledObject)
	so.SetNamespace("shop")
	so.SetName(name)
	return so
}

// withKEDA is a client of a cluster that has KEDA's CRDs, holding objs.
func withKEDA(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, autoscalingv2.AddToScheme(scheme))
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"), meta.RESTScopeNamespace)
	mapper.Add(scaledObject, meta.RESTScopeNamespace)
	return fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
}

func TestFind(t *testing.T) {
	tests := []struct {
		name  string
		objs  []client.Object
		kind  v1alpha1.TargetKind
		want  Autoscaler
		found bool
	}{
		{name: "an HPA", objs: []client.Object{hpa("web", "web", ptr.To[int32](2), 10)},
			want: Autoscaler{Kind: HPA, Name: "web", Min: 2, Max: 10}, found: true},
		{name: "an HPA's default minimum", objs: []client.Object{hpa("web", "web", nil, 4)},
			want: Autoscaler{Kind: HPA, Name: "web", Min: 1, Max: 4}, found: true},
		{name: "KEDA, not the HPA it creates", objs: []client.Object{hpa("keda-hpa-web", "web", ptr.To[int32](1), 20),
			keda("web", map[string]any{"scaleTargetRef": map[string]any{"name": "web"},
				"minReplicaCount": int64(1), "maxReplicaCount": int64(20)})},
			want: Autoscaler{Kind: KEDA, Name: "web", Min: 1, Max: 20}, found: true},
		{name: "KEDA's defaults", objs: []client.Object{keda("web", map[string]any{
			"scaleTargetRef": map[string]any{"name": "web"}})},
			want: Autoscaler{Kind: KEDA, Name: "web", Min: 0, Max: 100}, found: true},
		{name: "KEDA on a StatefulSet", kind: v1alpha1.TargetKindStatefulSet, objs: []client.Object{keda("db",
			map[string]any{"scaleTargetRef": map[string]any{"name": "web", "kind": "StatefulSet"}})},
			want: Autoscaler{Kind: KEDA, Name: "db", Min: 0, Max: 100}, found: true},
		{name: "another workload's", objs: []client.Object{hpa("api", "api", nil, 3),
			keda("api", map[string]any{"scaleTargetRef": map[string]any{"name": "api"}})}},
		{name: "an HPA of the same name, other kind", objs: []client.Object{func() client.Object {
			h := hpa("web", "web", nil, 3)
			h.Spec.ScaleTargetRef.Kind = "StatefulSet"
			return h
		}()}},
		{name: "same name, other kind", objs: []client.Object{keda("web", map[string]any{
			"scaleTargetRef": map[string]any{"name": "web", "kind": "StatefulSet"}})}},
		{name: "nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := tt.kind
			if kind == "" {
				kind = v1alpha1.TargetKindDeployment
			}

			got, found, err := NewFinder(withKEDA(t, tt.objs...)).Find(context.Background(), "shop", kind, "web")

			require.NoError(t, err)
			assert.Equal(t, tt.found, found)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Without KEDA installed, the Finder stops asking for ScaledObjects for a
// while, since every lookup of an unknown kind asks the API server for all
// its groups.
func TestFind_RemembersKEDAIsntInstalled(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, autoscalingv2.AddToScheme(scheme))
	lookups := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hpa("web", "web", nil, 3)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch,
			list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*unstructured.UnstructuredList); ok {
				lookups++
				return &meta.NoKindMatchError{GroupKind: scaledObject.GroupKind()}
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	f := NewFinder(c)
	f.now = func() time.Time { return now }

	for range 3 {
		got, found, err := f.Find(context.Background(), "shop", v1alpha1.TargetKindDeployment, "web")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, HPA, got.Kind, "HPAs are still found")
	}
	assert.Equal(t, 1, lookups)

	now = now.Add(kedaRecheck)
	_, _, err := f.Find(context.Background(), "shop", v1alpha1.TargetKindDeployment, "web")
	require.NoError(t, err)
	assert.Equal(t, 2, lookups, "looked for again, in case KEDA was installed since")
}

func TestHoldKEDA(t *testing.T) {
	c := withKEDA(t, keda("web", map[string]any{"scaleTargetRef": map[string]any{"name": "web"}}))
	annotation := func() (string, bool) {
		so := &unstructured.Unstructured{}
		so.SetGroupVersionKind(scaledObject)
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "web"}, so))
		v, ok := so.GetAnnotations()[PausedReplicasAnnotation]
		return v, ok
	}

	require.NoError(t, HoldKEDA(context.Background(), c, "shop", "web", true))
	v, ok := annotation()
	assert.True(t, ok)
	assert.Equal(t, "0", v)

	require.NoError(t, HoldKEDA(context.Background(), c, "shop", "web", false))
	_, ok = annotation()
	assert.False(t, ok, "released")

	assert.NoError(t, HoldKEDA(context.Background(), c, "shop", "gone", false), "nothing to release")
}

func TestClamp(t *testing.T) {
	a := Autoscaler{Min: 2, Max: 10}

	assert.Equal(t, int32(2), a.Clamp(1))
	assert.Equal(t, int32(5), a.Clamp(5))
	assert.Equal(t, int32(10), a.Clamp(30))
}
