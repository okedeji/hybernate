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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

func reportReconciler(t *testing.T, objs ...client.Object) *HybernateReportReconciler {
	t.Helper()
	scheme := testScheme(t)
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.HybernateReport{}).
		WithObjects(objs...)
	return &HybernateReportReconciler{
		Client: builder.Build(),
		Scheme: scheme,
	}
}

func managedWorkload(name string, phase v1alpha1.WorkloadPhase, cost *v1alpha1.CostStatus) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase: phase,
			Cost:  cost,
		},
	}
}

func TestReportReconciler_CreatesReportOnFirstRun(t *testing.T) {
	r := reportReconciler(t,
		managedWorkload("api", v1alpha1.PhaseRunning, nil),
	)

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	var report v1alpha1.HybernateReport
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: reportSingletonName}, &report))
	assert.Equal(t, 1, report.Status.TotalManagedWorkloads)
	assert.Equal(t, 1, report.Status.ActiveWorkloads)
}

func TestReportReconciler_CountsByPhase(t *testing.T) {
	r := reportReconciler(t,
		managedWorkload("api-1", v1alpha1.PhaseRunning, nil),
		managedWorkload("api-2", v1alpha1.PhaseIdle, nil),
		managedWorkload("api-3", v1alpha1.PhasePaused, nil),
		managedWorkload("api-4", v1alpha1.PhasePaused, nil),
		managedWorkload("api-5", v1alpha1.PhaseDestroyed, nil),
	)

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	var report v1alpha1.HybernateReport
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: reportSingletonName}, &report))
	assert.Equal(t, 5, report.Status.TotalManagedWorkloads)
	assert.Equal(t, 2, report.Status.ActiveWorkloads)
	assert.Equal(t, 2, report.Status.PausedWorkloads)
	assert.Equal(t, 1, report.Status.DestroyedWorkloads)
}

func TestReportReconciler_AggregatesCosts(t *testing.T) {
	r := reportReconciler(t,
		managedWorkload("api-1", v1alpha1.PhaseRunning, &v1alpha1.CostStatus{
			CurrentMonthCPUHours:     *resource.NewMilliQuantity(10000, resource.DecimalSI),
			CurrentMonthMemoryHours:  *resource.NewMilliQuantity(20000, resource.DecimalSI),
			CurrentMonthStorageHours: *resource.NewMilliQuantity(5000, resource.DecimalSI),
			MonthlySavings:           "$10.00",
			CostWithoutManagement:    "$50.00",
		}),
		managedWorkload("api-2", v1alpha1.PhasePaused, &v1alpha1.CostStatus{
			CurrentMonthCPUHours:     *resource.NewMilliQuantity(3000, resource.DecimalSI),
			CurrentMonthMemoryHours:  *resource.NewMilliQuantity(8000, resource.DecimalSI),
			CurrentMonthStorageHours: *resource.NewMilliQuantity(15000, resource.DecimalSI),
			MonthlySavings:           "$25.00",
			CostWithoutManagement:    "$30.00",
		}),
	)

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	var report v1alpha1.HybernateReport
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: reportSingletonName}, &report))

	assert.Equal(t, "$35.00", report.Status.TotalMonthlySavings)
	assert.Equal(t, "$80.00", report.Status.CostWithoutManagement)
	assert.Equal(t, "$45.00", report.Status.EstimatedMonthlyCost)
	assert.InDelta(t, 13.0, report.Status.TotalCPUHours.AsApproximateFloat64(), 0.01)
	assert.InDelta(t, 28.0, report.Status.TotalMemoryHours.AsApproximateFloat64(), 0.01)
	assert.InDelta(t, 20.0, report.Status.TotalStorageHours.AsApproximateFloat64(), 0.01)
	assert.NotNil(t, report.Status.LastUpdated)
}

func TestReportReconciler_EmptyCluster(t *testing.T) {
	r := reportReconciler(t)

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	var report v1alpha1.HybernateReport
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: reportSingletonName}, &report))
	assert.Equal(t, 0, report.Status.TotalManagedWorkloads)
	assert.Equal(t, "$0.00", report.Status.EstimatedMonthlyCost)
}

func TestReportReconciler_UpdatesExistingReport(t *testing.T) {
	existing := &v1alpha1.HybernateReport{
		ObjectMeta: metav1.ObjectMeta{Name: reportSingletonName},
	}
	r := reportReconciler(t,
		existing,
		managedWorkload("api", v1alpha1.PhaseRunning, &v1alpha1.CostStatus{
			MonthlySavings:        "$5.00",
			CostWithoutManagement: "$20.00",
		}),
	)

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	var report v1alpha1.HybernateReport
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Name: reportSingletonName}, &report))
	assert.Equal(t, 1, report.Status.TotalManagedWorkloads)
	assert.Equal(t, "$5.00", report.Status.TotalMonthlySavings)
}
