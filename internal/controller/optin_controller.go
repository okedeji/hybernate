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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

const (
	actionOptIn = "OptIn"

	ReasonManaged         = "Managed"
	ReasonNoLongerManaged = "NoLongerManaged"
	ReasonInvalidSetting  = "InvalidSetting"
	ReasonLabelIgnored    = "LabelIgnored"

	// optInTimeout bounds one opt-in reconcile, which reads from the cache
	// and makes at most one write.
	optInTimeout = 30 * time.Second
)

// OptInReconciler gives each Deployment or StatefulSet opted in with the
// managed label, on itself or its namespace, a ManagedWorkload whose spec
// follows the opt-in annotations, and removes it when the workload opts out.
type OptInReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Kind     v1alpha1.TargetKind
	Defaults OptInDefaults
	// ProtectedNamespaces are name patterns of namespaces whose workloads
	// aren't opted in, whatever their labels, unless the namespace is
	// labelled to allow it.
	ProtectedNamespaces []string
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=hybernate.io,resources=managedworkloads,verbs=get;list;watch;create;update;patch;delete

// Reconcile makes the workload's label-created ManagedWorkload match its
// labels and annotations. A ManagedWorkload someone wrote for the workload
// always wins, and the label then does nothing.
func (r *OptInReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, optInTimeout)
	defer cancel()

	obj := r.newWorkload()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		// A deleted workload's ManagedWorkload goes with it, through its
		// owner reference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, &ns); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting namespace %s: %w", req.Namespace, err)
	}

	target := v1alpha1.WorkloadRef{Kind: r.Kind, Name: obj.GetName()}
	existing, err := r.managedWorkloadsFor(ctx, req.Namespace, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if existing.written != nil || !r.optedIn(obj, &ns) {
		return ctrl.Result{}, r.release(ctx, obj, existing.ours, existing.written)
	}

	spec, problems := optInSpec(target, obj.GetAnnotations(), ns.GetAnnotations(), r.Defaults)
	for _, p := range problems {
		r.Recorder.Eventf(obj, nil, "Warning", ReasonInvalidSetting, actionOptIn, "%s", p)
	}
	if existing.ours != nil {
		return ctrl.Result{}, r.update(ctx, obj, existing.ours, spec)
	}
	if existing.leaving {
		// Its old ManagedWorkload is still restoring it on the way out.
		// That one's deletion requeues this, and the new one is made then.
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.create(ctx, obj, existing.names, spec)
}

func (r *OptInReconciler) newWorkload() client.Object {
	if r.Kind == v1alpha1.TargetKindStatefulSet {
		return &appsv1.StatefulSet{}
	}
	return &appsv1.Deployment{}
}

// optedIn reports whether the workload is managed through the label: its
// own, or its namespace's, unless it's marked to be ignored or its
// namespace is protected. A label value
// other than "true" doesn't opt in, and is pointed out, since it's most
// likely a setting meant for an annotation.
func (r *OptInReconciler) optedIn(obj client.Object, ns *corev1.Namespace) bool {
	if obj.GetLabels()[v1alpha1.LabelIgnore] == v1alpha1.True {
		return false
	}
	if v1alpha1.Protected(ns.Name, ns.Labels, r.ProtectedNamespaces) {
		if obj.GetLabels()[v1alpha1.LabelManaged] == v1alpha1.True || ns.Labels[v1alpha1.LabelManaged] == v1alpha1.True {
			r.Recorder.Eventf(obj, nil, "Warning", ReasonProtected, actionOptIn,
				"namespace %s is protected, so the %s label doesn't opt the workload in", ns.Name, v1alpha1.LabelManaged)
		}
		return false
	}
	if v, ok := obj.GetLabels()[v1alpha1.LabelManaged]; ok {
		if v == v1alpha1.True {
			return true
		}
		r.Recorder.Eventf(obj, nil, "Warning", ReasonLabelIgnored, actionOptIn,
			`%s=%q isn't "true", so it doesn't opt the workload in; settings such as dry-run are annotations`,
			v1alpha1.LabelManaged, v)
		return false
	}
	return ns.GetLabels()[v1alpha1.LabelManaged] == v1alpha1.True
}

// existingWorkloads is what the namespace's ManagedWorkloads mean for one
// target.
type existingWorkloads struct {
	// ours is the target's label-created ManagedWorkload.
	ours *v1alpha1.ManagedWorkload
	// written is one someone wrote for the target.
	written *v1alpha1.ManagedWorkload
	// leaving says the target's label-created ManagedWorkload is being
	// deleted.
	leaving bool
	// names are taken by a ManagedWorkload, for any target.
	names map[string]bool
}

// managedWorkloadsFor finds the target's ManagedWorkloads by the target
// they manage, never by name: a Deployment and a StatefulSet can share a
// name, and so can't both have a ManagedWorkload named after them.
func (r *OptInReconciler) managedWorkloadsFor(ctx context.Context, namespace string, target v1alpha1.WorkloadRef) (
	existingWorkloads, error) {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return existingWorkloads{}, fmt.Errorf("listing managed workloads: %w", err)
	}
	found := existingWorkloads{names: make(map[string]bool, len(list.Items))}
	for i := range list.Items {
		mw := &list.Items[i]
		found.names[mw.Name] = true
		if mw.Spec.Target != target {
			continue
		}
		fromLabel := mw.Labels[v1alpha1.LabelFromLabel] == v1alpha1.True
		switch {
		case !mw.DeletionTimestamp.IsZero():
			found.leaving = found.leaving || fromLabel
		case fromLabel:
			found.ours = mw
		default:
			found.written = mw
		}
	}
	return found, nil
}

