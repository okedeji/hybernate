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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// runCLI runs the plugin as kubectl would, against c, with a kubeconfig
// whose context's namespace is preview-42.
func runCLI(t *testing.T, c client.Client, args ...string) (string, error) {
	t.Helper()
	writeKubeconfig(t, "https://127.0.0.1:1", "preview-42")
	root := newRootCmd(func(*rest.Config) (client.Client, error) { return c, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestCLI_StatusYAML(t *testing.T) {
	out, err := runCLI(t, newStatusClient(t, interceptor.Funcs{}, statusCluster()...), "status", "-o", "yaml")
	require.NoError(t, err)

	var got statusResult
	require.NoError(t, yaml.Unmarshal([]byte(out), &got))
	assert.Equal(t, "hanging", got.Context)
	assert.Len(t, got.Workloads, 4)
	require.NotEmpty(t, got.Problems)
	assert.Equal(t, "GitOpsConflict", got.Problems[0].Type)
	assert.InDelta(t, 42.45, got.SavedThisMonth, 0.001)
}

func TestCLI_StatusListsTenRecent(t *testing.T) {
	objs := make([]client.Object, 0, 13)
	objs = append(objs, statusWorkloadObj("shop", "api", v1alpha1.TargetKindDeployment, v1alpha1.PhasePaused,
		time.Hour))
	for i := range 12 {
		ev := statusEvent("shop", "api", "Paused", "api: paused", time.Duration(i)*time.Minute)
		ev.EventTime = metav1.NewMicroTime(time.Now().Add(-time.Duration(i+1) * time.Minute))
		objs = append(objs, ev)
	}

	out, err := runCLI(t, newStatusClient(t, interceptor.Funcs{}, objs...), "status")
	require.NoError(t, err)

	assert.Contains(t, out, "  10m ago   shop/api   paused\n  and 2 more; -o json lists them all\n")
	assert.NotContains(t, out, "11m ago")
}

func notInstalled() interceptor.Funcs {
	noCRD := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "hybernate.io", Kind: "ManagedWorkload"}}
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.ManagedWorkload); ok {
				return noCRD
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.ManagedWorkloadList); ok {
				return noCRD
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// Without the CRD, the commands that need the operator say it isn't
// installed, and an error from the cluster doesn't print usage.
func TestCLI_NotInstalled(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"wake", "api"}, {"deps", "api"}} {
		t.Run(args[0], func(t *testing.T) {
			out, err := runCLI(t, newStatusClient(t, notInstalled()), args...)

			require.ErrorIs(t, err, errNotInstalled)
			assert.NotContains(t, out, "Usage:")
		})
	}
}

func TestCLI_StatusAllNamespacesForbidden(t *testing.T) {
	c := newStatusClient(t, forbidClusterWide("managedworkloads"), statusCluster()...)

	out, err := runCLI(t, c, "status", "-A")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing ManagedWorkloads in every namespace")
	assert.Contains(t, err.Error(), "forbidden")
	assert.NotContains(t, out, "Usage:")

	out, err = runCLI(t, c, "status")

	require.NoError(t, err)
	assert.Contains(t, out, "Only preview-42, the context's namespace, is shown")
}

// Without access to list namespaces, scan reads the context's, and says
// so, unless -A asked for all of them.
func TestCLI_ScanCantListNamespaces(t *testing.T) {
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
		opts ...client.ListOption) error {
		if _, ok := list.(*corev1.NamespaceList); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", nil)
		}
		return c.List(ctx, list, opts...)
	}}
	c := newStatusClient(t, funcs)

	out, err := runCLI(t, c, "scan", "--window", "0", "-o", "json")

	require.NoError(t, err)
	assert.Contains(t, out, "only preview-42, the context's namespace, was scanned; name others with -n")

	_, err = runCLI(t, c, "scan", "--window", "0", "-o", "json", "-A")

	assert.ErrorContains(t, err, "your access there doesn't allow that")
}

// Usage helps with a mistyped command line, so it's printed for one.
func TestCLI_UsageForUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"wake"},
		{"deps", "a", "b"},
		{"status", "--bogus"},
		{"status", "-n", "a", "-A"},
		{"enable"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			out, err := runCLI(t, newStatusClient(t, interceptor.Funcs{}), args...)

			require.Error(t, err)
			assert.Contains(t, out, "Usage:")
		})
	}
}

func TestCLI_Version(t *testing.T) {
	out, err := runCLI(t, nil, "version")

	require.NoError(t, err)
	assert.Equal(t, "kubectl-hybernate dev\n", out)
}

