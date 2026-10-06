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

package v1alpha1

import "path"

// True is the value that turns a Hybernate label or boolean annotation on.
// Any other value, "True" and "yes" included, leaves it off.
const True = "true"

// FieldManager is the name Hybernate's writes, such as the replica count it
// pauses, are made under, so the API server attributes them to it where it
// records who set a field.
const FieldManager = "hybernate"

const (
	// LabelManaged opts a Deployment or StatefulSet, or every one in a
	// namespace, in to Hybernate: "true" gets it a ManagedWorkload.
	LabelManaged = "hybernate.io/managed"

	// LabelProtected marks a namespace Hybernate must never manage, such as
	// production: nothing in it is opted in or paused, whatever its labels.
	LabelProtected = "hybernate.io/protected"

	// LabelAllowProtected lets Hybernate manage a protected namespace, one
	// marked with LabelProtected or matching the operator's protected
	// namespace patterns. It's set on purpose, never by Hybernate.
	LabelAllowProtected = "hybernate.io/allow-protected"

	// LabelIgnore excludes a workload from management, even in a managed
	// namespace.
	LabelIgnore = "hybernate.io/ignore"

	// LabelFromLabel marks a ManagedWorkload created because its workload
	// or namespace carries LabelManaged. Its spec follows the annotations
	// and is rewritten from them.
	LabelFromLabel = "hybernate.io/from-label"

	// AnnotationLastActivity records activity seen by another tool, such as a
	// developer portal or CI pipeline, as an RFC 3339 time. Set on the ManagedWorkload
	// or its target. A newer value wakes a paused workload.
	AnnotationLastActivity = "hybernate.io/last-activity"

	// AnnotationActiveUntil keeps a workload awake until an RFC 3339 time.
	// Set on the ManagedWorkload or its target.
	AnnotationActiveUntil = "hybernate.io/active-until"

	// AnnotationLastRequest records, as an RFC 3339 time, a request the
	// doorman is holding for a paused workload. It wakes the workload like
	// AnnotationLastActivity, and the clock records the wake as a request.
	AnnotationLastRequest = "hybernate.io/last-request"

	// AnnotationLastRequestFrom is the address the request that woke a
	// paused workload came from, stamped by the doorman with
	// AnnotationLastRequest, so the operator can learn which workload sent
	// it.
	AnnotationLastRequestFrom = "hybernate.io/last-request-from"

	// AnnotationPauseRequested asks Hybernate to pause a ManagedWorkload's
	// workload now rather than when its idle clock runs out. The value is a
	// token, such as the time of the request, that only needs to differ from
	// the last request's: Hybernate acts on each value once, recording it in
	// status.lastPauseRequest. kubectl hybernate pause sets it.
	AnnotationPauseRequested = "hybernate.io/pause-requested"

	// Settings for a workload opted in with LabelManaged, set on the
	// workload or its namespace; the workload's own wins.
	AnnotationDryRun        = "hybernate.io/dry-run"
	AnnotationIdleAfter     = "hybernate.io/idle-after"
	AnnotationCPUThreshold  = "hybernate.io/cpu-threshold"
	AnnotationAutoResume    = "hybernate.io/auto-resume"
	AnnotationDependsOn     = "hybernate.io/depends-on"
	AnnotationWakeOnRequest = "hybernate.io/wake-on-request"
	AnnotationWakeMaxWait   = "hybernate.io/wake-max-wait"
	AnnotationWakePage      = "hybernate.io/wake-page"

	// AnnotationIgnoreDependencies lists dependencies Hybernate shouldn't
	// learn for a workload, comma-separated, as namespace/name or a name in
	// its own namespace. Set on the workload or its ManagedWorkload.
	AnnotationIgnoreDependencies = "hybernate.io/ignore-dependencies"

	// FinalizerCleanup is the finalizer added to ManagedWorkloads, so deleting
	// one scales its paused workload back up first.
	FinalizerCleanup = "hybernate.io/cleanup"
)

// Protected says Hybernate must not manage the namespace: it's labelled
// protected, or its name matches one of patterns (shell globs, such as
// prod-*), and it isn't labelled to allow it anyway.
func Protected(name string, labels map[string]string, patterns []string) bool {
	if labels[LabelAllowProtected] == True {
		return false
	}
	if labels[LabelProtected] == True {
		return true
	}
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}
