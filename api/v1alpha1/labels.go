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

const (
	// LabelIgnore excludes a workload from discovery and management.
	LabelIgnore = "hybernate.io/ignore"

	// LabelAutoDiscovered marks a ManagedWorkload created by auto-manage mode.
	LabelAutoDiscovered = "hybernate.io/auto-discovered"

	// AnnotationWorkloadPolicy links a ManagedWorkload back to the WorkloadPolicy that created it.
	AnnotationWorkloadPolicy = "hybernate.io/workload-policy"

	// AnnotationLastActivity records activity seen by another tool, such as a
	// sandbox UI or CI pipeline, as an RFC 3339 time. Set on the ManagedWorkload
	// or its target. A newer value wakes a paused workload.
	AnnotationLastActivity = "hybernate.io/last-activity"

	// AnnotationActiveUntil keeps a workload awake until an RFC 3339 time.
	// Set on the ManagedWorkload or its target.
	AnnotationActiveUntil = "hybernate.io/active-until"

	// AnnotationLastRequest records, as an RFC 3339 time, a request the
	// doorman is holding for a paused workload. It wakes the workload like
	// AnnotationLastActivity, and the clock records the wake as a request.
	AnnotationLastRequest = "hybernate.io/last-request"

	// FinalizerCleanup is the finalizer added to ManagedWorkloads for PVC retention cleanup.
	FinalizerCleanup = "hybernate.io/cleanup"
)
