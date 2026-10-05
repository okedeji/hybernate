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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

const conditionProtected = "Protected"

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

// reconcileProtected keeps a workload in a protected namespace running:
// Hybernate pauses nothing there, and hands back what it had paused, or was
// pausing, before the namespace was protected, so protecting one never
// leaves a workload off.
func (r *Reconciler) reconcileProtected(ctx context.Context, workload *v1alpha1.ManagedWorkload) (ctrl.Result, error) {
	reported := meta.IsStatusConditionTrue(workload.Status.Conditions, conditionProtected)
	r.setCondition(workload, conditionProtected, metav1.ConditionTrue, "ProtectedNamespace",
		fmt.Sprintf("namespace %s is protected, so Hybernate doesn't pause workloads in it; label it %s=%s to allow it",
			workload.Namespace, v1alpha1.LabelAllowProtected, v1alpha1.True))

	pause := workload.Status.Pause
	routed := len(workload.Status.Doorman) > 0
	released, err := r.releaseTarget(ctx, workload)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case workload.Status.Phase != v1alpha1.PhaseRunning || released:
		if _, err := r.transition(ctx, workload, v1alpha1.PhaseRunning, "Protected"); err != nil {
			return ctrl.Result{}, err
		}
	case !reported || routed:
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status of a protected workload: %w", err)
		}
	}
	if !reported {
		r.emitEvent(workload, false, "Warning", ReasonProtected, actionCheckNamespace,
			"namespace %s is protected, so Hybernate doesn't pause workloads in it", workload.Namespace)
	}
	if released {
		r.emitEvent(workload, false, "Normal", ReasonResumed, actionResume,
			"restored to %d replicas: namespace %s is protected", pause.PreviousReplicas, workload.Namespace)
	}
	return ctrl.Result{RequeueAfter: protectedRecheckInterval}, nil
}
