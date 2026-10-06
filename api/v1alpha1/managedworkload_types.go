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
// +kubebuilder:printcolumn:name="Saved",type=string,JSONPath=`.status.cost.savedThisMonth`,description="Saved by pausing this month (UTC)"
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

// +kubebuilder:validation:Enum=Running;Paused
type DesiredState string

const (
	DesiredStateRunning DesiredState = "Running"
	DesiredStatePaused  DesiredState = "Paused"
)

// ManagedWorkloadSpec defines the desired lifecycle behavior for a workload.
type ManagedWorkloadSpec struct {
	// Target identifies the workload to manage (e.g. a Deployment or
	// StatefulSet). It can't be changed, since a paused target would be left
	// at zero: create another ManagedWorkload to manage another workload.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="target is immutable; create another ManagedWorkload to manage another workload"
	Target WorkloadRef `json:"target"`

	// DesiredState overrides automation and forces the workload into the given
	// state. When set, the operator stops evaluating the idle policy and
	// drives the workload to this state instead.
	// +optional
	DesiredState *DesiredState `json:"desiredState,omitempty"`

	// IdlePolicy configures automatic pausing: the operator pauses the
	// workload once it has had no activity for IdlePolicy.IdleAfter.
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

	// Prediction configures the Holt-Winters forecasting engine that confirms
	// idle detection and wakes paused workloads ahead of predicted demand.
	// +required
	Prediction PredictionSpec `json:"prediction"`

	// CostTracking configures custom cost rates for this workload.
	// Cost tracking is always enabled with AWS on-demand defaults.
	// Set this field only to override pricing rates.
	// +optional
	CostTracking *CostTrackingSpec `json:"costTracking,omitempty"`

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
	// Confidence is the accuracy percentage (50-100) a season's forecasts
	// must reach before they drive decisions rather than only being
	// reported. Accuracy is 1 - WAPE: 85 means the forecast's total error
	// over the window is 15% of the demand in it. A season that falls 5
	// points below it stops driving decisions until it earns it again. The
	// minimum is 50 because below that a forecast that is wrong more than it
	// is right would wake workloads and hold off pauses.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=85
	Confidence int `json:"confidence"`
}

// IdlePolicySpec configures automatic pausing. The operator tracks when the
// workload was last active and pauses it once it has been inactive for
// IdleAfter. Any single activity source keeps the workload awake.
type IdlePolicySpec struct {
	// IdleAfter is how long the workload must go without any activity
	// before the operator pauses it.
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
	// +kubebuilder:validation:Minimum=1
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

// +kubebuilder:validation:Enum=Creating;Running;Idle;Pausing;Paused;Resuming
type WorkloadPhase string

const (
	PhaseCreating WorkloadPhase = "Creating"
	PhaseRunning  WorkloadPhase = "Running"
	PhaseIdle     WorkloadPhase = "Idle"
	PhasePausing  WorkloadPhase = "Pausing"
	PhasePaused   WorkloadPhase = "Paused"
	PhaseResuming WorkloadPhase = "Resuming"
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

	// Pause records what pausing changed, from before the workload is scaled
	// to zero until it's running again.
	// +optional
	Pause *PauseStatus `json:"pause,omitempty"`

	// LearnedDependencies are the workloads Hybernate found this one depends
	// on without a dependsOn: from addresses in its environment that name
	// their Services, and from requests it sent that woke them. They're held
	// awake and woken like dependsOn, less the ones the
	// hybernate.io/ignore-dependencies annotation names.
	// +optional
	LearnedDependencies *LearnedDependencies `json:"learnedDependencies,omitempty"`

	// LastScaledUp is the last time something other than Hybernate scaled
	// the workload up while it was paused, which wakes it.
	// +optional
	LastScaledUp *ScaledUp `json:"lastScaledUp,omitempty"`

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

	// LastPauseRequest is the last hybernate.io/pause-requested value
	// Hybernate acted on, so each request is acted on once, across restarts.
	// The PauseRequest condition says what came of it.
	// +optional
	LastPauseRequest string `json:"lastPauseRequest,omitempty"`

	// LastActedAt is when the operator last mutated the target workload
	// (pause or resume).
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

// +kubebuilder:validation:Enum=created;woke;request;cpu;rollout;annotation;prometheus;unobserved;scaled-up
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
	// ActivitySourceScaledUp is a paused workload scaled up by something
	// other than Hybernate, such as a person or a GitOps tool.
	ActivitySourceScaledUp ActivitySource = "scaled-up"
)

// LearnedDependencies are dependencies Hybernate found, and what from.
type LearnedDependencies struct {
	// From identifies what they were learned from, the pod template and the
	// ignore annotation, so they're learned again when either changes.
	From string `json:"from"`

	// At is when they were last learned. They're also learned again
	// hourly, for ConfigMaps changed since.
	At metav1.Time `json:"at"`

	// +listType=atomic
	// +optional
	Dependencies []LearnedDependency `json:"dependencies,omitempty"`
}

// LearnedDependency is one workload this one was found to depend on.
type LearnedDependency struct {
	Namespace string     `json:"namespace"`
	Kind      TargetKind `json:"kind"`
	Name      string     `json:"name"`

	// Source is how it was found: in the workload's environment, or from a
	// request it sent that woke the dependency.
	Source LearnedSource `json:"source"`

	// Via is the environment variable that holds its address, and Address
	// the address, with any password hidden, for one found in the
	// environment.
	// +optional
	Via string `json:"via,omitempty"`
	// +optional
	Address string `json:"address,omitempty"`
}

// +kubebuilder:validation:Enum=environment;wake
type LearnedSource string

const (
	LearnedFromEnvironment LearnedSource = "environment"
	LearnedFromWake        LearnedSource = "wake"
)

// ScaledUp is a paused workload scaled up outside Hybernate.
type ScaledUp struct {
	At metav1.Time `json:"at"`
	// By is the field manager that set the replicas, such as kubectl-scale
	// or argocd-controller.
	By string `json:"by"`
	// GitOps is the GitOps tool behind By, such as Argo CD or Flux, which
	// will set the replicas again each time Hybernate pauses the workload
	// until it's told to leave them.
	// +optional
	GitOps   string `json:"gitOps,omitempty"`
	Replicas int32  `json:"replicas"`
}

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

	// FreedCPUHours and FreedMemoryHours are the vCPU-hours and GiB-hours
	// the replicas requested during Slept: what the would-be pauses would
	// have freed. They're kept to a billionth of an hour, so short pauses
	// of small workloads add up.
	FreedCPUHours    resource.Quantity `json:"freedCPUHours"`
	FreedMemoryHours resource.Quantity `json:"freedMemoryHours"`

	// EstimatedSavings is the freed hours priced at the workload's cost
	// rates, to the cent. It's for display: the hours are the record.
	EstimatedSavings string `json:"estimatedSavings"`

	// Resources is what the workload ran when the current would-be pause
	// began, which is what pausing it would free.
	// +optional
	Resources *ResourceSnapshot `json:"resources,omitempty"`
}

