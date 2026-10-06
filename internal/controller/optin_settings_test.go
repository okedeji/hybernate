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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

var apiTarget = v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"}

func TestOptInSpec_Defaults(t *testing.T) {
	spec, problems := optInSpec(apiTarget, nil, nil, DefaultOptInDefaults)

	require.Empty(t, problems)
	assert.Equal(t, apiTarget, spec.Target)
	assert.False(t, spec.DryRun)
	assert.Equal(t, time.Hour, spec.IdlePolicy.IdleAfter.Duration)
	assert.Equal(t, 10, spec.IdlePolicy.Activity.CPUThreshold)
	assert.False(t, spec.IdlePolicy.AutoResume)
	assert.True(t, *spec.Wake.OnRequest)
	assert.Equal(t, 2*time.Minute, spec.Wake.MaxWait.Duration)
	assert.True(t, *spec.Wake.Page)
	// Fields the API server would default, set so a rewrite changes nothing.
	assert.Equal(t, 75, spec.Prediction.Confidence)
}

func TestOptInSpec_Precedence(t *testing.T) {
	cluster := OptInDefaults{IdleAfter: 3 * time.Hour, CPUThreshold: 20, DryRun: true}
	namespace := map[string]string{v1alpha1.AnnotationIdleAfter: "2h", v1alpha1.AnnotationCPUThreshold: "15"}
	workload := map[string]string{v1alpha1.AnnotationIdleAfter: "30m"}

	spec, problems := optInSpec(apiTarget, workload, namespace, cluster)

	require.Empty(t, problems)
	assert.Equal(t, 30*time.Minute, spec.IdlePolicy.IdleAfter.Duration, "the workload's own wins")
	assert.Equal(t, 15, spec.IdlePolicy.Activity.CPUThreshold, "then its namespace's")
	assert.True(t, spec.DryRun, "then the cluster default")
}

func TestOptInSpec_DryRun(t *testing.T) {
	tests := []struct {
		name      string
		workload  map[string]string
		namespace map[string]string
		want      bool
	}{
		{name: "not annotated", want: false},
		{name: "annotated true", workload: map[string]string{v1alpha1.AnnotationDryRun: "true"}, want: true},
		{name: "annotated false", workload: map[string]string{v1alpha1.AnnotationDryRun: "false"}, want: false},
		{name: "the namespace measures", namespace: map[string]string{v1alpha1.AnnotationDryRun: "true"}, want: true},
		{name: "a workload goes live in a measuring namespace",
			workload:  map[string]string{v1alpha1.AnnotationDryRun: "false"},
			namespace: map[string]string{v1alpha1.AnnotationDryRun: "true"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, problems := optInSpec(apiTarget, tt.workload, tt.namespace, DefaultOptInDefaults)
			require.Empty(t, problems)
			assert.Equal(t, tt.want, spec.DryRun)
		})
	}
}

// A bad value is reported and falls back to what the setting would be
// without it, except dry-run, which turns on; the other settings still
// apply.
func TestOptInSpec_InvalidValues(t *testing.T) {
	tests := []struct {
		annotation string
		value      string
		check      func(t *testing.T, spec v1alpha1.ManagedWorkloadSpec)
	}{
		{v1alpha1.AnnotationDryRun, "yes", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, s.DryRun) }},
		{v1alpha1.AnnotationIdleAfter, "0s", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, time.Hour, s.IdlePolicy.IdleAfter.Duration)
		}},
		{v1alpha1.AnnotationWakeMaxWait, "0", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, 2*time.Minute, s.Wake.MaxWait.Duration)
		}},
		{v1alpha1.AnnotationWakeOnRequest, "yes", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, *s.Wake.OnRequest) }},
		{v1alpha1.AnnotationIdleAfter, "soon", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, time.Hour, s.IdlePolicy.IdleAfter.Duration)
		}},
		{v1alpha1.AnnotationIdleAfter, "-5m", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, time.Hour, s.IdlePolicy.IdleAfter.Duration)
		}},
		{v1alpha1.AnnotationCPUThreshold, "0", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, 10, s.IdlePolicy.Activity.CPUThreshold)
		}},
		{v1alpha1.AnnotationCPUThreshold, "150", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
			assert.Equal(t, 10, s.IdlePolicy.Activity.CPUThreshold)
		}},
		{v1alpha1.AnnotationWakePage, "False", func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, *s.Wake.Page) }},
	}
	for _, tt := range tests {
		t.Run(tt.annotation+"="+tt.value, func(t *testing.T) {
			annotations := map[string]string{tt.annotation: tt.value, v1alpha1.AnnotationAutoResume: "true"}

			spec, problems := optInSpec(apiTarget, annotations, nil, DefaultOptInDefaults)

			require.Len(t, problems, 1)
			assert.Equal(t, tt.annotation, problems[0].annotation)
			assert.Contains(t, problems[0].String(), tt.value)
			tt.check(t, spec)
			assert.True(t, spec.IdlePolicy.AutoResume, "the other settings still apply")
		})
	}
}

