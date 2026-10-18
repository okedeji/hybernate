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
	Fleet  Tool = "Fleet"
	Kapp   Tool = "kapp"
	Kluctl Tool = "Kluctl"
)

// Writer is the field manager that last set a field, such as a workload's
// replicas.
type Writer struct {
	// Manager is the field manager's name, such as argocd-controller.
	Manager string
	// Tool is the GitOps tool the manager belongs to, if any.
	Tool Tool
	At   time.Time
}

// FromGit says the field comes from Git, so the tool will set it again.
func (w Writer) FromGit() bool {
	return w.Tool != ""
}

// ReplicasWriter is whoever last set spec.replicas, and false when no field
// manager records it. A write through the scale subresource, as kubectl
// scale, an HPA and KEDA make, takes the field from whoever owned it without
// recording a new owner, so a scale-up that way has no writer. A GitOps tool
// writes the workload itself, so it's always recorded.
func ReplicasWriter(managedFields []metav1.ManagedFieldsEntry) (Writer, bool) {
	return FieldWriter(managedFields, "spec", "replicas")
}

// FieldWriter is whoever last set the field at path, given as its field
// names, such as "metadata", "annotations", "hybernate.io/dry-run", and
// false when no field manager records it.
//
// Managed-field times have one-second precision, so two managers can tie.
// A GitOps tool wins a tie: it sets the field again on every sync, and
// missing that would have Hybernate keep pausing what the tool keeps waking.
func FieldWriter(managedFields []metav1.ManagedFieldsEntry, path ...string) (Writer, bool) {
	var found Writer
	ok := false
	for _, entry := range managedFields {
		if !sets(entry.FieldsV1.GetRawBytes(), path) {
			continue
		}
		w := Writer{Manager: entry.Manager, Tool: ToolOf(entry.Manager)}
		if entry.Time != nil {
			w.At = entry.Time.Time
		}
		if ok && (w.At.Before(found.At) || w.At.Equal(found.At) && (found.FromGit() || !w.FromGit())) {
			continue
		}
		found, ok = w, true
	}
	return found, ok
}

// sets says a managed fields entry, in the FieldsV1 format, owns the field
// at path.
func sets(raw []byte, path []string) bool {
	for _, name := range path {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return false
		}
		var ok bool
		if raw, ok = fields["f:"+name]; !ok {
			return false
		}
	}
	return len(path) > 0
}

// ToolOf names the GitOps tool behind a field manager. Argo CD applies as
// argocd-controller, client-side and server-side, older releases as
// argocd-application-controller; Flux as kustomize-controller or
// helm-controller; Rancher Fleet as fleetagent, its agent's binary; kapp,
// and kapp-controller through it, as kapp; Kluctl as kluctl or kluctl-*.
func ToolOf(manager string) Tool {
	switch {
	case strings.HasPrefix(manager, "argocd"):
		return ArgoCD
	case manager == "kustomize-controller", manager == "helm-controller":
		return Flux
	case manager == "fleetagent", manager == "fleet-agent":
		return Fleet
	case manager == "kapp":
		return Kapp
	case manager == "kluctl", strings.HasPrefix(manager, "kluctl-"):
		return Kluctl
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
	case Fleet:
		return []string{
			"Leave replicas out of the workload's manifest in Git, so Fleet doesn't set them;",
			"for a Helm chart that always sets them, remove them with a kustomize patch in the bundle's fleet.yaml.",
		}
	case Kapp:
		return []string{
			"Have kapp keep the replicas it finds, as for an HPA, with a rebase rule in a kapp Config:",
			"  rebaseRules:",
			"  - path: [spec, replicas]",
			"    type: copy",
			"    sources: [existing, new]",
			"    resourceMatchers:",
			"    - apiVersionKindMatcher: {apiVersion: apps/v1, kind: Deployment}",
			"    - apiVersionKindMatcher: {apiVersion: apps/v1, kind: StatefulSet}",
		}
	case Kluctl:
		return []string{
			"Leave replicas out of the workload's manifest in Git, so Kluctl doesn't set them;",
			"for a Helm chart that always sets them, remove them with a patch in the deployment item's kustomization.yaml.",
		}
	}
	return nil
}
