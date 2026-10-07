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

package gitops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var t0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

func entry(manager, fields string, at time.Time) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Time: &metav1.Time{Time: at},
		FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)}}
}

// scaled is a write through the scale subresource, such as kubectl scale,
// an HPA, or Hybernate's own pause, which the API server records against
// the workload's spec.replicas.
func scaled(manager string, at time.Time) metav1.ManagedFieldsEntry {
	e := entry(manager, replicas, at)
	e.Operation = metav1.ManagedFieldsOperationUpdate
	e.Subresource = "scale"
	return e
}

const (
	replicas = `{"f:spec":{"f:replicas":{}}}`
	template = `{"f:spec":{"f:template":{}}}`
)

func TestReplicasWriter(t *testing.T) {
	tests := []struct {
		name   string
		fields []metav1.ManagedFieldsEntry
		want   Writer
		ok     bool
	}{
		{name: "Argo CD", fields: []metav1.ManagedFieldsEntry{entry("argocd-controller", replicas, t0)},
			want: Writer{Manager: "argocd-controller", Tool: ArgoCD, At: t0}, ok: true},
		{name: "older Argo CD", fields: []metav1.ManagedFieldsEntry{entry("argocd-application-controller", replicas, t0)},
			want: Writer{Manager: "argocd-application-controller", Tool: ArgoCD, At: t0}, ok: true},
		{name: "Flux kustomize", fields: []metav1.ManagedFieldsEntry{entry("kustomize-controller", replicas, t0)},
			want: Writer{Manager: "kustomize-controller", Tool: Flux, At: t0}, ok: true},
		{name: "Flux Helm", fields: []metav1.ManagedFieldsEntry{entry("helm-controller", replicas, t0)},
			want: Writer{Manager: "helm-controller", Tool: Flux, At: t0}, ok: true},
		{name: "a person", fields: []metav1.ManagedFieldsEntry{entry("kubectl-scale", replicas, t0)},
			want: Writer{Manager: "kubectl-scale", At: t0}, ok: true},
		{name: "the latest of several", fields: []metav1.ManagedFieldsEntry{
			entry("argocd-controller", replicas, t0), entry("hybernate", replicas, t0.Add(time.Hour)),
			entry("kubectl-scale", replicas, t0.Add(time.Minute))},
			want: Writer{Manager: "hybernate", At: t0.Add(time.Hour)}, ok: true},
		{name: "others' fields don't count", fields: []metav1.ManagedFieldsEntry{
			entry("argocd-controller", template, t0.Add(time.Hour)), entry("kubectl-scale", replicas, t0)},
			want: Writer{Manager: "kubectl-scale", At: t0}, ok: true},
		{name: "a tie goes to the GitOps tool", fields: []metav1.ManagedFieldsEntry{
			entry("hybernate", replicas, t0), entry("argocd-controller", replicas, t0)},
			want: Writer{Manager: "argocd-controller", Tool: ArgoCD, At: t0}, ok: true},
		{name: "a tie goes to the GitOps tool whichever comes first", fields: []metav1.ManagedFieldsEntry{
			entry("kustomize-controller", replicas, t0), entry("kubectl-scale", replicas, t0)},
			want: Writer{Manager: "kustomize-controller", Tool: Flux, At: t0}, ok: true},
		{name: "a tie between people goes to the first", fields: []metav1.ManagedFieldsEntry{
			entry("kubectl-scale", replicas, t0), entry("kubectl-edit", replicas, t0)},
			want: Writer{Manager: "kubectl-scale", At: t0}, ok: true},
		{name: "through the scale subresource", fields: []metav1.ManagedFieldsEntry{
			entry("argocd-controller", replicas, t0), scaled("kube-controller-manager", t0.Add(time.Minute))},
			want: Writer{Manager: "kube-controller-manager", At: t0.Add(time.Minute)}, ok: true},
		{name: "a GitOps tool after a scale", fields: []metav1.ManagedFieldsEntry{
			scaled("hybernate", t0), entry("argocd-controller", replicas, t0.Add(time.Minute))},
			want: Writer{Manager: "argocd-controller", Tool: ArgoCD, At: t0.Add(time.Minute)}, ok: true},
		{name: "nobody", fields: []metav1.ManagedFieldsEntry{entry("argocd-controller", template, t0)}},
		{name: "no fields", fields: []metav1.ManagedFieldsEntry{{Manager: "argocd-controller"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ReplicasWriter(tt.fields)

			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want.Tool != "", got.FromGit())
		})
	}
}

func TestFix(t *testing.T) {
	assert.Contains(t, Fix(ArgoCD), "    jsonPointers: [/spec/replicas]")
	assert.NotEmpty(t, Fix(Flux))
	assert.Empty(t, Fix(""))
}
