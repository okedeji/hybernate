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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func depsWorkload(namespace, name string, kind v1alpha1.TargetKind, phase v1alpha1.WorkloadPhase,
	dependsOn []v1alpha1.DependencyRef, learned ...v1alpha1.LearnedDependency) *v1alpha1.ManagedWorkload {
	w := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(namespace + "/" + name)},
		Spec:       v1alpha1.ManagedWorkloadSpec{Target: v1alpha1.WorkloadRef{Kind: kind, Name: name}, DependsOn: dependsOn},
		Status:     v1alpha1.ManagedWorkloadStatus{Phase: phase},
	}
	if len(learned) > 0 {
		w.Status.LearnedDependencies = &v1alpha1.LearnedDependencies{Dependencies: learned}
	}
	return w
}

func depsCluster() []client.Object {
	postgres := v1alpha1.LearnedDependency{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet,
		Name: "postgres", Via: "PGHOST"}
	return []client.Object{
		depsWorkload("preview-42", "postgres", v1alpha1.TargetKindStatefulSet, v1alpha1.PhasePaused, nil),
		depsWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning,
			[]v1alpha1.DependencyRef{{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats"}},
			postgres, v1alpha1.LearnedDependency{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet,
				Name: "redis", Via: "REDIS_URL"}),
		depsWorkload("preview-42", "worker", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused,
			[]v1alpha1.DependencyRef{{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres", WaitForReady: true}},
			postgres),
		depsWorkload("messaging", "nats", v1alpha1.TargetKindStatefulSet, v1alpha1.PhaseRunning, nil),
	}
}

func TestDeps(t *testing.T) {
	tests := []struct {
		name string
		key  client.ObjectKey
		want string
	}{
		{name: "what depends on it", key: client.ObjectKey{Namespace: "preview-42", Name: "postgres"},
			want: `preview-42/postgres (StatefulSet, Paused)
Depends on:
  nothing
Depended on by:
  preview-42/api      learned from PGHOST                   Running
  preview-42/worker   declared in dependsOn, waitForReady   Paused
Learned links come from the dependent's environment or its requests; hybernate.io/ignore-dependencies drops one.
`},
		{name: "what it depends on", key: client.ObjectKey{Namespace: "preview-42", Name: "api"},
			want: `preview-42/api (Deployment, Running)
Depends on:
  messaging/nats        declared in dependsOn    Running
  preview-42/postgres   learned from PGHOST      Paused
  preview-42/redis      learned from REDIS_URL   not managed
Depended on by:
  nothing
Learned links come from the dependent's environment or its requests; hybernate.io/ignore-dependencies drops one.
`},
		{name: "a dependency in another namespace", key: client.ObjectKey{Namespace: "messaging", Name: "nats"},
			want: `messaging/nats (StatefulSet, Running)
Depends on:
  nothing
Depended on by:
  preview-42/api   declared in dependsOn   Running
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			require.NoError(t, deps(context.Background(), newClient(t, interceptor.Funcs{}, depsCluster()...), tt.key, &out))

			assert.Equal(t, tt.want, out.String())
		})
	}
}

// A dependency declared in dependsOn and also learned is shown once, as
// declared.
func TestDeps_DeclaredAndLearned(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, deps(context.Background(), newClient(t, interceptor.Funcs{}, depsCluster()...),
		client.ObjectKey{Namespace: "preview-42", Name: "worker"}, &out))

	assert.Contains(t, out.String(), "preview-42/postgres   declared in dependsOn, waitForReady   Paused")
	assert.NotContains(t, out.String(), "learned from PGHOST")
}

// Without access to every namespace, only the workload's own is read, and
// it says so.
func TestDeps_OneNamespace(t *testing.T) {
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if len(opts) == 0 {
			return apierrors.NewForbidden(schema.GroupResource{Group: "hybernate.io", Resource: "managedworkloads"}, "", nil)
		}
		return c.List(ctx, list, opts...)
	}}
	var out bytes.Buffer

	require.NoError(t, deps(context.Background(), newClient(t, funcs, depsCluster()...),
		client.ObjectKey{Namespace: "messaging", Name: "nats"}, &out))

	assert.Contains(t, out.String(), "Depended on by:\n  nothing\n")
	assert.Contains(t, out.String(), "Only messaging was read: your access doesn't allow listing ManagedWorkloads "+
		"in every namespace, so dependents elsewhere aren't shown.")
}

func TestDeps_LearnedFromAWake(t *testing.T) {
	api := depsWorkload("preview-42", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhaseRunning, nil,
		v1alpha1.LearnedDependency{Namespace: "preview-42", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres",
			Source: v1alpha1.LearnedFromWake})
	var out bytes.Buffer

	require.NoError(t, deps(context.Background(), newClient(t, interceptor.Funcs{}, api),
		client.ObjectKey{Namespace: "preview-42", Name: "api"}, &out))

	assert.Contains(t, out.String(), "preview-42/postgres   learned from a wake   not managed")
}
