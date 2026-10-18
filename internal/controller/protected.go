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
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

const (
	conditionProtected = "Protected"

	reasonPVCsDeletedOnScaleDown = "PVCsDeletedOnScaleDown"
)

// protectedRecheckInterval is how often a workload in a protected namespace
// is looked at again, as a fallback to the watch on its namespace.
const protectedRecheckInterval = 10 * time.Minute

// inProtectedNamespace says the workload's namespace is one Hybernate must
// not manage.
func (r *Reconciler) inProtectedNamespace(ctx context.Context, workload *v1alpha1.ManagedWorkload) (bool, error) {
	var ns corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: workload.Namespace}, &ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting namespace %s: %w", workload.Namespace, err)
	}
	return v1alpha1.Protected(ns.Name, ns.Labels, r.ProtectedNamespaces), nil
}

// protectionOf is why Hybernate mustn't pause the workload, or nil when it
// may.
func (r *Reconciler) protectionOf(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	target client.Object) (*protection, error) {
	protected, err := r.inProtectedNamespace(ctx, workload)
	switch {
	case err != nil:
		return nil, err
	case protected:
		p := namespaceProtection(workload.Namespace)
		return &p, nil
	case r.isHybernate(target):
		p := hybernateProtection(workload)
		return &p, nil
	case deletesClaimsOnScaleDown(target):
		p := claimsProtection(workload)
		return &p, nil
	}
	return nil, nil
}

// deletesClaimsOnScaleDown reports whether target is a StatefulSet whose
// controller deletes its PersistentVolumeClaims as it scales down, so a
// pause would delete its data.
func deletesClaimsOnScaleDown(target client.Object) bool {
	sts, ok := target.(*appsv1.StatefulSet)
	if !ok || sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		return false
	}
	return sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled == appsv1.DeletePersistentVolumeClaimRetentionPolicyType
}

// controlPlaneLabel marks the operator's and the doorman's Deployments.
const controlPlaneLabel = "control-plane"

// isHybernate reports whether target is the operator's or the doorman's own
// Deployment, which Hybernate never pauses, however its namespace is
// labelled: paused, there'd be nothing left to wake it, or anything else.
func (r *Reconciler) isHybernate(target client.Object) bool {
	if !slices.Contains(r.HybernateNamespaces, target.GetNamespace()) {
		return false
	}
	switch target.GetLabels()[controlPlaneLabel] {
	case "controller-manager", "doorman":
		return true
	}
	return false
}

// protection is why a workload is protected: the condition's reason and
// message, and what a refused pause request and the event say.
type protection struct {
	reason, message, why string
}

func namespaceProtection(namespace string) protection {
	why := fmt.Sprintf("namespace %s is protected, so Hybernate doesn't pause workloads in it", namespace)
	return protection{reason: "ProtectedNamespace", why: why,
		message: fmt.Sprintf("%s; label it %s=%s to allow it", why, v1alpha1.LabelAllowProtected, v1alpha1.True)}
}

func hybernateProtection(workload *v1alpha1.ManagedWorkload) protection {
	why := fmt.Sprintf("%s %s runs Hybernate itself, which never pauses its own components",
		workload.Spec.Target.Kind, workload.Spec.Target.Name)
	return protection{reason: "HybernateComponent", why: why, message: why}
}

func claimsProtection(workload *v1alpha1.ManagedWorkload) protection {
	why := fmt.Sprintf("StatefulSet %s deletes its PersistentVolumeClaims when scaled down "+
		"(persistentVolumeClaimRetentionPolicy.whenScaled: Delete), so Hybernate doesn't pause it: "+
		"pausing it would delete its data", workload.Spec.Target.Name)
	return protection{reason: reasonPVCsDeletedOnScaleDown, why: why,
		message: why + "; set whenScaled to Retain to let Hybernate pause it"}
}

// reconcileProtected keeps a protected workload running: Hybernate pauses
// nothing in a protected namespace, nor its own components, nor a
// StatefulSet that deletes its claims as it scales down, and hands back
// what it had paused, or was pausing, before it was protected, so
// protecting one never leaves a workload off.
func (r *Reconciler) reconcileProtected(ctx context.Context, workload *v1alpha1.ManagedWorkload,
	p protection) (ctrl.Result, error) {
	reported := meta.IsStatusConditionTrue(workload.Status.Conditions, conditionProtected)
	r.setCondition(workload, conditionProtected, metav1.ConditionTrue, p.reason, p.message)
	if err := r.refusePauseRequest(ctx, workload, "Warning", ReasonProtected, p.why); err != nil {
		return ctrl.Result{}, err
	}

	routed := len(workload.Status.Doorman) > 0
	released, replicas, err := r.releaseTarget(ctx, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case workload.Status.Phase != v1alpha1.PhaseRunning || released:
		r.endWouldBePauseNow(workload)
		if err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Protected"); err != nil {
			return ctrl.Result{}, err
		}
	case !reported || routed:
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status of a protected workload: %w", err)
		}
	}
	if !reported {
		r.emitEvent(workload, false, "Warning", ReasonProtected, actionCheckNamespace, "%s", p.why)
	}
	if released {
		r.announceHandBack(workload, replicas, p.why)
	}
	return ctrl.Result{RequeueAfter: protectedRecheckInterval}, nil
}
