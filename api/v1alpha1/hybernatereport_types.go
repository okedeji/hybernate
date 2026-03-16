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
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Managed",type=integer,JSONPath=`.status.totalManagedWorkloads`
// +kubebuilder:printcolumn:name="Cost",type=string,JSONPath=`.status.estimatedMonthlyCost`
// +kubebuilder:printcolumn:name="Savings",type=string,JSONPath=`.status.totalMonthlySavings`

// HybernateReport is a cluster-scoped singleton that aggregates cost and
// lifecycle data across all ManagedWorkloads.
type HybernateReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +optional
	Status HybernateReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HybernateReportList contains a list of HybernateReport.
type HybernateReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HybernateReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HybernateReport{}, &HybernateReportList{})
}

// HybernateReportStatus aggregates cost and workload data across the cluster.
type HybernateReportStatus struct {
	TotalManagedWorkloads int `json:"totalManagedWorkloads"`
	ActiveWorkloads       int `json:"activeWorkloads"`
	PausedWorkloads       int `json:"pausedWorkloads"`
	DestroyedWorkloads    int `json:"destroyedWorkloads"`

	TotalCPUHours    resource.Quantity `json:"totalCPUHours"`
	TotalMemoryHours resource.Quantity `json:"totalMemoryHours"`
	TotalStorageHours resource.Quantity `json:"totalStorageHours"`

	EstimatedMonthlyCost  string `json:"estimatedMonthlyCost"`
	TotalMonthlySavings   string `json:"totalMonthlySavings"`
	CostWithoutManagement string `json:"costWithoutManagement"`

	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}
