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

// ManagedWorkloadSpec defines the desired lifecycle behavior for a workload.
type ManagedWorkloadSpec struct {
	// Target identifies the workload to manage (e.g. a Deployment or StatefulSet).
	Target WorkloadRef `json:"target"`

	// DesiredState overrides automation and forces the workload into the given
	// state. When set, the operator stops evaluating idle/scale policies and
	// drives the workload to this state instead.
	// +optional
	DesiredState *DesiredState `json:"desiredState,omitempty"`

	// IdlePolicy configures automatic idle detection. Signals detect ground
	// truth, the prediction engine confirms the pattern, and after a grace
	// period the operator executes the configured action (pause or destroy).
	// +optional
	IdlePolicy *IdlePolicySpec `json:"idlePolicy,omitempty"`

	// ScalePolicy configures prediction-driven replica scaling with
	// stabilization windows, step limits, and min/max bounds.
	// +optional
	ScalePolicy *ScalePolicySpec `json:"scalePolicy,omitempty"`

	// Pause configures behavior while the workload is paused, including
	// automatic expiry and what action to take when the pause expires.
	// +optional
	Pause *PauseSpec `json:"pause,omitempty"`

	// Destroy configures behavior after the workload is destroyed, including
	// PVC retention and cleanup.
	// +optional
	Destroy *DestroySpec `json:"destroy,omitempty"`

	// Prediction configures the Holt-Winters forecasting engine that drives
	// idle detection confirmation and scale decisions.
	// +required
	Prediction PredictionSpec `json:"prediction"`

	// CostTracking configures custom cost rates for this workload.
	// Cost tracking is always enabled with AWS on-demand defaults.
	// Set this field only to override pricing rates.
	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

	// ConflictAction controls how the operator reacts when someone changes
	// the target's replicas outside of Hybernate. "enforce" corrects the
	// drift, "warn" emits an event but leaves the change, "defer" accepts
	// the external change and updates internal state to match.
	// +kubebuilder:default=warn
	// +optional
	ConflictAction ConflictAction `json:"conflictAction,omitempty"`

	// DryRun makes the operator evaluate all policies and emit events but
	// take no action. Useful for validating configuration before going live.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// +kubebuilder:validation:Enum=Deployment;StatefulSet
type TargetKind string

const (
	TargetKindDeployment  TargetKind = "Deployment"
	TargetKindStatefulSet TargetKind = "StatefulSet"
)

// WorkloadRef identifies the target workload by kind and name.
// The workload must exist in the same namespace as the ManagedWorkload CR.
type WorkloadRef struct {
	// +kubebuilder:default=Deployment
	Kind TargetKind `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// PredictionSpec configures the Holt-Winters forecasting engine.
type PredictionSpec struct {
	// Confidence is the minimum accuracy percentage (0-100) required before
	// the prediction engine transitions from suggesting (shadow mode) to
	// actively driving decisions.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=85
	Confidence int `json:"confidence"`
}

// +kubebuilder:validation:Enum=enforce;warn;defer
type ConflictAction string

const (
	ConflictActionEnforce ConflictAction = "enforce"
	ConflictActionWarn    ConflictAction = "warn"
	ConflictActionDefer   ConflictAction = "defer"
)

// +kubebuilder:validation:Enum=auto;pause;destroy
type IdleAction string

const (
	IdleActionAuto    IdleAction = "auto"
	IdleActionPause   IdleAction = "pause"
	IdleActionDestroy IdleAction = "destroy"
)

// IdlePolicySpec configures how idle detection works for this workload.
type IdlePolicySpec struct {
	// Action to take when the workload is confirmed idle. "auto" and "pause"
	// both scale to zero; "destroy" deletes the workload entirely.
	// +kubebuilder:default=auto
	Action IdleAction `json:"action"`

	// CPUIdleThreshold is the CPU utilization percentage of request below
	// which the workload is considered potentially idle (e.g. 10 means idle
	// if CPU usage < 10% of CPU request).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	CPUIdleThreshold int `json:"cpuIdleThreshold,omitempty"`

	// MemoryIdleThreshold is the memory utilization percentage of request
	// below which the workload is considered potentially idle. Both CPU and
	// memory must be below their respective thresholds for idle detection
	// to confirm.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	MemoryIdleThreshold int `json:"memoryIdleThreshold,omitempty"`

	// Signals are additional checks that must all confirm before the workload
	// is considered idle. An internal CPU usage check runs automatically;
	// these signals are layered on top for application-level confirmation.
	// +optional
	Signals []ProbeSpec `json:"signals,omitempty"`

	// GracePeriod is how long signals must continuously confirm idle before
	// the operator acts. Protects against brief quiet moments triggering
	// a false idle.
	// +optional
	// +kubebuilder:validation:Format=duration
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`

	// AutoResume re-enables the workload when idle detection clears
	// (i.e. signals no longer confirm idle).
	// +optional
	AutoResume bool `json:"autoResume,omitempty"`
}

// +kubebuilder:validation:Enum=prometheus
type ProbeSource string

const (
	ProbeSourcePrometheus ProbeSource = "prometheus"
)

// ProbeSpec defines an external check used for idle detection signals or
// scale-down guards. For Prometheus probes, the PromQL query must return a
// non-zero value to confirm the action. An empty result or zero value denies it.
type ProbeSpec struct {
	// +kubebuilder:default=prometheus
	Source ProbeSource `json:"source"`

	// PromQL is an instant query evaluated against the configured Prometheus
	// endpoint. The query must return a non-zero scalar to confirm the action.
	// Examples:
	//   Idle signal:      rate(http_requests_total{service="api"}[10m]) == 0
	//   Scale-down guard: sum(websocket_active_connections{service="api"}) < 100
	// +optional
	PromQL string `json:"promQL,omitempty"`
}

// +kubebuilder:validation:Enum=destroy;resume
type ExpireAction string

const (
	ExpireActionDestroy ExpireAction = "destroy"
	ExpireActionResume  ExpireAction = "resume"
)

// PauseSpec configures pause behavior and automatic expiry.
type PauseSpec struct {
	// ExpireAfter is the maximum duration a workload can remain paused.
	// After this period, the operator executes the ExpireAction.
	// +optional
	// +kubebuilder:validation:Format=duration
	ExpireAfter *metav1.Duration `json:"expireAfter,omitempty"`

	// ExpireAction determines what happens when ExpireAfter elapses.
	// "destroy" deletes the workload; "resume" scales it back up.
	// +kubebuilder:default=destroy
	// +optional
	ExpireAction ExpireAction `json:"expireAction,omitempty"`
}

// ScalePolicySpec configures prediction-driven replica scaling.
type ScalePolicySpec struct {
	// MinReplicas is the floor for scaling. The operator will never scale
	// below this value.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	MinReplicas int `json:"minReplicas"`

	// MaxReplicas is the ceiling for scaling. The operator will never scale
	// above this value.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int `json:"maxReplicas"`

	// OverrideReplicas bypasses prediction and forces the operator to scale
	// to this exact replica count (clamped to [minReplicas, maxReplicas]).
	// Stabilization and step limits still apply. Remove the field to return
	// to prediction-driven scaling.
	// +optional
	// +kubebuilder:validation:Minimum=0
	OverrideReplicas *int32 `json:"overrideReplicas,omitempty"`

	// Down configures scale-down behavior including stabilization, step
	// limits, and guard probes.
	// +optional
	Down *ScaleDirectionSpec `json:"down,omitempty"`

	// Up configures scale-up behavior including stabilization and step limits.
	// +optional
	Up *ScaleDirectionSpec `json:"up,omitempty"`
}

// ScaleDirectionSpec configures guardrails for a single scaling direction.
type ScaleDirectionSpec struct {
	// Stabilization is the cooldown period after a scale event in this
	// direction. Prevents oscillation by blocking same-direction scaling
	// until the window elapses.
	// +optional
	// +kubebuilder:validation:Format=duration
	Stabilization *metav1.Duration `json:"stabilization,omitempty"`

	// MaxStep limits how many replicas can be added or removed in a single
	// reconciliation loop. Enables gradual ramp-up or ramp-down.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxStep *int `json:"maxStep,omitempty"`

	// Guard probes that must all confirm before a scale-down proceeds.
	// An internal CPU capacity check runs automatically; these probes
	// add application-level safety gates (e.g. active connections, in-flight jobs).
	// +optional
	Guard []ProbeSpec `json:"guard,omitempty"`
}

// DestroySpec configures cleanup behavior after a workload is destroyed.
type DestroySpec struct {
	// PVCRetention is how long to keep PVCs after the workload is destroyed.
	// After this period, PVCs are deleted. Omit to delete PVCs immediately.
	// +optional
	// +kubebuilder:validation:Format=duration
	PVCRetention *metav1.Duration `json:"pvcRetention,omitempty"`

	// PVCRetentionWarning triggers a warning event this duration before
	// PVCs are deleted, giving users time to recover data if needed.
	// +optional
	// +kubebuilder:validation:Format=duration
	PVCRetentionWarning *metav1.Duration `json:"pvcRetentionWarning,omitempty"`
}

// CostTrackingSpec configures resource cost calculation for this workload.
// Cost tracking is always enabled. This struct exists to allow custom rate overrides.
type CostTrackingSpec struct {
	// Rates overrides the default cost rates. Omit to use AWS on-demand defaults.
	// +optional
	Rates *CostRates `json:"rates,omitempty"`
}

// CostRates holds per-unit cost rates. Users set these to match their
// cloud provider pricing. All values are in USD.
type CostRates struct {
	// CPUPerHour is the cost per vCPU-hour (default: $0.031, AWS on-demand).
	// +optional
	CPUPerHour *resource.Quantity `json:"cpuPerHour,omitempty"`

	// MemoryPerHour is the cost per GiB-hour (default: $0.004, AWS on-demand).
	// +optional
	MemoryPerHour *resource.Quantity `json:"memoryPerHour,omitempty"`

	// StoragePerMonth is the cost per GiB-month for PVC storage
	// (default: $0.08, AWS EBS gp3).
	// +optional
	StoragePerMonth *resource.Quantity `json:"storagePerMonth,omitempty"`
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

// ManagedWorkloadStatus reflects the observed state of the workload.
type ManagedWorkloadStatus struct {
	// Phase is the current lifecycle phase of the workload.
	// +optional
	Phase WorkloadPhase `json:"phase,omitempty"`

	// Conditions provide detailed status information following the standard
	// Kubernetes condition pattern (Type, Status, Reason, Message).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Pause holds state while the workload is paused.
	// +optional
	Pause *PauseStatus `json:"pause,omitempty"`

	// Scale holds state related to the most recent scaling event.
	// +optional
	Scale *ScaleStatus `json:"scale,omitempty"`

	// Destroy holds state after the workload is destroyed.
	// +optional
	Destroy *DestroyStatus `json:"destroy,omitempty"`

	// Prediction reflects the current state of the forecasting engine.
	// +optional
	Prediction *PredictionStatus `json:"prediction,omitempty"`

	// Cost holds accumulated resource cost data for the current month.
	// +optional
	Cost *CostStatus `json:"cost,omitempty"`

	// LastActedAt is when the operator last mutated the target workload
	// (scale, pause, resume, destroy, or drift correction).
	// +optional
	LastActedAt *metav1.Time `json:"lastActedAt,omitempty"`

	// LastTransitionTime is when the workload last changed phases.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// ResourceSnapshot captures the workload's resource profile at the moment of a
// lifecycle action so savings can be calculated without querying the target.
type ResourceSnapshot struct {
	// Replicas is the replica count at the time of the snapshot.
	Replicas int32 `json:"replicas"`

	// CPUMillis is total CPU request in millicores per replica.
	CPUMillis int64 `json:"cpuMillis"`

	// MemoryBytes is total memory request in bytes per replica.
	MemoryBytes int64 `json:"memoryBytes"`

	// StorageBytes is total PVC provisioned capacity in bytes.
	StorageBytes int64 `json:"storageBytes"`
}

// PauseStatus records state while the workload is paused.
type PauseStatus struct {
	// PreviousReplicas is the replica count before pausing, used to
	// restore on resume.
	PreviousReplicas int32 `json:"previousReplicas"`

	// PausedAt is when the workload was paused.
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`

	// Resources captures the workload's resource profile at pause time
	// for cost savings calculation.
	// +optional
	Resources *ResourceSnapshot `json:"resources,omitempty"`
}

// ScaleStatus records state from the most recent scaling event.
type ScaleStatus struct {
	// PreviousReplicas is the replica count before the last scale event.
	PreviousReplicas int32 `json:"previousReplicas"`

	// CurrentReplicas is the replica count after the last scale event.
	CurrentReplicas int32 `json:"currentReplicas"`

	// ScaledAt is when the last scale event occurred.
	ScaledAt *metav1.Time `json:"scaledAt,omitempty"`
}

// DestroyStatus records state after the workload is destroyed.
type DestroyStatus struct {
	// DestroyedAt is when the workload was destroyed.
	DestroyedAt *metav1.Time `json:"destroyedAt,omitempty"`

	// Resources captures the workload's resource profile at destroy time
	// for cost savings calculation.
	// +optional
	Resources *ResourceSnapshot `json:"resources,omitempty"`

	// PVCRetentionExpiresAt is when remaining PVCs will be cleaned up.
	// Only set when DestroySpec.PVCRetention is configured.
	// +optional
	PVCRetentionExpiresAt *metav1.Time `json:"pvcRetentionExpiresAt,omitempty"`
}

// PredictionStatus reflects the current state of the Holt-Winters engine's
// dual-season lifecycle.
type PredictionStatus struct {
	// DailyPhase is the daily season's lifecycle phase
	// (Observing, Suggesting, or Active).
	DailyPhase string `json:"dailyPhase"`

	// DailyConfidence is the daily season's prediction accuracy percentage.
	DailyConfidence int `json:"dailyConfidence"`

	// WeeklyPhase is the weekly season's lifecycle phase
	// (Observing, Suggesting, or Active).
	WeeklyPhase string `json:"weeklyPhase"`

	// WeeklyConfidence is the weekly season's prediction accuracy percentage.
	WeeklyConfidence int `json:"weeklyConfidence"`
}

// CostStatus holds accumulated resource cost data for the current billing period.
type CostStatus struct {
	// CurrentMonthCPUHours is total vCPU-hours consumed this month.
	CurrentMonthCPUHours resource.Quantity `json:"currentMonthCPUHours"`

	// CurrentMonthMemoryHours is total GiB-hours of memory consumed this month.
	CurrentMonthMemoryHours resource.Quantity `json:"currentMonthMemoryHours"`

	// CurrentMonthStorageHours is total GiB-hours of PVC storage provisioned this month.
	CurrentMonthStorageHours resource.Quantity `json:"currentMonthStorageHours"`

	// EstimatedMonthlyCost is the projected cost for the full month based
	// on current usage patterns. Set to "pending" on day 1 of the month.
	EstimatedMonthlyCost string `json:"estimatedMonthlyCost"`

	// EstimatedMonthlySavings is the projected dollar amount saved by Hybernate
	// actions (pause, scale-down, destroy) this month. These savings are only
	// realized when freed resources lead to node removal by a cluster autoscaler.
	EstimatedMonthlySavings string `json:"estimatedMonthlySavings"`

	// EstimatedCostWithoutManagement is what this workload would have cost
	// without Hybernate — the sum of estimated cost and estimated savings.
	EstimatedCostWithoutManagement string `json:"estimatedCostWithoutManagement"`

	// ResourceReduction tracks the concrete resources freed by Hybernate actions.
	// Unlike cost estimates, these values are always accurate regardless of
	// whether a cluster autoscaler removes the underlying nodes.
	// +optional
	ResourceReduction *ResourceReduction `json:"resourceReduction,omitempty"`

	// LastAccumulatedAt is when costs were last accumulated.
	// +optional
	LastAccumulatedAt *metav1.Time `json:"lastAccumulatedAt,omitempty"`
}

// ResourceReduction tracks the workload-level resources freed by Hybernate
// actions (pause, scale-down, destroy). These resources are released on the
// node when pods are removed, but the node itself is only removed if a
// cluster autoscaler determines it is underutilized.
type ResourceReduction struct {
	// CPUMillis is the total CPU millicores freed by removing pods.
	CPUMillis int64 `json:"cpuMillis"`

	// MemoryBytes is the total memory bytes freed by removing pods.
	MemoryBytes int64 `json:"memoryBytes"`

	// Replicas is the number of pod replicas removed.
	Replicas int32 `json:"replicas"`
}
