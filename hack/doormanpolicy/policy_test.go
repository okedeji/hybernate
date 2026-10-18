// Package doormanpolicy tests the doorman's ValidatingAdmissionPolicy, as
// the chart and the kustomize manifests install it, against a real API
// server. make test-doorman-policy renders both and runs it; with neither
// CHART_MANIFEST nor KUSTOMIZE_MANIFEST set it's skipped.
package doormanpolicy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/okedeji/hybernate/api/v1alpha1"
)

const workloadNS = "shop"

var appliedKinds = map[string]bool{
	"ClusterRole":                      true,
	"ClusterRoleBinding":               true,
	"ValidatingAdmissionPolicy":        true,
	"ValidatingAdmissionPolicyBinding": true,
}

func TestDoormanMayOnlyStampRequests(t *testing.T) {
	manifests := []struct{ name, env string }{
		{name: "chart", env: "CHART_MANIFEST"},
		{name: "kustomize", env: "KUSTOMIZE_MANIFEST"},
	}
	ran := false
	for _, m := range manifests {
		path := os.Getenv(m.env)
		if path == "" {
			continue
		}
		ran = true
		t.Run(m.name, func(t *testing.T) { testPolicy(t, path) })
	}
	if !ran {
		t.Skip("neither CHART_MANIFEST nor KUSTOMIZE_MANIFEST is set; run make test-doorman-policy")
	}
}

func testPolicy(t *testing.T, manifest string) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() }) // best-effort; the process exits next

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	scheme := clientScheme(t)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	doormanUser := apply(ctx, t, admin, manifest)

	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workloadNS}}))
	workload := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web",
			Namespace:   workloadNS,
			Labels:      map[string]string{"team": "checkout"},
			Annotations: map[string]string{"keep": "me"},
		},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: "Deployment", Name: "web"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 75},
		},
	}
	require.NoError(t, admin.Create(ctx, workload))

	doormanCfg := rest.CopyConfig(cfg)
	doormanCfg.Impersonate = rest.ImpersonationConfig{UserName: doormanUser}
	doorman, err := client.New(doormanCfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	patchAs := func(c client.Client, p string) error {
		web := &v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: workload.Name, Namespace: workloadNS}}
		return c.Patch(ctx, web, client.RawPatch(types.MergePatchType, []byte(p)))
	}
	patch := func(p string) error { return patchAs(doorman, p) }

	// The policy is enforced once the API server has loaded it, a moment
	// after it's created.
	require.Eventually(t, func() bool {
		return apierrors.IsInvalid(patch(`{"spec":{"dryRun":true}}`))
	}, 30*time.Second, 200*time.Millisecond, "the policy never denied the doorman a spec change")

	tests := []struct {
		name    string
		patch   string
		allowed bool
	}{
		{"stamps a request", `{"metadata":{"annotations":{` +
			`"hybernate.io/last-request":"2026-10-17T00:00:00Z","hybernate.io/last-request-from":"10.0.0.1"}}}`, true},
		{"removes its own stamp", `{"metadata":{"annotations":{"hybernate.io/last-request-from":null}}}`, true},
		{"changes the spec", `{"spec":{"dryRun":true}}`, false},
		{"adds a settings annotation", `{"metadata":{"annotations":{"hybernate.io/idle-after":"1m"}}}`, false},
		{"changes another annotation", `{"metadata":{"annotations":{"keep":"you"}}}`, false},
		{"removes another annotation", `{"metadata":{"annotations":{"keep":null}}}`, false},
		{"changes a label", `{"metadata":{"labels":{"team":"other"}}}`, false},
		{"adds a finalizer", `{"metadata":{"finalizers":["example.com/hold"]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := patch(tt.patch)
			if tt.allowed {
				assert.NoError(t, err)
				return
			}
			assert.True(t, apierrors.IsInvalid(err), "want the policy to deny it, got %v", err)
		})
	}

	t.Run("the operator isn't restricted", func(t *testing.T) {
		assert.NoError(t, patchAs(admin, `{"spec":{"dryRun":false}}`))
	})
}

func clientScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// apply creates the manifest's cluster-wide RBAC and admission policy, and
// returns the username of the doorman's ServiceAccount, wherever and
// whatever the manifest named it.
func apply(ctx context.Context, t *testing.T, c client.Client, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	applied := 0
	var doormanUser string
	for {
		var obj unstructured.Unstructured
		err := decoder.Decode(&obj.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if obj.GetKind() == "ServiceAccount" && strings.HasSuffix(obj.GetName(), "doorman") {
			doormanUser = "system:serviceaccount:" + obj.GetNamespace() + ":" + obj.GetName()
		}
		if !appliedKinds[obj.GetKind()] {
			continue
		}
		// Kustomize gives cluster-scoped objects its namespace too.
		obj.SetNamespace("")
		require.NoError(t, c.Create(ctx, &obj), "creating %s %s", obj.GetKind(), obj.GetName())
		applied++
	}
	require.Positive(t, applied, "the manifest has nothing to apply")
	require.NotEmpty(t, doormanUser, "the manifest has no doorman ServiceAccount")
	return doormanUser
}
