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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ManagedWorkload declares a workload whose lifecycle is managed by Hybernate.
type ManagedWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ManagedWorkloadSpec `json:"spec"`

	// +optional
	Status ManagedWorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ManagedWorkloadList contains a list of ManagedWorkload.
type ManagedWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ManagedWorkload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ManagedWorkload{}, &ManagedWorkloadList{})
}

// --- Spec ---

type ManagedWorkloadSpec struct {
	// Template identifies the workload to manage.
	Template WorkloadTemplate `json:"template"`

	// +optional
	WarmPool *WarmPoolSpec `json:"warmPool,omitempty"`

	// +optional
	IdlePolicy *IdlePolicySpec `json:"idlePolicy,omitempty"`

	// +optional
	PausePolicy *PausePolicySpec `json:"pausePolicy,omitempty"`

	// +optional
	ScaleDown *ScalingSpec `json:"scaleDown,omitempty"`

	// +optional
	ScaleUp *ScalingSpec `json:"scaleUp,omitempty"`

	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

type WorkloadTemplate struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// +kubebuilder:pruning:PreserveUnknownFields
	Spec runtime.RawExtension `json:"spec"`
}

type WarmPoolSpec struct {
	Enabled bool `json:"enabled"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	MinReady int `json:"minReady"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=100
	MaxReady int `json:"maxReady"`

	// Confidence threshold as a percentage (0-100). The prediction engine
	// must exceed this before driving real pool sizing.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=85
	// +optional
	Confidence int `json:"confidence,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=168
	// +optional
	MinDataPoints int `json:"minDataPoints,omitempty"`

	// +optional
	Override *int `json:"override,omitempty"`
}

// +kubebuilder:validation:Enum=auto;scaleDown;pause;destroy
type IdleAction string

const (
	IdleActionAuto      IdleAction = "auto"
	IdleActionScaleDown IdleAction = "scaleDown"
	IdleActionPause     IdleAction = "pause"
	IdleActionDestroy   IdleAction = "destroy"
)

type IdlePolicySpec struct {
	// +kubebuilder:validation:Format=duration
	DetectAfter metav1.Duration `json:"detectAfter"`

	// +kubebuilder:default=auto
	Action IdleAction `json:"action"`

	// +optional
	Signal *SignalSpec `json:"signal,omitempty"`

	// +optional
	// +kubebuilder:validation:Format=duration
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
}

// +kubebuilder:validation:Enum=internal;prometheus;webhook
type SignalSource string

const (
	SignalSourceInternal   SignalSource = "internal"
	SignalSourcePrometheus SignalSource = "prometheus"
	SignalSourceWebhook    SignalSource = "webhook"
)

type SignalSpec struct {
	// +kubebuilder:default=internal
	Source SignalSource `json:"source"`

	// +optional
	Query string `json:"query,omitempty"`

	// +optional
	URL string `json:"url,omitempty"`
}

// +kubebuilder:validation:Enum=internal
type StorageBackendType string

const (
	StorageBackendInternal StorageBackendType = "internal"
)

type PausePolicySpec struct {
	// +kubebuilder:default=internal
	SnapshotStorage StorageBackendType `json:"snapshotStorage"`

	// +optional
	// +kubebuilder:validation:Format=duration
	MaxPauseDuration *metav1.Duration `json:"maxPauseDuration,omitempty"`

	// +optional
	// +kubebuilder:validation:Format=duration
	ResumeTime *metav1.Duration `json:"resumeTime,omitempty"`
}

type ScalingSpec struct {
	// +optional
	// +kubebuilder:validation:Format=duration
	Stabilization *metav1.Duration `json:"stabilization,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxStep *int `json:"maxStep,omitempty"`
}

type CostTrackingSpec struct {
	Enabled bool `json:"enabled"`

	// +optional
	Labels []string `json:"labels,omitempty"`
}

// --- Status ---

// +kubebuilder:validation:Enum=Creating;Running;Idle;Pausing;Paused;Resuming;Destroyed
type WorkloadPhase string

const (
	PhaseCreating  WorkloadPhase = "Creating"
	PhaseRunning   WorkloadPhase = "Running"
	PhaseIdle      WorkloadPhase = "Idle"
	PhasePausing   WorkloadPhase = "Pausing"
	PhasePaused    WorkloadPhase = "Paused"
	PhaseResuming  WorkloadPhase = "Resuming"
	PhaseDestroyed WorkloadPhase = "Destroyed"
)

type ManagedWorkloadStatus struct {
	// +optional
	Phase WorkloadPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Pool *PoolStatus `json:"pool,omitempty"`

	// +optional
	Prediction *PredictionStatus `json:"prediction,omitempty"`

	// +optional
	Cost *CostStatus `json:"cost,omitempty"`

	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

type PoolStatus struct {
	Ready  int `json:"ready"`
	InUse  int `json:"inUse"`
	Target int `json:"target"`
}

type PredictionStatus struct {
	Phase      string `json:"phase"`
	Confidence int    `json:"confidence"`
	DataPoints int    `json:"dataPoints"`
}

type CostStatus struct {
	CurrentMonthCPUHours    resource.Quantity `json:"currentMonthCPUHours"`
	CurrentMonthMemoryHours resource.Quantity `json:"currentMonthMemoryHours"`
	EstimatedMonthlyCost    string            `json:"estimatedMonthlyCost"`
}