// release removes the label-created ManagedWorkload. Its deletion wakes the
// workload first if it's paused.
func (r *OptInReconciler) release(ctx context.Context, obj client.Object, ours, written *v1alpha1.ManagedWorkload) error {
	if ours == nil {
		return nil
	}
	if err := r.Delete(ctx, ours); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting managed workload %s: %w", ours.Name, err)
	}
	why := "the managed label was removed"
	if written != nil {
		why = fmt.Sprintf("ManagedWorkload %s manages it", written.Name)
	}
	r.Recorder.Eventf(obj, nil, "Normal", ReasonNoLongerManaged, actionOptIn, "no longer managed from its label: %s", why)
	return nil
}

// update brings the label-created ManagedWorkload in line with the
// annotations, keeping what they don't control.
func (r *OptInReconciler) update(ctx context.Context, obj client.Object, ours *v1alpha1.ManagedWorkload,
	spec v1alpha1.ManagedWorkloadSpec) error {
	mw := ours.DeepCopy()
	keepUserSettings(&spec, &ours.Spec)
	mw.Spec = spec
	// An earlier version could also make a Deployment and a StatefulSet
	// sharing a name both owners, so deleting its own target left it behind.
	mw.OwnerReferences = slices.DeleteFunc(mw.OwnerReferences, func(ref metav1.OwnerReference) bool {
		return ref.APIVersion == appsv1.SchemeGroupVersion.String() && ref.UID != obj.GetUID() &&
			(ref.Kind == string(v1alpha1.TargetKindDeployment) || ref.Kind == string(v1alpha1.TargetKindStatefulSet))
	})
	if err := controllerutil.SetOwnerReference(obj, mw, r.Scheme); err != nil {
		return fmt.Errorf("owning managed workload %s: %w", mw.Name, err)
	}
	if equality.Semantic.DeepEqual(ours.Spec, mw.Spec) && equality.Semantic.DeepEqual(ours.OwnerReferences, mw.OwnerReferences) {
		return nil
	}
	if err := r.Update(ctx, mw); err != nil {
		return fmt.Errorf("updating managed workload %s: %w", mw.Name, err)
	}
	return nil
}

