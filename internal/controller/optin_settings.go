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

// settingProblem is an annotation value that couldn't be read, so the
// setting came from further down the order of precedence.
type settingProblem struct {
	annotation string
	value      string
	// on is where the annotation is: the workload or its namespace.
	on      string
	reason  string
	outcome string
}

func (p settingProblem) String() string {
	return fmt.Sprintf("%s=%q on the %s %s, so %s", p.annotation, p.value, p.on, p.reason, p.outcome)
}

// settings reads the opt-in annotations of a workload and its namespace.
// Each setting is the first value that can be read: the workload's, then
// its namespace's, then the fallback. A value that can't be read is
// reported and skipped, never replaced by the fallback, so a typo on a
// workload doesn't undo its namespace's setting.
type settings struct {
	workload, namespace map[string]string
	problems            []settingProblem
}

// resolve offers each value of key, in order of precedence, to read until
// read accepts one, and reports each one it rejects along with where the
// setting came from instead. read is told where the value is.
func (s *settings) resolve(key, invalid string, read func(value, on string) bool) {
	start := len(s.problems)
	outcome := "the default is used"
	for _, level := range []struct {
		on          string
		annotations map[string]string
	}{{"workload", s.workload}, {"namespace", s.namespace}} {
		v, ok := level.annotations[key]
		if !ok {
			continue
		}
		if read(v, level.on) {
			outcome = "the " + level.on + "'s value is used"
			break
		}
		s.problems = append(s.problems, settingProblem{annotation: key, value: v, on: level.on, reason: invalid})
	}
	for i := start; i < len(s.problems); i++ {
		if s.problems[i].outcome == "" {
			s.problems[i].outcome = outcome
		}
	}
}

// boolean accepts exactly "true" or "false", the spellings kubectl
// hybernate and the docs use, so every reader of the annotation agrees on
// what it says.
func (s *settings) boolean(key string, fallback bool) bool {
	b := fallback
	s.resolve(key, `isn't "true" or "false"`, func(v, _ string) bool {
		switch v {
		case v1alpha1.True:
			b = true
		case "false":
			b = false
		default:
			return false
		}
		return true
	})
	return b
}

// dryRun reads the dry-run annotation as a boolean, except that any value
// that can't be read along the way turns dry-run on: a typo in the setting
// meant to stop pauses must never start them.
func (s *settings) dryRun(fallback bool) bool {
	start := len(s.problems)
	on := s.boolean(v1alpha1.AnnotationDryRun, fallback)
	if len(s.problems) == start {
		return on
	}
	for i := start; i < len(s.problems); i++ {
		s.problems[i].outcome = "dry-run is on, to be safe"
	}
	return true
}

// duration accepts a Go duration above zero.
func (s *settings) duration(key string, fallback time.Duration) time.Duration {
	d := fallback
	s.resolve(key, `isn't a duration above zero, like "90m" or "2h"`, func(v, _ string) bool {
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			return false
		}
		d = parsed
		return true
	})
	return d
}

// percent accepts a whole number from 1 to 100.
func (s *settings) percent(key string, fallback int) int {
	n := fallback
	s.resolve(key, "isn't a whole number from 1 to 100", func(v, _ string) bool {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 100 {
			return false
		}
		n = parsed
		return true
	})
	return n
}

// dependencies reads a comma-separated list of "kind/name" or
// "namespace/kind/name". An entry that can't be read is reported and
// skipped while the others apply; only a value with no readable entry at
// all falls through to the namespace's. An empty value declares none, so a
// workload can drop its namespace's.
func (s *settings) dependencies(key string) []v1alpha1.DependencyRef {
	const invalid = `isn't "kind/name" or "namespace/kind/name", with kind deployment or statefulset`
	var deps []v1alpha1.DependencyRef
	s.resolve(key, invalid, func(v, on string) bool {
		deps = nil
		var bad []string
		for entry := range strings.SplitSeq(v, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if dep, ok := parseDependency(entry); ok {
				deps = append(deps, dep)
			} else {
				bad = append(bad, entry)
			}
		}
		if len(deps) == 0 && len(bad) > 0 {
			return false
		}
		for _, entry := range bad {
			s.problems = append(s.problems, settingProblem{annotation: key, value: entry, on: on, reason: invalid,
				outcome: "the entry is skipped"})
		}
		return true
	})
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

// optInSpec is the ManagedWorkload spec for an opted-in workload: for each
// setting its annotation, then its namespace's, then the cluster default,
// then what a ManagedWorkload written with no settings would get.
//
// The annotations control dryRun, idlePolicy (idleAfter, the CPU threshold
// and autoResume), which dependencies dependsOn lists, and wake. The rest
// is the user's to set on the ManagedWorkload; see keepUserSettings.
func optInSpec(target v1alpha1.WorkloadRef, workload, namespace map[string]string, d OptInDefaults) (
	v1alpha1.ManagedWorkloadSpec, []settingProblem) {
	s := &settings{workload: workload, namespace: namespace}
	spec := v1alpha1.ManagedWorkloadSpec{
		Target: target,
		DryRun: s.dryRun(d.DryRun),
		IdlePolicy: &v1alpha1.IdlePolicySpec{
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

// keepUserSettings carries over, from a label-created ManagedWorkload's
// current spec, everything no annotation sets, so an edit made to it, such
// as setting desiredState with kubectl patch, isn't undone. Those are
// desiredState, prediction, costTracking, the Prometheus activity queries,
// and waitForReady on a dependency the annotation still lists. The target
// is never changed once the ManagedWorkload exists.
func keepUserSettings(spec *v1alpha1.ManagedWorkloadSpec, current *v1alpha1.ManagedWorkloadSpec) {
	spec.Target = current.Target
	spec.DesiredState = current.DesiredState
	spec.Prediction = current.Prediction
	spec.CostTracking = current.CostTracking
	if current.IdlePolicy != nil && current.IdlePolicy.Activity != nil {
		spec.IdlePolicy.Activity.Prometheus = current.IdlePolicy.Activity.Prometheus
	}
	for i := range spec.DependsOn {
		dep := &spec.DependsOn[i]
		for _, have := range current.DependsOn {
			if have.Namespace == dep.Namespace && have.Kind == dep.Kind && have.Name == dep.Name {
				dep.WaitForReady = have.WaitForReady
			}
		}
	}
}
