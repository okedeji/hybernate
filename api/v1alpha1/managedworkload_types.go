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

// +kubebuilder:validation:Enum=Running;Paused;Destroyed
type DesiredState string

const (
	DesiredStateRunning   DesiredState = "Running"
	DesiredStatePaused    DesiredState = "Paused"
	DesiredStateDestroyed DesiredState = "Destroyed"
)

type ManagedWorkloadSpec struct {
	// Target identifies the workload to manage.
	Target WorkloadRef `json:"target"`

	// +optional
	DesiredState *DesiredState `json:"desiredState,omitempty"`

	// +optional
	IdlePolicy *IdlePolicySpec `json:"idlePolicy,omitempty"`

	// +optional
	Pause *PauseSpec `json:"pause,omitempty"`

	// +optional
	ScalePolicy *ScalePolicySpec `json:"scalePolicy,omitempty"`

	// +optional
	Destroy *DestroySpec `json:"destroy,omitempty"`

	// +optional
	Prediction *PredictionSpec `json:"prediction,omitempty"`

	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

type WorkloadRef struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type PredictionSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=85
	Confidence int `json:"confidence"`
}

// +kubebuilder:validation:Enum=auto;pause;destroy
type IdleAction string

const (
	IdleActionAuto    IdleAction = "auto"
	IdleActionPause   IdleAction = "pause"
	IdleActionDestroy IdleAction = "destroy"
)

type IdlePolicySpec struct {
	// +kubebuilder:validation:Format=duration
	DetectAfter metav1.Duration `json:"detectAfter"`

	// +kubebuilder:default=auto
	Action IdleAction `json:"action"`

	// +optional
	Signals []SignalSpec `json:"signals,omitempty"`

	// +optional
	// +kubebuilder:validation:Format=duration
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`

	// +optional
	AutoResume bool `json:"autoResume,omitempty"`
}

// +kubebuilder:validation:Enum=internal;prometheus
type SignalSource string

const (
	SignalSourceInternal   SignalSource = "internal"
	SignalSourcePrometheus SignalSource = "prometheus"
)

type SignalSpec struct {
	// +kubebuilder:default=internal
	Source SignalSource `json:"source"`

	// +optional
	PromQL string `json:"promQL,omitempty"`
}

// +kubebuilder:validation:Enum=destroy;resume
type ExpireAction string

const (
	ExpireActionDestroy ExpireAction = "destroy"
	ExpireActionResume  ExpireAction = "resume"
)

type PauseSpec struct {
	// +optional
	// +kubebuilder:validation:Format=duration
	ExpireAfter *metav1.Duration `json:"expireAfter,omitempty"`

	// +kubebuilder:default=destroy
	// +optional
	ExpireAction ExpireAction `json:"expireAction,omitempty"`
}

type ScalePolicySpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	MinReplicas int `json:"minReplicas"`

	// +kubebuilder:validation:Minimum=1
	MaxReplicas int `json:"maxReplicas"`

	// +optional
	Signals []SignalSpec `json:"signals,omitempty"`

	// +optional
	Down *ScaleDirectionSpec `json:"down,omitempty"`

	// +optional
	Up *ScaleDirectionSpec `json:"up,omitempty"`
}

type ScaleDirectionSpec struct {
	// +optional
	// +kubebuilder:validation:Format=duration
	Stabilization *metav1.Duration `json:"stabilization,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxStep *int `json:"maxStep,omitempty"`
}

type DestroySpec struct {
	// +optional
	// +kubebuilder:validation:Format=duration
	PVCRetention *metav1.Duration `json:"pvcRetention,omitempty"`

	// +optional
	// +kubebuilder:validation:Format=duration
	PVCRetentionWarning *metav1.Duration `json:"pvcRetentionWarning,omitempty"`
}

type CostTrackingSpec struct {
	Enabled bool `json:"enabled"`

	// +optional
	Labels []string `json:"labels,omitempty"`
}

// --- Status ---

// +kubebuilder:validation:Enum=Creating;Running;Idle;Scaling;Pausing;Paused;Resuming;Destroying;Destroyed
type WorkloadPhase string

const (
	PhaseCreating   WorkloadPhase = "Creating"
	PhaseRunning    WorkloadPhase = "Running"
	PhaseIdle       WorkloadPhase = "Idle"
	PhaseScaling    WorkloadPhase = "Scaling"
	PhasePausing    WorkloadPhase = "Pausing"
	PhasePaused     WorkloadPhase = "Paused"
	PhaseResuming   WorkloadPhase = "Resuming"
	PhaseDestroying WorkloadPhase = "Destroying"
	PhaseDestroyed  WorkloadPhase = "Destroyed"
)

type ManagedWorkloadStatus struct {
	// +optional
	Phase WorkloadPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	Pause *PauseStatus `json:"pause,omitempty"`

	// +optional
	Scale *ScaleStatus `json:"scale,omitempty"`

	// +optional
	Destroy *DestroyStatus `json:"destroy,omitempty"`

	// +optional
	Prediction *PredictionStatus `json:"prediction,omitempty"`

	// +optional
	Cost *CostStatus `json:"cost,omitempty"`

	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

type PauseStatus struct {
	PreviousReplicas int32        `json:"previousReplicas"`
	PausedAt         *metav1.Time `json:"pausedAt,omitempty"`
}

type ScaleStatus struct {
	PreviousReplicas int32        `json:"previousReplicas"`
	CurrentReplicas  int32        `json:"currentReplicas"`
	ScaledAt         *metav1.Time `json:"scaledAt,omitempty"`
}

type DestroyStatus struct {
	DestroyedAt *metav1.Time `json:"destroyedAt,omitempty"`

	// +optional
	PVCRetentionExpiresAt *metav1.Time `json:"pvcRetentionExpiresAt,omitempty"`
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
