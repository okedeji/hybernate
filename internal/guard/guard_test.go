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

package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func managed(namespace, name string, finalizers ...string) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, Finalizers: finalizers,
	}}
}

func beingDeleted(namespace, name string) *v1alpha1.ManagedWorkload {
	mw := managed(namespace, name, v1alpha1.FinalizerCleanup)
	mw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	return mw
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		op      Operation
		watched []string
		objs    []client.Object
		refused bool
		names   []string
		absent  []string
	}{
		{
			name:    "uninstall refused while any ManagedWorkload is left",
			op:      Uninstall,
			objs:    []client.Object{managed("shop", "api", v1alpha1.FinalizerCleanup), managed("blog", "web")},
			refused: true,
			names:   []string{"shop/api", "blog/web", "refuses to uninstall"},
		},
		{
			name: "uninstall goes ahead with none left",
			op:   Uninstall,
		},
		{
			name:    "uninstall ignores unwatched ManagedWorkloads the operator never took on",
			op:      Uninstall,
			watched: []string{"shop"},
			objs:    []client.Object{managed("blog", "web")},
		},
		{
			name:    "uninstall refused for one in a watched namespace",
			op:      Uninstall,
			watched: []string{"shop", "blog"},
			objs:    []client.Object{managed("blog", "web"), managed("other", "x")},
			refused: true,
			names:   []string{"blog/web"},
			absent:  []string{"other/x"},
		},
		{
			name:    "uninstall after a refused upgrade still refused for one the operator took on",
			op:      Uninstall,
			watched: []string{"shop"},
			objs:    []client.Object{managed("blog", "web", v1alpha1.FinalizerCleanup)},
			refused: true,
			names:   []string{"blog/web"},
		},
		{
			name:    "uninstall refused for one still being deleted after the wait",
			op:      Uninstall,
			objs:    []client.Object{beingDeleted("shop", "api")},
			refused: true,
			names:   []string{"were being deleted", "shop/api"},
			absent:  []string{"refuses to uninstall"},
		},
		{
			name:    "upgrade refused when it stops watching a namespace the operator manages workloads in",
			op:      Upgrade,
			watched: []string{"shop"},
			objs: []client.Object{
				managed("shop", "api", v1alpha1.FinalizerCleanup),
				managed("blog", "web", v1alpha1.FinalizerCleanup),
			},
			refused: true,
			names:   []string{"blog/web", "refuses this upgrade", "watchNamespaces"},
			absent:  []string{"shop/api"},
		},
		{
			name:    "upgrade ignores ManagedWorkloads the operator never took on",
			op:      Upgrade,
			watched: []string{"shop"},
			objs:    []client.Object{managed("blog", "web")},
		},
		{
			name: "upgrade to every namespace strands nothing",
			op:   Upgrade,
			objs: []client.Object{managed("blog", "web", v1alpha1.FinalizerCleanup)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tt.objs...).Build()
			var out strings.Builder
			g := &Guard{Reader: c, Out: &out, Watched: tt.watched, Wait: 20 * time.Millisecond, Poll: 5 * time.Millisecond}

			err := g.Check(context.Background(), tt.op)

			if tt.refused {
				require.ErrorIs(t, err, ErrRefused)
			} else {
				require.NoError(t, err, out.String())
			}
			for _, s := range tt.names {
				assert.Contains(t, out.String(), s)
			}
			for _, s := range tt.absent {
				assert.NotContains(t, out.String(), s)
			}
		})
	}
}

func TestCheckWaitsForThoseBeingDeleted(t *testing.T) {
	var lists int
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(beingDeleted("shop", "api")).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch,
			list client.ObjectList, opts ...client.ListOption) error {
			lists++
			if lists == 2 {
				var mw v1alpha1.ManagedWorkload
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &mw))
				mw.Finalizers = nil
				require.NoError(t, c.Update(ctx, &mw))
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	var out strings.Builder
	g := &Guard{Reader: c, Out: &out, Wait: time.Minute, Poll: time.Millisecond}

	require.NoError(t, g.Check(context.Background(), Uninstall), out.String())
	assert.Contains(t, out.String(), "can go ahead")
}

func TestCheckWithoutTheCRD(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch,
			client.ObjectList, ...client.ListOption) error {
			return &meta.NoKindMatchError{GroupKind: v1alpha1.GroupVersion.WithKind("ManagedWorkload").GroupKind()}
		}}).Build()
	var out strings.Builder
	g := &Guard{Reader: c, Out: &out, Wait: time.Minute, Poll: time.Millisecond}

	require.NoError(t, g.Check(context.Background(), Uninstall))
	assert.Contains(t, out.String(), "CRD isn't installed")
}

func TestCheckFailsClosedWhenItCantList(t *testing.T) {
	forbidden := errors.New("forbidden")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch,
			client.ObjectList, ...client.ListOption) error {
			return forbidden
		}}).Build()
	g := &Guard{Reader: c, Out: &strings.Builder{}, Watched: []string{"shop"}, Wait: time.Minute, Poll: time.Millisecond}

	err := g.Check(context.Background(), Upgrade)

	require.ErrorIs(t, err, forbidden)
	assert.NotErrorIs(t, err, ErrRefused)
}
