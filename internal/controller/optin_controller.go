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
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
)

// errNameTaken means a ManagedWorkload someone else wrote already has the
// name the workload's own would get.
var errNameTaken = errors.New("name taken by another ManagedWorkload")

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
	ours, written, err := r.managedWorkloadsFor(ctx, req.Namespace, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if written != nil || !r.optedIn(obj, &ns) {
		return ctrl.Result{}, r.release(ctx, obj, ours, written)
	}

	spec, problems := optInSpec(target, obj.GetAnnotations(), ns.GetAnnotations(), r.Defaults)
	for _, p := range problems {
		r.Recorder.Eventf(obj, nil, "Warning", ReasonInvalidSetting, actionOptIn,
			"%s, so its default is used", p)
	}
	return ctrl.Result{}, r.apply(ctx, obj, ours, spec)
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

// managedWorkloadsFor returns the label-created ManagedWorkload for the
// target, and one someone wrote for it, if either exists.
func (r *OptInReconciler) managedWorkloadsFor(ctx context.Context, namespace string, target v1alpha1.WorkloadRef) (
	ours, written *v1alpha1.ManagedWorkload, err error) {
	var list v1alpha1.ManagedWorkloadList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing managed workloads: %w", err)
	}
	for i := range list.Items {
		mw := &list.Items[i]
		if mw.Spec.Target != target || !mw.DeletionTimestamp.IsZero() {
			continue
		}
		if mw.Labels[v1alpha1.LabelFromLabel] == v1alpha1.True {
			ours = mw
		} else {
			written = mw
		}
	}
	return ours, written, nil
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

func (r *OptInReconciler) apply(ctx context.Context, obj client.Object, ours *v1alpha1.ManagedWorkload, spec v1alpha1.ManagedWorkloadSpec) error {
	mw := &v1alpha1.ManagedWorkload{ObjectMeta: metav1.ObjectMeta{Name: obj.GetName(), Namespace: obj.GetNamespace()}}
	if ours != nil {
		mw.Name = ours.Name
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, mw, func() error {
		if mw.ResourceVersion != "" && mw.Labels[v1alpha1.LabelFromLabel] != v1alpha1.True {
			return errNameTaken
		}
		// Only this label is set: copying the workload's labels would carry
		// a GitOps tool's tracking label, and the tool would claim and
		// prune the ManagedWorkload as its own.
		if mw.Labels == nil {
			mw.Labels = map[string]string{}
		}
		mw.Labels[v1alpha1.LabelFromLabel] = v1alpha1.True
		if !equality.Semantic.DeepEqual(mw.Spec, spec) {
			mw.Spec = spec
		}
		return controllerutil.SetOwnerReference(obj, mw, r.Scheme)
	})
	if errors.Is(err, errNameTaken) {
		r.Recorder.Eventf(obj, nil, "Warning", ReasonLabelIgnored, actionOptIn,
			"can't manage it from its label: a ManagedWorkload named %s already exists for another workload", mw.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("applying managed workload %s: %w", mw.Name, err)
	}
	if op == controllerutil.OperationResultCreated {
		r.Recorder.Eventf(obj, nil, "Normal", ReasonManaged, actionOptIn, "managed by Hybernate as ManagedWorkload %s", mw.Name)
		log.FromContext(ctx).Info("managing workload from its label", "workload", obj.GetName(),
			"namespace", obj.GetNamespace(), "managed_workload", mw.Name)
	}
	return nil
}

// SetupWithManager reconciles a workload when its labels or annotations
// change, when its namespace's do, and when its ManagedWorkload changes.
// Replica changes, which pausing makes, don't trigger it.
func (r *OptInReconciler) SetupWithManager(mgr ctrl.Manager) error {
	metadataChanged := predicate.Or(predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).
		Named("optin-"+string(r.Kind)).
		For(r.newWorkload(), builder.WithPredicates(metadataChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.workloadsInNamespace),
			builder.WithPredicates(metadataChanged)).
		Watches(&v1alpha1.ManagedWorkload{}, handler.EnqueueRequestsFromMapFunc(r.targetOf)).
		Complete(r)
}

func (r *OptInReconciler) workloadsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
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
