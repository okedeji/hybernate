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

package export

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func testPolicy() *v1alpha1.WorkloadPolicy {
	return &v1alpha1.WorkloadPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "staging-policy",
			Namespace: "staging",
		},
		Spec: v1alpha1.WorkloadPolicySpec{
			DryRun:         true,
			ConflictAction: v1alpha1.ConflictActionWarn,
			Prediction:     &v1alpha1.PredictionSpec{Confidence: 85},
		},
		Status: v1alpha1.WorkloadPolicyStatus{
			Discovered: []v1alpha1.DiscoveredWorkload{
				{
					Name:           "idle-app",
					Kind:           v1alpha1.TargetKindDeployment,
					Classification: v1alpha1.ClassificationIdle,
				},
				{
					Name:           "wasteful-svc",
					Kind:           v1alpha1.TargetKindDeployment,
					Classification: v1alpha1.ClassificationWasteful,
				},
				{
					Name:           "active-api",
					Kind:           v1alpha1.TargetKindDeployment,
					Classification: v1alpha1.ClassificationActive,
				},
				{
					Name:           "managed-db",
					Kind:           v1alpha1.TargetKindStatefulSet,
					Classification: v1alpha1.ClassificationIdle,
					Managed:        true,
				},
				{
					Name:           "ignored-job",
					Kind:           v1alpha1.TargetKindDeployment,
					Classification: v1alpha1.ClassificationIdle,
					Ignored:        true,
				},
			},
		},
	}
}

func TestGenerate(t *testing.T) {
	tests := []struct {
		name            string
		filter          Filter
		wantNames       []string
		wantSkipReasons map[string]string
	}{
		{
			name:      "no filter returns all unmanaged non-ignored workloads",
			filter:    Filter{},
			wantNames: []string{"deployment-idle-app", "deployment-wasteful-svc", "deployment-active-api"},
			wantSkipReasons: map[string]string{
				"managed-db":  "already has a ManagedWorkload",
				"ignored-job": "ignored (hybernate.io/ignore label)",
			},
		},
		{
			name:      "filter by idle classification",
			filter:    Filter{Classifications: []v1alpha1.Classification{v1alpha1.ClassificationIdle}},
			wantNames: []string{"deployment-idle-app"},
		},
		{
			name:      "filter by wasteful classification",
			filter:    Filter{Classifications: []v1alpha1.Classification{v1alpha1.ClassificationWasteful}},
			wantNames: []string{"deployment-wasteful-svc"},
		},
		{
			name:      "filter by active classification",
			filter:    Filter{Classifications: []v1alpha1.Classification{v1alpha1.ClassificationActive}},
			wantNames: []string{"deployment-active-api"},
		},
		{
			name:      "filter by name",
			filter:    Filter{Name: "idle-app"},
			wantNames: []string{"deployment-idle-app"},
		},
		{
			name:      "filter by name no match",
			filter:    Filter{Name: "nonexistent"},
			wantNames: nil,
		},
		{
			name:      "include managed",
			filter:    Filter{IncludeManaged: true},
			wantNames: []string{"deployment-idle-app", "deployment-wasteful-svc", "deployment-active-api", "statefulset-managed-db"},
			wantSkipReasons: map[string]string{
				"ignored-job": "ignored (hybernate.io/ignore label)",
			},
		},
		{
			name: "include managed with active filter returns all active including managed",
			filter: Filter{
				IncludeManaged:  true,
				Classifications: []v1alpha1.Classification{v1alpha1.ClassificationIdle},
			},
			wantNames: []string{"deployment-idle-app", "statefulset-managed-db"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Generate(testPolicy(), tt.filter)

			names := make([]string, 0, len(result.Workloads))
			for _, mw := range result.Workloads {
				names = append(names, mw.Name)
			}
			if tt.wantNames == nil {
				assert.Empty(t, names)
			} else {
				assert.Equal(t, tt.wantNames, names)
			}

			if tt.wantSkipReasons != nil {
				skipReasons := make(map[string]string)
				for _, s := range result.Skipped {
					skipReasons[s.Name] = s.Reason
				}
				for name, reason := range tt.wantSkipReasons {
					assert.Equal(t, reason, skipReasons[name], "skip reason for %s", name)
				}
			}
		})
	}
}

func TestGenerateSpecFields(t *testing.T) {
	policy := testPolicy()
	result := Generate(policy, Filter{})

	require.NotEmpty(t, result.Workloads)
	mw := result.Workloads[0]

	assert.Equal(t, v1alpha1.GroupVersion.String(), mw.APIVersion)
	assert.Equal(t, "ManagedWorkload", mw.Kind)
	assert.Equal(t, "staging", mw.Namespace)
	assert.Equal(t, "true", mw.Labels[v1alpha1.LabelAutoDiscovered])
	assert.Equal(t, "staging-policy", mw.Annotations[v1alpha1.AnnotationWorkloadPolicy])
	assert.True(t, mw.Spec.DryRun)
	assert.Equal(t, 85, mw.Spec.Prediction.Confidence)
	assert.Equal(t, v1alpha1.ConflictActionWarn, mw.Spec.ConflictAction)
}

func TestResourceName(t *testing.T) {
	assert.Equal(t, "deployment-my-app", ResourceName(v1alpha1.TargetKindDeployment, "my-app"))
	assert.Equal(t, "statefulset-redis", ResourceName(v1alpha1.TargetKindStatefulSet, "redis"))
}

func TestWriteYAML(t *testing.T) {
	result := Generate(testPolicy(), Filter{})
	require.NotEmpty(t, result.Workloads)

	var buf bytes.Buffer
	require.NoError(t, WriteYAML(&buf, result.Workloads))

	out := buf.String()
	assert.Contains(t, out, "name: deployment-idle-app")
	assert.Contains(t, out, "name: deployment-wasteful-svc")
	assert.Contains(t, out, "---")
}

func TestWriteFiles(t *testing.T) {
	result := Generate(testPolicy(), Filter{})
	require.NotEmpty(t, result.Workloads)

	dir := t.TempDir()
	require.NoError(t, WriteFiles(dir, result.Workloads))

	for _, mw := range result.Workloads {
		path := filepath.Join(dir, mw.Name+".yaml")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(data), mw.Name)
	}
}
