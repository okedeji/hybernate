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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=suggest;"auto-manage"
type PolicyMode string

const (
	PolicyModeSuggest    PolicyMode = "suggest"
	PolicyModeAutoManage PolicyMode = "auto-manage"
)

// +kubebuilder:validation:Enum=Active;Idle;Wasteful
type Classification string

const (
	ClassificationActive   Classification = "Active"
	ClassificationIdle     Classification = "Idle"
	ClassificationWasteful Classification = "Wasteful"
)

// WorkloadPolicySpec defines the discovery scope, classification thresholds,
// and default policies for workloads found in the namespace.
type WorkloadPolicySpec struct {
	// +kubebuilder:default={"Deployment","StatefulSet"}
	// +optional
	TargetKinds []TargetKind `json:"targetKinds,omitempty"`

	// +kubebuilder:default=suggest
	// +optional
	Mode PolicyMode `json:"mode,omitempty"`

	// +kubebuilder:default="10m"
	// +optional
	ScanInterval *metav1.Duration `json:"scanInterval,omitempty"`

	// CPU millis below which a workload is classified as Idle.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=50
	// +optional
	CPUIdleThreshold int `json:"cpuIdleThreshold,omitempty"`

	// Memory usage in bytes below which a workload is classified as Idle.
	// Both CPU and memory must be below their respective thresholds.
	// +kubebuilder:default=104857600
	// +optional
	MemoryIdleThreshold int64 `json:"memoryIdleThreshold,omitempty"`

	// CPU utilization percentage below which a non-idle workload is Wasteful.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	// +optional
	CPUWastefulThreshold int `json:"cpuWastefulThreshold,omitempty"`

	// Memory utilization percentage below which a non-idle workload is Wasteful.
	// A workload is Wasteful if either CPU or memory utilization is below
	// its respective threshold.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=30
	// +optional
	MemoryWastefulThreshold int `json:"memoryWastefulThreshold,omitempty"`

	// Target utilization percentage for right-sizing savings estimates.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=70
	// +optional
	RightSizeTarget int `json:"rightSizeTarget,omitempty"`

	// +optional
	Rates *CostRates `json:"rates,omitempty"`

	// DryRun default for ManagedWorkloads created in auto-manage mode.
	// +kubebuilder:default=true
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// Default idle policy copied into exported/auto-created ManagedWorkloads.
	// +kubebuilder:default={"action":"pause","cpuIdleThreshold":50,"memoryIdleThreshold":104857600,"gracePeriod":"5m0s","autoResume":true}
	IdlePolicy *IdlePolicySpec `json:"idlePolicy,omitempty"`

	// Default scaling policy copied into exported/auto-created ManagedWorkloads.
	// +kubebuilder:default={"minReplicas":1,"maxReplicas":10,"down":{"stabilization":"5m0s"},"up":{"stabilization":"2m0s"}}
	ScalePolicy *ScalePolicySpec `json:"scalePolicy,omitempty"`

	// Default pause behavior copied into exported/auto-created ManagedWorkloads.
	// +kubebuilder:default={"expireAfter":"168h0m0s","expireAction":"resume"}
	Pause *PauseSpec `json:"pause,omitempty"`

	// Default destroy behavior copied into exported/auto-created ManagedWorkloads.
	// +kubebuilder:default={"pvcRetention":"168h0m0s","pvcRetentionWarning":"24h0m0s"}
	Destroy *DestroySpec `json:"destroy,omitempty"`

	// Default prediction config copied into exported/auto-created ManagedWorkloads.
	// +kubebuilder:default={"confidence":85}
	Prediction *PredictionSpec `json:"prediction,omitempty"`

	// Default cost rate overrides copied into exported/auto-created ManagedWorkloads.
	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

	// Default conflict action for exported/auto-created ManagedWorkloads.
	// +kubebuilder:default=warn
	ConflictAction ConflictAction `json:"conflictAction,omitempty"`
}

// WorkloadPolicyStatus reports the results of the most recent namespace scan.
type WorkloadPolicyStatus struct {
	// +optional
	Summary DiscoverySummary `json:"summary,omitempty"`

	// +optional
	LastScanAt *metav1.Time `json:"lastScanAt,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Per-workload results from the last scan, capped at 500 entries
	// sorted by estimated savings descending.
	// +optional
	Discovered []DiscoveredWorkload `json:"discovered,omitempty"`
}

type DiscoverySummary struct {
	Total                     int    `json:"total"`
	Active                    int    `json:"active"`
	Idle                      int    `json:"idle"`
	Wasteful                  int    `json:"wasteful"`
	Managed                   int    `json:"managed"`
	EstimatedMonthlyCost      string `json:"estimatedMonthlyCost,omitempty"`
	EstimatedPotentialSavings string `json:"estimatedPotentialSavings,omitempty"`
}

type DiscoveredWorkload struct {
	Name                      string         `json:"name"`
	Kind                      TargetKind     `json:"kind"`
	Classification            Classification `json:"classification"`
	CPUUsageMillis            int64          `json:"cpuUsageMillis"`
	CPURequestMillis          int64          `json:"cpuRequestMillis"`
	Replicas                  int32          `json:"replicas"`
	MemoryUsageBytes          int64          `json:"memoryUsageBytes"`
	MemoryRequestBytes        int64          `json:"memoryRequestBytes"`
	StorageBytes              int64          `json:"storageBytes,omitempty"`
	UtilizationPercent        int            `json:"utilizationPercent"`
	MemoryUtilizationPercent  int            `json:"memoryUtilizationPercent,omitempty"`
	EstimatedMonthlyCost      string         `json:"estimatedMonthlyCost,omitempty"`
	EstimatedPotentialSavings string         `json:"estimatedPotentialSavings,omitempty"`
	Managed                   bool           `json:"managed"`
	Ignored                   bool           `json:"ignored,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Discovered",type=integer,JSONPath=`.status.summary.total`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.summary.active`
// +kubebuilder:printcolumn:name="Idle",type=integer,JSONPath=`.status.summary.idle`
// +kubebuilder:printcolumn:name="Wasteful",type=integer,JSONPath=`.status.summary.wasteful`
// +kubebuilder:printcolumn:name="Projected Cost",type=string,JSONPath=`.status.summary.estimatedMonthlyCost`
// +kubebuilder:printcolumn:name="Projected Savings",type=string,JSONPath=`.status.summary.estimatedPotentialSavings`
// +kubebuilder:resource:shortName=wp

// WorkloadPolicy scans its own namespace for workloads that are candidates
// for lifecycle management and reports classification and savings estimates.
type WorkloadPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadPolicySpec   `json:"spec,omitempty"`
	Status WorkloadPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadPolicyList contains a list of WorkloadPolicy.
type WorkloadPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadPolicy{}, &WorkloadPolicyList{})
}
