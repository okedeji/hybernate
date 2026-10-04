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

// Package gitops tells who sets a workload's replica count, from the field
// managers the API server records, and how to have a GitOps tool leave it
// to Hybernate.
package gitops

import (
	"encoding/json"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tool is a GitOps tool that applies workloads from Git.
type Tool string

const (
	ArgoCD Tool = "Argo CD"
	Flux   Tool = "Flux"
)

// Writer is the field manager that last set a workload's replicas.
type Writer struct {
	// Manager is the field manager's name, such as kubectl-scale.
	Manager string
	// Tool is the GitOps tool the manager belongs to, if any.
	Tool Tool
	At   time.Time
}

// FromGit says the replicas come from Git, so the tool will set them again.
func (w Writer) FromGit() bool {
	return w.Tool != ""
}

// ReplicasWriter is whoever last set spec.replicas, and false when no field
// manager records it, such as on an object created before managed fields.
func ReplicasWriter(managedFields []metav1.ManagedFieldsEntry) (Writer, bool) {
	var found Writer
	ok := false
	for _, entry := range managedFields {
		if entry.FieldsV1 == nil || !setsReplicas(entry.FieldsV1.Raw) {
			continue
		}
		var at time.Time
		if entry.Time != nil {
			at = entry.Time.Time
		}
		if ok && !at.After(found.At) {
			continue
		}
		found, ok = Writer{Manager: entry.Manager, Tool: toolOf(entry.Manager), At: at}, true
	}
	return found, ok
}

func setsReplicas(raw []byte) bool {
	var fields struct {
		Spec map[string]json.RawMessage `json:"f:spec"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, ok := fields.Spec["f:replicas"]
	return ok
}

// toolOf names the GitOps tool behind a field manager. Argo CD applies as
// argocd-controller, older releases as argocd-application-controller; Flux
// as kustomize-controller or helm-controller.
func toolOf(manager string) Tool {
	switch {
	case strings.HasPrefix(manager, "argocd"):
		return ArgoCD
	case manager == "kustomize-controller", manager == "helm-controller":
		return Flux
	}
	return ""
}

// Fix is what to change so the tool stops setting replicas Hybernate has
// paused, one line per step.
func Fix(t Tool) []string {
	switch t {
	case ArgoCD:
		return []string{
			"Have Argo CD ignore replicas, as for an HPA, once for every app, in the argocd-cm ConfigMap:",
			"  resource.customizations.ignoreDifferences.apps_Deployment: |",
			"    jsonPointers: [/spec/replicas]",
			"  resource.customizations.ignoreDifferences.apps_StatefulSet: |",
			"    jsonPointers: [/spec/replicas]",
			"and add RespectIgnoreDifferences=true to each app's syncPolicy.syncOptions, so syncs leave them too.",
		}
	case Flux:
		return []string{
			"Leave replicas out of the workload's manifest in Git, so Flux doesn't set them;",
			"for a Helm chart that always sets them, remove them with a postRenderers kustomize patch in the HelmRelease.",
		}
	}
	return nil
}