// ResourceSnapshot is what a workload runs, read at one moment, so it can be
// priced later without reading the target again.
type ResourceSnapshot struct {
	// Replicas is the replica count at the time of the snapshot.
	Replicas int32 `json:"replicas"`

	// CPUMillis and MemoryBytes are what one replica's pod requests,
	// sidecars injected when it was created included.
	CPUMillis   int64 `json:"cpuMillis"`
	MemoryBytes int64 `json:"memoryBytes"`

	// StorageBytes is the capacity of the PersistentVolumeClaims the
	// workload's pods mount, in all.
	StorageBytes int64 `json:"storageBytes"`
}

// PauseStatus records what a pause changed, so it can be undone exactly. It's
// written before the workload is scaled to zero, so a pause interrupted at
// any point is finished or undone from it.
type PauseStatus struct {
	// PreviousReplicas is the replica count before pausing, used to
	// restore on resume.
	PreviousReplicas int32 `json:"previousReplicas"`

	// PausedAt is when the workload was scaled to zero. Unset while the
	// pause is under way.
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`

	// ScaledObject is the KEDA ScaledObject held at zero while the workload
	// is paused, so KEDA doesn't scale it up. Resuming releases it.
	// +optional
	ScaledObject string `json:"scaledObject,omitempty"`

	// ScaledObjectPausedReplicas is the ScaledObject's own
	// autoscaling.keda.sh/paused-replicas annotation from before the pause,
	// put back when it's released. Unset when it had none.
	// +optional
	ScaledObjectPausedReplicas *string `json:"scaledObjectPausedReplicas,omitempty"`

	// Resources captures the workload's resource profile at pause time
	// for cost savings calculation.
	// +optional
	Resources *ResourceSnapshot `json:"resources,omitempty"`

	// WakeAnnotations are the activity annotations as the pause began. One
	// that has changed since wakes the workload whatever time it states, so
	// a clock behind the operator's can't lose a wake.
	// +optional
	WakeAnnotations *WakeAnnotations `json:"wakeAnnotations,omitempty"`
}

// WakeAnnotations are the hybernate.io/last-activity, last-request and
// active-until annotations, by name, on the ManagedWorkload and its target.
type WakeAnnotations struct {
	// +optional
	Workload map[string]string `json:"workload,omitempty"`
	// +optional
	Target map[string]string `json:"target,omitempty"`
}

// PredictionStatus reflects the current state of the Holt-Winters engine's
// dual-season lifecycle.
type PredictionStatus struct {
	// DailyPhase is the daily season's lifecycle phase
	// (Observing, Suggesting, or Active).
	DailyPhase string `json:"dailyPhase"`

	// DailyConfidence is the forecast's accuracy over the last 24 observed
	// hours, as a percentage.
	DailyConfidence int `json:"dailyConfidence"`

	// WeeklyPhase is the weekly season's lifecycle phase
	// (Observing, Suggesting, or Active).
	WeeklyPhase string `json:"weeklyPhase"`

	// WeeklyConfidence is the forecast's accuracy over the last 168
	// observed hours, a whole week of weekdays and weekend, as a percentage.
	WeeklyConfidence int `json:"weeklyConfidence"`

	// State is what the forecasting engine has learned, compressed and
	// encoded, so that it survives an operator restart. It is written with
	// each hourly observation. State that can't be read is discarded, with a
	// warning event, and the engine starts learning again.
	// +optional
	State string `json:"state,omitempty"`
}

// CostStatus is what the workload has cost, and what pausing it has saved,
// this calendar month in UTC.
//
// Everything is priced on what the workload's pods request, sidecars
// injected when they were created included, rather than on what they use:
// requests are what a pod reserves on a node, so they're what the workload
// costs in capacity, and what a cluster autoscaler can remove once it's
// paused.
//
// The resource-hours are the record, kept to a billionth of an hour. The
// dollar figures are derived from them at the workload's current rates
// whenever they're brought up to date, and are rounded to the cent for
// display, so CostThisMonth plus SavedThisMonth is
// CostWithoutHybernateThisMonth to within a cent.
type CostStatus struct {
	// Tracked is how much of the month the totals cover. It's less than
	// the time since the month began when the workload was created during
	// the month, or when the operator wasn't running.
	Tracked metav1.Duration `json:"tracked"`

	// LastAccumulatedAt is when the totals were last brought up to date:
	// at every phase change, so time counts in the phase it was spent in,
	// and every few minutes in between. A longer gap, such as while the
	// operator wasn't running, counts as two hours at most, since what the
	// workload did meanwhile isn't known.
	// +optional
	LastAccumulatedAt *metav1.Time `json:"lastAccumulatedAt,omitempty"`

	// AwakeCPUHours and AwakeMemoryHours are the vCPU-hours and GiB-hours
	// the workload's replicas requested while it was awake, in any phase
	// but Paused.
	AwakeCPUHours    resource.Quantity `json:"awakeCPUHours"`
	AwakeMemoryHours resource.Quantity `json:"awakeMemoryHours"`

	// StorageHours is the GiB-hours its claims provisioned, awake or
	// paused: a pause doesn't free storage.
	StorageHours resource.Quantity `json:"storageHours"`

	// PausedCPUHours and PausedMemoryHours are the vCPU-hours and GiB-hours
	// the replicas Hybernate paused had requested, for as long as they were
	// paused: what pausing freed.
	PausedCPUHours    resource.Quantity `json:"pausedCPUHours"`
	PausedMemoryHours resource.Quantity `json:"pausedMemoryHours"`

	// CostThisMonth is the awake hours and the storage hours, priced.
	CostThisMonth string `json:"costThisMonth"`

	// SavedThisMonth is the paused hours, priced. It becomes money only
	// once a cluster autoscaler removes the capacity a pause frees.
	SavedThisMonth string `json:"savedThisMonth"`

	// CostWithoutHybernateThisMonth is what the workload would have cost
	// this month had it never been paused: CostThisMonth plus
	// SavedThisMonth.
	CostWithoutHybernateThisMonth string `json:"costWithoutHybernateThisMonth"`

	// ProjectedMonthlyCost and ProjectedMonthlySavings are CostThisMonth
	// and SavedThisMonth carried from Tracked to the whole month at the
	// same rate. They're "pending" until a day has been tracked, since a
	// workload's pattern is daily.
	ProjectedMonthlyCost    string `json:"projectedMonthlyCost"`
	ProjectedMonthlySavings string `json:"projectedMonthlySavings"`

	// Running is what the workload ran when the totals were last brought
	// up to date while it was awake, which its awake time is priced on.
	// Its replicas and storage are read each time, and what a replica
	// requests at PricedAt.
	// +optional
	Running *ResourceSnapshot `json:"running,omitempty"`

	// PricedAt is when what a replica requests and ListRates were last
	// read, from the workload's pods and their nodes. They're read again
	// hourly while it's awake, and when its pod template changes.
	// +optional
	PricedAt *metav1.Time `json:"pricedAt,omitempty"`

	// PricedTemplateHash is the pod template's hash at PricedAt.
	// +optional
	PricedTemplateHash string `json:"pricedTemplateHash,omitempty"`

	// ListRates are the on-demand list rates of the nodes the workload's
	// pods last ran on, from their instance type and region, which its cost
	// and savings are priced at unless costTracking.rates sets its own.
	// Kept while it's paused, so savings are priced at where it ran. Unset
	// when its nodes aren't in Hybernate's price table, and the default
	// rates apply.
	// +optional
	ListRates *CostRates `json:"listRates,omitempty"`

	// ResourceReduction is what the current pause freed. Unlike the dollar
	// figures, it doesn't depend on a cluster autoscaler removing nodes.
	// +optional
	ResourceReduction *ResourceReduction `json:"resourceReduction,omitempty"`
}

// ResourceReduction tracks the workload-level resources freed by Hybernate
// pauses. These resources are released on the
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
