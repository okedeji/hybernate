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
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// OptInDefaults are the cluster-wide settings for workloads opted in with
// the managed label, from the Helm values. Annotations override them.
type OptInDefaults struct {
	IdleAfter    time.Duration
	CPUThreshold int
	DryRun       bool
}

// DefaultOptInDefaults match a ManagedWorkload written with no settings.
var DefaultOptInDefaults = OptInDefaults{IdleAfter: defaultIdleAfter, CPUThreshold: defaultCPUThreshold}

// settingProblem is an annotation that couldn't be used, so its setting
// fell back to what it would be without it.
type settingProblem struct {
	annotation string
	value      string
	reason     string
}

func (p settingProblem) String() string {
	return fmt.Sprintf("%s=%q %s", p.annotation, p.value, p.reason)
}

// settings reads the opt-in annotations of a workload and its namespace;
// the workload's own wins.
type settings struct {
	workload, namespace map[string]string
	problems            []settingProblem
}

func (s *settings) lookup(key string) (string, bool) {
	if v, ok := s.workload[key]; ok {
		return v, true
	}
	v, ok := s.namespace[key]
	return v, ok
}

func (s *settings) problem(key, value, reason string) {
	s.problems = append(s.problems, settingProblem{annotation: key, value: value, reason: reason})
}

// boolean reads "true" or "false". Without the annotation, or with any
// other value, the setting is fallback.
func (s *settings) boolean(key string, fallback bool) bool {
	v, ok := s.lookup(key)
	if !ok {
		return fallback
	}
	switch v {
	case v1alpha1.True:
		return true
	case "false":
		return false
	}
	s.problem(key, v, `isn't "true" or "false"`)
	return fallback
}

func (s *settings) duration(key string, fallback time.Duration) time.Duration {
	v, ok := s.lookup(key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		s.problem(key, v, `isn't a duration like "90m" or "2h"`)
		return fallback
	}
	return d
}

func (s *settings) percent(key string, fallback int) int {
	v, ok := s.lookup(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 {
		s.problem(key, v, "isn't a whole number from 1 to 100")
		return fallback
	}
	return n
}

// dependencies reads a comma-separated list of "kind/name" or
// "namespace/kind/name", dropping any entry it can't read.
func (s *settings) dependencies(key string) []v1alpha1.DependencyRef {
	v, ok := s.lookup(key)
	if !ok {
		return nil
	}
	var deps []v1alpha1.DependencyRef
	for entry := range strings.SplitSeq(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		dep, ok := parseDependency(entry)
		if !ok {
			s.problem(key, entry, `isn't "kind/name" or "namespace/kind/name", with kind deployment or statefulset`)
			continue
		}
		deps = append(deps, dep)
	}
	return deps
}

func parseDependency(entry string) (v1alpha1.DependencyRef, bool) {
	parts := strings.Split(entry, "/")
	var dep v1alpha1.DependencyRef
	switch len(parts) {
	case 2:
		dep.Name = parts[1]
	case 3:
		dep.Namespace, dep.Name = parts[0], parts[2]
	default:
		return dep, false
	}
	switch strings.ToLower(parts[len(parts)-2]) {
	case "deployment":
		dep.Kind = v1alpha1.TargetKindDeployment
	case "statefulset":
		dep.Kind = v1alpha1.TargetKindStatefulSet
	default:
		return dep, false
	}
	return dep, dep.Name != "" && (len(parts) == 2 || dep.Namespace != "")
}

// optInSpec is the ManagedWorkload spec for an opted-in workload: its
// annotations, then its namespace's, then the cluster defaults, then what a
// ManagedWorkload written with no settings would get.
func optInSpec(target v1alpha1.WorkloadRef, workload, namespace map[string]string, d OptInDefaults) (
	v1alpha1.ManagedWorkloadSpec, []settingProblem) {
	s := &settings{workload: workload, namespace: namespace}
	spec := v1alpha1.ManagedWorkloadSpec{
		Target: target,
		DryRun: s.boolean(v1alpha1.AnnotationDryRun, d.DryRun),
		IdlePolicy: &v1alpha1.IdlePolicySpec{
			Action:     v1alpha1.IdleActionPause,
			IdleAfter:  &metav1.Duration{Duration: s.duration(v1alpha1.AnnotationIdleAfter, d.IdleAfter)},
			Activity:   &v1alpha1.ActivitySpec{CPUThreshold: s.percent(v1alpha1.AnnotationCPUThreshold, d.CPUThreshold)},
			AutoResume: s.boolean(v1alpha1.AnnotationAutoResume, false),
		},
		DependsOn: s.dependencies(v1alpha1.AnnotationDependsOn),
		Wake: &v1alpha1.WakeSpec{
			OnRequest: ptr.To(s.boolean(v1alpha1.AnnotationWakeOnRequest, true)),
			MaxWait:   &metav1.Duration{Duration: s.duration(v1alpha1.AnnotationWakeMaxWait, 2*time.Minute)},
			Page:      ptr.To(s.boolean(v1alpha1.AnnotationWakePage, true)),
		},
		// Every field the API server would default is set here, so the spec
		// written matches the one stored and rewriting it changes nothing.
		Prediction: v1alpha1.PredictionSpec{Confidence: 85},
	}
	return spec, s.problems
}