func TestOptInSpec_DependsOn(t *testing.T) {
	annotations := map[string]string{
		v1alpha1.AnnotationDependsOn: "statefulset/postgres, messaging/statefulset/nats ,Deployment/cache,cronjob/report,bad",
	}

	spec, problems := optInSpec(apiTarget, annotations, nil, DefaultOptInDefaults)

	assert.Equal(t, []v1alpha1.DependencyRef{
		{Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"},
		{Namespace: "messaging", Kind: v1alpha1.TargetKindStatefulSet, Name: "nats"},
		{Kind: v1alpha1.TargetKindDeployment, Name: "cache"},
	}, spec.DependsOn)
	require.Len(t, problems, 2, "the unsupported kind and the malformed entry")
	assert.Equal(t, "cronjob/report", problems[0].value)
	assert.Equal(t, "bad", problems[1].value)
}

// A value on the workload that can't be read falls through to its
// namespace's, then the default, as a missing one would; it's never taken
// to mean the default. Dry-run fails safe instead: any value along the way
// that can't be read turns it on.
func TestOptInSpec_InvalidWorkloadValueFallsThrough(t *testing.T) {
	tests := []struct {
		name        string
		workload    map[string]string
		namespace   map[string]string
		check       func(t *testing.T, s v1alpha1.ManagedWorkloadSpec)
		wantOutcome string
	}{
		{name: "dry-run spelled differently keeps the namespace's dry-run",
			workload:    map[string]string{v1alpha1.AnnotationDryRun: "True"},
			namespace:   map[string]string{v1alpha1.AnnotationDryRun: "true"},
			check:       func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, s.DryRun) },
			wantOutcome: "dry-run is on, to be safe"},
		{name: "an unreadable dry-run never goes live",
			workload:    map[string]string{v1alpha1.AnnotationDryRun: "False"},
			namespace:   map[string]string{v1alpha1.AnnotationDryRun: "false"},
			check:       func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, s.DryRun) },
			wantOutcome: "dry-run is on, to be safe"},
		{name: "an unreadable dry-run on the namespace",
			namespace:   map[string]string{v1alpha1.AnnotationDryRun: "yes"},
			check:       func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.True(t, s.DryRun) },
			wantOutcome: "dry-run is on, to be safe"},
		{name: "idle-after",
			workload:  map[string]string{v1alpha1.AnnotationIdleAfter: "soon"},
			namespace: map[string]string{v1alpha1.AnnotationIdleAfter: "3h"},
			check: func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
				assert.Equal(t, 3*time.Hour, s.IdlePolicy.IdleAfter.Duration)
			},
			wantOutcome: "the namespace's value is used"},
		{name: "idle-after unreadable on both",
			workload:  map[string]string{v1alpha1.AnnotationIdleAfter: "soon"},
			namespace: map[string]string{v1alpha1.AnnotationIdleAfter: "later"},
			check: func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
				assert.Equal(t, time.Hour, s.IdlePolicy.IdleAfter.Duration)
			},
			wantOutcome: "the default is used"},
		{name: "cpu-threshold",
			workload:  map[string]string{v1alpha1.AnnotationCPUThreshold: "0"},
			namespace: map[string]string{v1alpha1.AnnotationCPUThreshold: "25"},
			check: func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
				assert.Equal(t, 25, s.IdlePolicy.Activity.CPUThreshold)
			},
			wantOutcome: "the namespace's value is used"},
		{name: "wake-page",
			workload:    map[string]string{v1alpha1.AnnotationWakePage: "no"},
			namespace:   map[string]string{v1alpha1.AnnotationWakePage: "false"},
			check:       func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) { assert.False(t, *s.Wake.Page) },
			wantOutcome: "the namespace's value is used"},
		{name: "depends-on with no readable entry",
			workload:  map[string]string{v1alpha1.AnnotationDependsOn: "postgres"},
			namespace: map[string]string{v1alpha1.AnnotationDependsOn: "shared/statefulset/postgres"},
			check: func(t *testing.T, s v1alpha1.ManagedWorkloadSpec) {
				assert.Equal(t, []v1alpha1.DependencyRef{
					{Namespace: "shared", Kind: v1alpha1.TargetKindStatefulSet, Name: "postgres"}}, s.DependsOn)
			},
			wantOutcome: "the namespace's value is used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, problems := optInSpec(apiTarget, tt.workload, tt.namespace, DefaultOptInDefaults)

			tt.check(t, spec)
			require.NotEmpty(t, problems)
			for _, p := range problems {
				assert.Equal(t, tt.wantOutcome, p.outcome, p.String())
			}
		})
	}
}

// A namespace's value that can't be read doesn't matter, and isn't
// reported, when the workload sets its own.
func TestOptInSpec_WorkloadValueShadowsNamespaceProblem(t *testing.T) {
	spec, problems := optInSpec(apiTarget,
		map[string]string{v1alpha1.AnnotationDryRun: "false"},
		map[string]string{v1alpha1.AnnotationDryRun: "maybe"}, DefaultOptInDefaults)

	assert.Empty(t, problems)
	assert.False(t, spec.DryRun)
}

// An empty depends-on on a workload drops its namespace's.
func TestOptInSpec_EmptyDependsOnDropsTheNamespaces(t *testing.T) {
	spec, problems := optInSpec(apiTarget,
		map[string]string{v1alpha1.AnnotationDependsOn: ""},
		map[string]string{v1alpha1.AnnotationDependsOn: "statefulset/postgres"}, DefaultOptInDefaults)

	assert.Empty(t, problems)
	assert.Empty(t, spec.DependsOn)
}
