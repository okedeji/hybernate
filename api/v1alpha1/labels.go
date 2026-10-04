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

// True is the value that turns a Hybernate label or boolean annotation on.
// Any other value, "True" and "yes" included, leaves it off.
const True = "true"

const (
	// LabelManaged opts a Deployment or StatefulSet, or every one in a
	// namespace, in to Hybernate: "true" gets it a ManagedWorkload.
	LabelManaged = "hybernate.io/managed"

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

	// FinalizerCleanup is the finalizer added to ManagedWorkloads for PVC retention cleanup.
	FinalizerCleanup = "hybernate.io/cleanup"
)
