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
	// state. When set, the operator stops evaluating the idle policy and
	// drives the workload to this state instead.
	// +optional
	DesiredState *DesiredState `json:"desiredState,omitempty"`

	// IdlePolicy configures automatic idle detection. Signals detect ground
	// truth, the prediction engine confirms the pattern, and after a grace
	// period the operator executes the configured action (pause or destroy).
	// +optional
	IdlePolicy *IdlePolicySpec `json:"idlePolicy,omitempty"`

	// DependsOn lists the workloads this one needs, such as a database or a
	// message broker. A dependency isn't paused while anything that depends
	// on it is awake, and waking this workload wakes its dependencies.
	// +listType=atomic
	// +optional
	DependsOn []DependencyRef `json:"dependsOn,omitempty"`

	// Wake configures waking the workload when a request reaches it while
	// it's paused.
	// +optional
	Wake *WakeSpec `json:"wake,omitempty"`

	// Pause configures behavior while the workload is paused, including
	// automatic expiry and what action to take when the pause expires.
	// +optional
	Pause *PauseSpec `json:"pause,omitempty"`

	// Destroy configures behavior after the workload is destroyed, including
	// PVC retention and cleanup.
	// +optional
	Destroy *DestroySpec `json:"destroy,omitempty"`

	// Prediction configures the Holt-Winters forecasting engine that confirms
	// idle detection and wakes paused workloads ahead of predicted demand.
	// +required
	Prediction PredictionSpec `json:"prediction"`

	// CostTracking configures custom cost rates for this workload.
	// Cost tracking is always enabled with AWS on-demand defaults.
	// Set this field only to override pricing rates.
	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

	// ConflictAction controls how the operator reacts when someone scales up
	// a paused target outside of Hybernate. "enforce" scales it back to zero,
	// "warn" emits an event but leaves the change, "defer" accepts the change
	// and treats the workload as running.
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

// WakeSpec configures waking on request. While the workload is paused, its
// Services route to the doorman, which holds each connection, wakes the
// workload, and passes the connection through once it's Ready.
type WakeSpec struct {
	// OnRequest routes the workload's Services to the doorman while it's
	// paused. When false, requests to a paused workload fail.
	// +kubebuilder:default=true
	// +optional
	OnRequest *bool `json:"onRequest,omitempty"`

	// MaxWait is how long the doorman holds a connection while the workload
	// wakes. After it, the connection is closed; the wake carries on.
	// +kubebuilder:default="2m"
	// +kubebuilder:validation:Format=duration
	// +optional
	MaxWait *metav1.Duration `json:"maxWait,omitempty"`

	// Page answers a browser loading a page with a "waking up" page that
	// refreshes until the workload is Running, instead of holding the
	// request. Other requests are held either way.
	// +kubebuilder:default=true
	// +optional
	Page *bool `json:"page,omitempty"`
}

// DependencyRef names a workload this one needs. It refers to the
// dependency's Deployment or StatefulSet, not to its ManagedWorkload.
type DependencyRef struct {
	// Namespace of the dependency. Defaults to the ManagedWorkload's own.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	Kind TargetKind `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// WaitForReady holds this workload's resume, without scaling it, until
	// the dependency's pods are Ready. Use it when this workload fails if it
	// starts before the dependency can serve, e.g. a database replaying its
	// write-ahead log. By default dependencies wake at the same time.
	// +optional
	WaitForReady bool `json:"waitForReady,omitempty"`
}

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

// +kubebuilder:validation:Enum=pause;destroy
type IdleAction string

const (
	IdleActionPause   IdleAction = "pause"
	IdleActionDestroy IdleAction = "destroy"
)

// IdlePolicySpec configures automatic pausing. The operator tracks when the
// workload was last active and acts once it has been inactive for IdleAfter.
// Any single activity source keeps the workload awake.
type IdlePolicySpec struct {
	// Action to take once the workload has been idle for IdleAfter. "pause"
	// scales to zero; "destroy" deletes the workload.
	// +kubebuilder:default=pause
	// +optional
	Action IdleAction `json:"action,omitempty"`

	// IdleAfter is how long the workload must go without any activity
	// before the operator acts.
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:Format=duration
	// +optional
	IdleAfter *metav1.Duration `json:"idleAfter,omitempty"`

	// Activity configures what counts as activity, beyond the deploys and
	// activity annotations that always count.
	// +optional
	Activity *ActivitySpec `json:"activity,omitempty"`

	// AutoResume wakes a paused workload ahead of the demand the forecast
	// predicts.
	// +optional
	AutoResume bool `json:"autoResume,omitempty"`
}

// ActivitySpec configures the activity sources for idle detection.
type ActivitySpec struct {
	// CPUThreshold is the CPU utilization, as a percentage of the workload's
	// CPU requests, above which the workload counts as active.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	// +optional
	CPUThreshold int `json:"cpuThreshold,omitempty"`

	// Prometheus queries that measure activity, such as an ingress request
	// rate. A result above zero counts as activity; an empty result or zero
	// doesn't. Requires the operator's --prometheus-url.
	// +optional
	Prometheus []PrometheusActivity `json:"prometheus,omitempty"`
}