// create makes the workload's ManagedWorkload, named after it, or after it
// and its kind when the name is taken.
func (r *OptInReconciler) create(ctx context.Context, obj client.Object, taken map[string]bool,
	spec v1alpha1.ManagedWorkloadSpec) error {
	plain, qualified := obj.GetName(), kindQualifiedName(obj.GetName(), r.Kind)
	name := plain
	if taken[name] {
		name = qualified
	}
	if taken[name] {
		r.Recorder.Eventf(obj, nil, "Warning", ReasonLabelIgnored, actionOptIn,
			"can't manage it from its label: ManagedWorkloads named %s and %s already exist for other workloads",
			plain, qualified)
		return nil
	}

	mw := &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: obj.GetNamespace(),
			// Only this label is set: copying the workload's labels would
			// carry a GitOps tool's tracking label, and the tool would claim
			// and prune the ManagedWorkload as its own.
			Labels: map[string]string{v1alpha1.LabelFromLabel: v1alpha1.True},
		},
		Spec: spec,
	}
	if err := controllerutil.SetOwnerReference(obj, mw, r.Scheme); err != nil {
		return fmt.Errorf("owning managed workload %s: %w", mw.Name, err)
	}
	// A cache that hasn't seen a ManagedWorkload yet makes this fail with
	// AlreadyExists; the retry sees it.
	if err := r.Create(ctx, mw); err != nil {
		return fmt.Errorf("creating managed workload %s: %w", mw.Name, err)
	}
	r.Recorder.Eventf(obj, nil, "Normal", ReasonManaged, actionOptIn, "managed by Hybernate as ManagedWorkload %s", mw.Name)
	log.FromContext(ctx).Info("managing workload from its label", "workload", obj.GetName(),
		"namespace", obj.GetNamespace(), "managed_workload", mw.Name)
	return nil
}

// kindQualifiedName is name-kind, such as api-statefulset. A name too long
// for the suffix is shortened, with a hash of it kept so two long names
// sharing a prefix still differ.
func kindQualifiedName(name string, kind v1alpha1.TargetKind) string {
	suffix := "-" + strings.ToLower(string(kind))
	if len(name)+len(suffix) <= validation.DNS1123SubdomainMaxLength {
		return name + suffix
	}
	sum := sha256.Sum256([]byte(name))
	suffix = "-" + hex.EncodeToString(sum[:4]) + suffix
	// A DNS subdomain's dot-separated parts must each end alphanumeric.
	return strings.TrimRight(name[:validation.DNS1123SubdomainMaxLength-len(suffix)], "-.") + suffix
}

// SetupWithManager reconciles a workload when its labels or annotations
// change, when its namespace's do, and when a ManagedWorkload for it is
// created, deleted, or has its spec or labels changed. Replica changes,
// which pausing makes, and status writes don't trigger it.
func (r *OptInReconciler) SetupWithManager(mgr ctrl.Manager) error {
	metadataChanged := predicate.Or(predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{})
	specOrLabelsChanged := predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).
		Named("optin-"+string(r.Kind)).
		For(r.newWorkload(), builder.WithPredicates(metadataChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.workloadsInNamespace),
			builder.WithPredicates(metadataChanged)).
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(r.targetOf),
			builder.WithPredicates(specOrLabelsChanged)).
		Complete(r)
}

func (r *OptInReconciler) workloadsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	ctx, cancel := context.WithTimeout(ctx, optInTimeout)
	defer cancel()

	var names []string
	if r.Kind == v1alpha1.TargetKindStatefulSet {
		var list appsv1.StatefulSetList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetName())); err != nil {
			log.FromContext(ctx).Error(err, "listing statefulsets for namespace change", "namespace", obj.GetName())
			return nil
		}
		for _, w := range list.Items {
			names = append(names, w.Name)
		}
	} else {
		var list appsv1.DeploymentList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetName())); err != nil {
			log.FromContext(ctx).Error(err, "listing deployments for namespace change", "namespace", obj.GetName())
			return nil
		}
		for _, w := range list.Items {
			names = append(names, w.Name)
		}
	}
	requests := make([]reconcile.Request, 0, len(names))
	for _, name := range names {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: obj.GetName(), Name: name}})
	}
	return requests
}

// targetOf re-checks a workload when any ManagedWorkload for it changes: one
// someone writes takes over, and one deleted by hand is put back.
func (r *OptInReconciler) targetOf(_ context.Context, obj client.Object) []reconcile.Request {
	mw, ok := obj.(*v1alpha1.ManagedWorkload)
	if !ok || mw.Spec.Target.Kind != r.Kind {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: mw.Namespace, Name: mw.Spec.Target.Name}}}
}
