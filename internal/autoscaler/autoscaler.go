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

// Package autoscaler finds the HPA or KEDA ScaledObject that scales a
// workload, and pauses and resumes a KEDA one through KEDA itself.
package autoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// Kind is what scales a workload.
type Kind string

const (
	HPA  Kind = "HPA"
	KEDA Kind = "KEDA"
)

// PausedReplicasAnnotation tells KEDA to hold a ScaledObject's target at
// that many replicas and stop scaling it.
const PausedReplicasAnnotation = "autoscaling.keda.sh/paused-replicas"

// keda's defaults when a ScaledObject doesn't set them.
const (
	kedaMinDefault = 0
	kedaMaxDefault = 100
)

var scaledObjects = schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObjectList"}

// Autoscaler is the HPA or ScaledObject that scales a workload, and the
// range it keeps the replicas in.
type Autoscaler struct {
	Kind     Kind
	Name     string
	Min, Max int32

	// PausedReplicas is a ScaledObject's paused-replicas annotation, and nil
	// when it has none.
	PausedReplicas *string
}

// Clamp is n within the autoscaler's range.
func (a Autoscaler) Clamp(n int32) int32 {
	return min(max(n, a.Min), a.Max)
}

// kedaRecheck is how long Find remembers that KEDA isn't installed. The
// client asks the API server for all its API groups each time it looks up a
// kind it doesn't know, which on every reconcile of every workload adds up.
const kedaRecheck = 10 * time.Minute

// Finder finds what scales a workload.
type Finder struct {
	c   client.Reader
	now func() time.Time

	mu          sync.Mutex
	noKEDAUntil time.Time
}

// NewFinder returns a Finder that reads through c.
func NewFinder(c client.Reader) *Finder {
	return &Finder{c: c, now: time.Now}
}

// Find is what scales the workload, and false when nothing does, relying
// on what it last learned of whether KEDA is installed. A KEDA ScaledObject
// also creates an HPA for its target, so it's looked for first.
func (f *Finder) Find(ctx context.Context, namespace string, kind v1alpha1.TargetKind, name string) (
	Autoscaler, bool, error) {
	return f.find(ctx, namespace, kind, name, false)
}

// FindNow is Find, but looks for KEDA whatever Find learned: for a pause or
// a resume, which mustn't miss a KEDA installed since. It's rare enough
// that asking the API server again costs nothing.
func (f *Finder) FindNow(ctx context.Context, namespace string, kind v1alpha1.TargetKind, name string) (
	Autoscaler, bool, error) {
	return f.find(ctx, namespace, kind, name, true)
}

func (f *Finder) find(ctx context.Context, namespace string, kind v1alpha1.TargetKind, name string, now bool) (
	Autoscaler, bool, error) {
	so, found, err := f.findScaledObject(ctx, namespace, kind, name, now)
	if err != nil || found {
		return so, found, err
	}
	c := f.c
	var hpas autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &hpas, client.InNamespace(namespace)); err != nil {
		return Autoscaler{}, false, fmt.Errorf("listing HPAs in %s: %w", namespace, err)
	}
	for _, h := range hpas.Items {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind != string(kind) || ref.Name != name {
			continue
		}
		minReplicas := int32(1)
		if h.Spec.MinReplicas != nil {
			minReplicas = *h.Spec.MinReplicas
		}
		return Autoscaler{Kind: HPA, Name: h.Name, Min: minReplicas, Max: h.Spec.MaxReplicas}, true, nil
	}
	return Autoscaler{}, false, nil
}

func (f *Finder) findScaledObject(ctx context.Context, namespace string, kind v1alpha1.TargetKind,
	name string, now bool) (Autoscaler, bool, error) {
	f.mu.Lock()
	skip := !now && f.now().Before(f.noKEDAUntil)
	f.mu.Unlock()
	if skip {
		return Autoscaler{}, false, nil
	}
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(scaledObjects)
	if err := f.c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		if kedaMissing(err) {
			f.mu.Lock()
			f.noKEDAUntil = f.now().Add(kedaRecheck)
			f.mu.Unlock()
			return Autoscaler{}, false, nil
		}
		return Autoscaler{}, false, fmt.Errorf("listing KEDA ScaledObjects in %s: %w", namespace, err)
	}
	f.mu.Lock()
	f.noKEDAUntil = time.Time{}
	f.mu.Unlock()
	for _, so := range list.Items {
		targetName, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "name")
		targetKind, _, _ := unstructured.NestedString(so.Object, "spec", "scaleTargetRef", "kind")
		if targetKind == "" {
			targetKind = string(v1alpha1.TargetKindDeployment)
		}
		if targetName != name || targetKind != string(kind) {
			continue
		}
		a := Autoscaler{Kind: KEDA, Name: so.GetName(), Min: kedaMinDefault, Max: kedaMaxDefault}
		if v, ok, _ := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount"); ok {
			a.Min = int32(v)
		}
		if v, ok, _ := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount"); ok {
			a.Max = int32(v)
		}
		if v, ok := so.GetAnnotations()[PausedReplicasAnnotation]; ok {
			a.PausedReplicas = &v
		}
		return a, true, nil
	}
	return Autoscaler{}, false, nil
}

// HoldKEDA has KEDA hold a ScaledObject's target at replicas and stop
// scaling it, so it doesn't undo a pause, or a resume still under way.
func HoldKEDA(ctx context.Context, c client.Client, namespace, name string, replicas int32) error {
	return setPausedReplicas(ctx, c, namespace, name, strconv.Itoa(int(replicas)))
}

// ReleaseKEDA lets KEDA scale a ScaledObject's target again, putting back
// the paused-replicas annotation it had before Hybernate held it, if any.
func ReleaseKEDA(ctx context.Context, c client.Client, namespace, name string, previous *string) error {
	if previous != nil {
		return setPausedReplicas(ctx, c, namespace, name, *previous)
	}
	return setPausedReplicas(ctx, c, namespace, name, nil)
}

// setPausedReplicas sets the annotation, or removes it for a nil value. A
// ScaledObject that's gone, or KEDA uninstalled, leaves nothing to hold.
func setPausedReplicas(ctx context.Context, c client.Client, namespace, name string, value any) error {
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(scaledObjects.GroupVersion().WithKind("ScaledObject"))
	so.SetNamespace(namespace)
	so.SetName(name)
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{PausedReplicasAnnotation: value}}})
	if err != nil {
		return fmt.Errorf("building the ScaledObject patch: %w", err)
	}
	err = c.Patch(ctx, so, client.RawPatch(types.MergePatchType, body))
	if err == nil || kedaMissing(err) {
		return nil
	}
	return fmt.Errorf("annotating ScaledObject %s/%s: %w", namespace, name, err)
}

// kedaMissing reports whether err says there's no ScaledObject to be had.
// KEDA never installed gives no match for the kind; KEDA uninstalled while
// the operator runs gives NotFound instead, because the client's RESTMapper
// still maps the kind it saw, and the API server no longer serves it.
func kedaMissing(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}