// PrometheusActivity is a PromQL instant query used as an activity source.
type PrometheusActivity struct {
	// PromQL is the query. Example:
	// sum(rate(nginx_ingress_controller_requests{exported_service="api"}[5m]))
	// +kubebuilder:validation:MinLength=1
	PromQL string `json:"promQL"`
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

// +kubebuilder:validation:Enum=Creating;Running;Idle;Pausing;Paused;Resuming;Destroying;Destroyed
type WorkloadPhase string

const (
	PhaseCreating   WorkloadPhase = "Creating"
	PhaseRunning    WorkloadPhase = "Running"
	PhaseIdle       WorkloadPhase = "Idle"
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

	// Destroy holds state after the workload is destroyed.
	// +optional
	Destroy *DestroyStatus `json:"destroy,omitempty"`

	// Prediction reflects the current state of the forecasting engine.
	// +optional
	Prediction *PredictionStatus `json:"prediction,omitempty"`

	// Cost holds accumulated resource cost data for the current month.
	// +optional
	Cost *CostStatus `json:"cost,omitempty"`

	// Doorman lists the doorman ports routing this workload's Services while
	// it's paused. Empty while it's awake.
	// +listType=atomic
	// +optional
	Doorman []DoormanRoute `json:"doorman,omitempty"`

	// Activity tracks when the workload was last active, for idle detection.
	// +optional
	Activity *ActivityStatus `json:"activity,omitempty"`

	// DryRun is what dry-run has measured: what Hybernate would have done
	// had it been allowed to pause the workload. Set only while spec.dryRun
	// is true.
	// +optional
	DryRun *DryRunStatus `json:"dryRun,omitempty"`

	// LastActedAt is when the operator last mutated the target workload
	// (pause, resume, destroy, or drift correction).
	// +optional
	LastActedAt *metav1.Time `json:"lastActedAt,omitempty"`

	// LastTransitionTime is when the workload last changed phases.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// DoormanRoute maps one Service port to the doorman port that stands in for
// it while the workload is paused.
type DoormanRoute struct {
	// Service is the name of the Service in the workload's namespace.
	Service string `json:"service"`

	// PortName is the Service port's name; empty for an unnamed single port.
	// +optional
	PortName string `json:"portName,omitempty"`

	// DoormanPort is the doorman's listening port for this Service port.
	DoormanPort int32 `json:"doormanPort"`
}

// +kubebuilder:validation:Enum=created;woke;request;cpu;rollout;annotation;prometheus;unobserved
type ActivitySource string

const (
	ActivitySourceCreated ActivitySource = "created"
	ActivitySourceWoke    ActivitySource = "woke"
	// ActivitySourceRequest is a wake by a request the doorman held.
	ActivitySourceRequest    ActivitySource = "request"
	ActivitySourceCPU        ActivitySource = "cpu"
	ActivitySourceRollout    ActivitySource = "rollout"
	ActivitySourceAnnotation ActivitySource = "annotation"
	ActivitySourcePrometheus ActivitySource = "prometheus"
	// ActivitySourceUnobserved restarts the clock after the operator could
	// not watch the workload, so a gap in observation never causes a pause.
	ActivitySourceUnobserved ActivitySource = "unobserved"
)

// ActivityStatus records the state of the activity clock.
type ActivityStatus struct {
	// LastActivityTime is the most recent activity from any source.
	LastActivityTime metav1.Time `json:"lastActivityTime"`

	// LastActivitySource is what produced LastActivityTime.
	LastActivitySource ActivitySource `json:"lastActivitySource"`

	// PauseAt is when the idle action will run if no further activity is
	// seen: LastActivityTime plus IdleAfter.
	// +optional
	PauseAt *metav1.Time `json:"pauseAt,omitempty"`

	// LastEvaluatedTime is when the operator last checked for activity.
	// +optional
	LastEvaluatedTime *metav1.Time `json:"lastEvaluatedTime,omitempty"`

	// TemplateHash fingerprints the target's pod template, so a deploy or
	// configuration change counts as activity while replica changes don't.
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`
}

// DryRunStatus sums up the pauses Hybernate would have made in dry-run. A
// would-be pause starts when the activity clock runs out and ends at the
// next activity, when a paused workload would have been woken.
type DryRunStatus struct {
	// Since is when dry-run started measuring the workload.
	Since metav1.Time `json:"since"`

	// Pauses is how many times the workload would have been paused,
	// including one under way.
	Pauses int32 `json:"pauses"`

	// Slept is how long the finished would-be pauses lasted in all. One
	// under way, which began at status.lastTransitionTime while the phase
	// is Idle, is added when it ends.
	Slept metav1.Duration `json:"slept"`

	// EstimatedSavings is what the replicas freed during Slept would have
	// cost, at the workload's cost rates.
	EstimatedSavings string `json:"estimatedSavings"`

	// Resources is what the workload ran when the current would-be pause
	// began, which is what pausing it would free.
	// +optional
	Resources *ResourceSnapshot `json:"resources,omitempty"`
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
	// actions (pause, destroy) this month. These savings are only
	// realized when freed resources lead to node removal by a cluster autoscaler.
	EstimatedMonthlySavings string `json:"estimatedMonthlySavings"`

	// EstimatedCostWithoutManagement is what this workload would have cost
	// without Hybernate — the sum of estimated cost and estimated savings.
	EstimatedCostWithoutManagement string `json:"estimatedCostWithoutManagement"`

	// ListRates are the on-demand list rates of the nodes the workload's
	// pods last ran on, from their instance type and region, which its cost
	// and savings are priced at unless costTracking.rates sets its own.
	// Kept while it's paused, so savings are priced at where it ran. Unset
	// when its nodes aren't in Hybernate's price table, and the default
	// rates apply.
	// +optional
	ListRates *CostRates `json:"listRates,omitempty"`

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
// actions (pause, destroy). These resources are released on the
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
