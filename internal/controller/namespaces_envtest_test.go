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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Under watchNamespaces the cache holds only the watched namespaces, so a
// change to any other namespace must not send the controllers to list in
// it: every such list fails, and each failure is logged as an error.
func TestNamespacedInstall_IgnoresUnwatchedNamespaces(t *testing.T) {
	cfg := startEnvtest(t)
	scheme := testScheme(t)
	watched := []string{"shop"}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{"shop": {}}},
	})
	require.NoError(t, err)

	cached, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme,
		Cache: &client.CacheOptions{Reader: mgr.GetCache()}})
	require.NoError(t, err)
	var mu sync.Mutex
	var listedIn []string
	recording := interceptor.NewClient(cached, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var o client.ListOptions
			o.ApplyOptions(opts)
			mu.Lock()
			listedIn = append(listedIn, o.Namespace)
			mu.Unlock()
			return c.List(ctx, list, opts...)
		},
	})

	optIn := &OptInReconciler{
		Client:          recording,
		Scheme:          scheme,
		Recorder:        mgr.GetEventRecorder("hybernate"),
		Kind:            v1alpha1.TargetKindDeployment,
		WatchNamespaces: watched,
	}
	require.NoError(t, optIn.SetupWithManager(mgr))
	workloads := &Reconciler{
		Client:          recording,
		Scheme:          scheme,
		Recorder:        mgr.GetEventRecorder("hybernate"),
		PodReader:       mgr.GetAPIReader(),
		WatchNamespaces: watched,
	}
	require.NoError(t, workloads.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { assert.NoError(t, mgr.Start(ctx)) })
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx))

	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	for _, name := range []string{"shop", "elsewhere"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		require.NoError(t, direct.Create(ctx, ns))
		ns.Labels = map[string]string{v1alpha1.LabelManaged: v1alpha1.True}
		require.NoError(t, direct.Update(ctx, ns))
	}

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(listedIn, "shop")
	}, 10*time.Second, 50*time.Millisecond, "a change to the watched namespace is acted on")
	assert.Never(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(listedIn, func(ns string) bool { return ns != "shop" })
	}, 3*time.Second, 100*time.Millisecond, "listed outside the watched namespace")
}
