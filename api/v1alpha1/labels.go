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

	// FinalizerCleanup is the finalizer added to ManagedWorkloads for PVC retention cleanup.
	FinalizerCleanup = "hybernate.io/cleanup"
)