// wake, enable and deps default to the kubeconfig context's namespace.
func TestCLI_ContextNamespace(t *testing.T) {
	t.Run("wake", func(t *testing.T) {
		c := newClient(t, interceptor.Funcs{}, managedWorkload(v1alpha1.PhaseRunning))

		out, err := runCLI(t, c, "wake", "deployment/api")

		require.NoError(t, err)
		assert.Equal(t, "preview-42/api is already running; its idle clock restarts now\n", out)
		assert.NotEmpty(t, annotations(t, c)[v1alpha1.AnnotationLastActivity])
	})

	t.Run("deps", func(t *testing.T) {
		out, err := runCLI(t, newClient(t, interceptor.Funcs{}, depsCluster()...), "deps", "worker")

		require.NoError(t, err)
		assert.Contains(t, out, "preview-42/worker (Deployment, Paused)\n")
	})

	t.Run("enable", func(t *testing.T) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-42"}}
		api := enableDeployment("api", optedIn, measuring(nil))
		api.Namespace = "preview-42"
		c := newClient(t, interceptor.Funcs{}, ns, api)

		out, err := runCLI(t, c, "enable", "api")

		require.NoError(t, err)
		assert.Equal(t, "deployment/api: dry-run ended, Hybernate will pause it while idle\n", out)
	})
}

// The kubectl connection flags work on every command, and every client is
// allowed more than client-go's default 5 requests a second.
// --request-timeout's help says what the plugin does: kubectl's says it
// defaults to 0, no timeout, which the plugin never uses.
func TestCLI_RequestTimeoutHelp(t *testing.T) {
	flag := newRootCmd(newKubeClient).PersistentFlags().Lookup("request-timeout")
	require.NotNil(t, flag)

	assert.Equal(t, "30s", flag.DefValue)
	assert.Contains(t, flag.Usage, "0 means the default")
	assert.NotContains(t, flag.Usage, "don't timeout")
}

func TestCLI_ConnectionFlags(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- {name: a, cluster: {server: "https://a.example:6443"}}
- {name: b, cluster: {server: "https://b.example:6443"}}
users: [{name: me, user: {token: t}}]
contexts:
- {name: a, context: {cluster: a, user: me, namespace: from-a}}
- {name: b, context: {cluster: b, user: me, namespace: from-b}}
current-context: a
`), 0o600))
	t.Setenv("KUBECONFIG", "")

	tests := []struct {
		name  string
		args  []string
		check func(*testing.T, *rest.Config)
	}{
		{name: "the kubeconfig's current context", args: nil, check: func(t *testing.T, config *rest.Config) {
			assert.Equal(t, "https://a.example:6443", config.Host)
			assert.Equal(t, apiRequestTimeout, config.Timeout)
			assert.InDelta(t, float32(clientQPS), config.QPS, 0)
			assert.Equal(t, clientBurst, config.Burst)
		}},
		{name: "--context", args: []string{"--context", "b"}, check: func(t *testing.T, config *rest.Config) {
			assert.Equal(t, "https://b.example:6443", config.Host)
		}},
		{name: "--cluster", args: []string{"--cluster", "b"}, check: func(t *testing.T, config *rest.Config) {
			assert.Equal(t, "https://b.example:6443", config.Host)
		}},
		{name: "--as", args: []string{"--as", "jane", "--as-group", "devs"}, check: func(t *testing.T, config *rest.Config) {
			assert.Equal(t, "jane", config.Impersonate.UserName)
			assert.Equal(t, []string{"devs"}, config.Impersonate.Groups)
		}},
		{name: "--request-timeout", args: []string{"--request-timeout", "5s"},
			check: func(t *testing.T, config *rest.Config) {
				assert.Equal(t, 5*time.Second, config.Timeout)
			}},
		{name: "--request-timeout 0", args: []string{"--request-timeout", "0"},
			check: func(t *testing.T, config *rest.Config) {
				assert.Equal(t, apiRequestTimeout, config.Timeout)
			}},
	}
	for _, command := range []string{"status", "wake", "enable", "deps", "scan"} {
		for _, tt := range tests {
			t.Run(command+" "+tt.name, func(t *testing.T) {
				var got *rest.Config
				root := newRootCmd(func(config *rest.Config) (client.Client, error) {
					got = config
					return nil, fmt.Errorf("stop here")
				})
				args := []string{command, "--kubeconfig", kubeconfig}
				if command == "wake" || command == "enable" || command == "deps" {
					args = append(args, "api")
				}
				root.SetOut(&bytes.Buffer{})
				root.SetErr(&bytes.Buffer{})
				root.SetArgs(append(args, tt.args...))

				require.ErrorContains(t, root.Execute(), "stop here")
				require.NotNil(t, got)
				tt.check(t, got)
			})
		}
	}
}
