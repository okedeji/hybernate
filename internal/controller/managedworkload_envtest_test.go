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
	"sync"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/autoscaler"
	"github.com/okedeji/hybernate/internal/lifecycle"
)

func envtestNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mw-"}}
	gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	return ns.Name
}

func envtestDeployment(namespace string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{"app": "api"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:v1"}}},
			},
		},
	}
}

func envtestWorkload(namespace string) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target:     v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			Prediction: v1alpha1.PredictionSpec{Confidence: 85},
		},
	}
}

// withPauseRequested asks for w's workload to be paused now.
func withPauseRequested(w *v1alpha1.ManagedWorkload) *v1alpha1.ManagedWorkload {
	w.Annotations = map[string]string{v1alpha1.AnnotationPauseRequested: pauseToken}
	return w
}

var _ = ginkgo.Describe("ManagedWorkload validation", func() {
	ginkgo.It("refuses to change the target, which would strand a paused one at zero", func() {
		ns := envtestNamespace()
		w := envtestWorkload(ns)
		gomega.Expect(k8sClient.Create(ctx, w)).To(gomega.Succeed())

		w.Spec.Target.Name = "other"
		err := k8sClient.Update(ctx, w)

		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "got %v", err)
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("target is immutable"))
	})

	ginkgo.It("refuses a cpuThreshold of zero, and defaults an unset one to 10", func() {
		ns := envtestNamespace()
		w := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "ManagedWorkload",
			"metadata":   map[string]any{"name": "api", "namespace": ns},
			"spec": map[string]any{
				"target":     map[string]any{"kind": "Deployment", "name": "api"},
				"prediction": map[string]any{"confidence": int64(85)},
				"idlePolicy": map[string]any{"activity": map[string]any{"cpuThreshold": int64(0)}},
			},
		}}

		err := k8sClient.Create(ctx, w)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "got %v", err)

		gomega.Expect(unstructured.SetNestedMap(w.Object, map[string]any{}, "spec", "idlePolicy", "activity")).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, w)).To(gomega.Succeed())
		var created v1alpha1.ManagedWorkload
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(w), &created)).To(gomega.Succeed())
		gomega.Expect(created.Spec.IdlePolicy.Activity.CPUThreshold).To(gomega.Equal(10))
	})
})

var _ = ginkgo.Describe("A pause interrupted by a conflict", func() {
	var (
		ns string
		r  *Reconciler
	)

	ginkgo.BeforeEach(func() {
		ns = envtestNamespace()
		gomega.Expect(k8sClient.Create(ctx, envtestDeployment(ns, 3))).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, withPauseRequested(envtestWorkload(ns)))).To(gomega.Succeed())

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		conflicted := false
		// Another writer touches the ManagedWorkload just before the Paused
		// write lands, so the API server rejects it as a real conflict.
		c := interceptor.NewClient(base, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				if mw, ok := obj.(*v1alpha1.ManagedWorkload); ok && sub == "status" && !conflicted &&
					mw.Status.Phase == v1alpha1.PhasePaused {
					conflicted = true
					other := &v1alpha1.ManagedWorkload{}
					gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(mw), other)).To(gomega.Succeed())
					other.Labels = map[string]string{"touched": "true"}
					gomega.Expect(c.Update(ctx, other)).To(gomega.Succeed())
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		})
		finder := autoscaler.NewFinder(c)
		r = &Reconciler{
			Client:      c,
			Scheme:      scheme.Scheme,
			Recorder:    events.NewFakeRecorder(100),
			pauser:      lifecycle.NewPauser(c, finder),
			autoscalers: finder,
			engines:     newEngineRegistry(func() forecaster { return &stubForecaster{} }),
			clock:       time.Now,
		}

		// The conflict is retried promptly rather than reported, so the
		// interruption shows as that retry.
		var res ctrl.Result
		for range 3 {
			res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: "api"}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if res.RequeueAfter == staleRetry {
				break
			}
		}
		gomega.Expect(res.RequeueAfter).To(gomega.Equal(staleRetry), "the pause was interrupted by a conflict")
	})

	replicas := func() int32 {
		var d appsv1.Deployment
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "api"}, &d)).To(gomega.Succeed())
		return *d.Spec.Replicas
	}
	workload := func() *v1alpha1.ManagedWorkload {
		var w v1alpha1.ManagedWorkload
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "api"}, &w)).To(gomega.Succeed())
		return &w
	}

	ginkgo.It("is finished from the record written before scaling to zero", func() {
		interrupted := workload()
		gomega.Expect(interrupted.Status.Phase).To(gomega.Equal(v1alpha1.PhasePausing))
		gomega.Expect(interrupted.Status.Pause.PreviousReplicas).To(gomega.Equal(int32(3)))
		gomega.Expect(replicas()).To(gomega.Equal(int32(0)))

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: "api"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		paused := workload()
		gomega.Expect(paused.Status.Phase).To(gomega.Equal(v1alpha1.PhasePaused))
		gomega.Expect(paused.Status.Pause.PreviousReplicas).To(gomega.Equal(int32(3)))
	})

	ginkgo.It("is undone, at the replicas it had, when the ManagedWorkload is deleted", func() {
		gomega.Expect(k8sClient.Delete(ctx, workload())).To(gomega.Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: "api"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Expect(replicas()).To(gomega.Equal(int32(3)))
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "api"}, &v1alpha1.ManagedWorkload{})
		gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
	})
})

func kedaCRD() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "scaledobjects.keda.sh"},
		"spec": map[string]any{
			"group": "keda.sh",
			"scope": "Namespaced",
			"names": map[string]any{"kind": "ScaledObject", "listKind": "ScaledObjectList",
				"plural": "scaledobjects", "singular": "scaledobject"},
			"versions": []any{map[string]any{"name": "v1alpha1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}},
		},
	}}
}

func envtestScaledObject(namespace, target string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1",
		"kind":       "ScaledObject",
		"metadata":   map[string]any{"name": target, "namespace": namespace},
		"spec": map[string]any{"scaleTargetRef": map[string]any{"name": target},
			"minReplicaCount": int64(0), "maxReplicaCount": int64(10)},
	}}
}

// Uninstalling KEDA while the operator runs leaves its RESTMapper still
// mapping ScaledObject, so listing them answers NotFound rather than no
// match. Every workload must go on reconciling, and a paused one must still
// be handed back when its ManagedWorkload is deleted.
func TestKEDAUninstalledWhileTheOperatorRuns(t *testing.T) {
	cfg := startEnvtest(t)
	opScheme := testScheme(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := client.New(cfg, client.Options{Scheme: opScheme})
	require.NoError(t, err)

	crd := kedaCRD()
	require.NoError(t, c.Create(ctx, crd))
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}))
	require.Eventually(t, func() bool {
		return c.Create(ctx, envtestScaledObject("shop", "api")) == nil
	}, 30*time.Second, 100*time.Millisecond, "KEDA's CRD is served")
	require.NoError(t, c.Create(ctx, envtestScaledObject("shop", "web")))
	for name, replicas := range map[string]int32{"api": 3, "web": 2} {
		d := envtestDeployment("shop", replicas)
		d.Name = name
		require.NoError(t, c.Create(ctx, d))
		w := envtestWorkload("shop")
		w.Name, w.Spec.Target.Name = name, name
		if name == "api" {
			withPauseRequested(w)
		}
		require.NoError(t, c.Create(ctx, w))
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: opScheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	require.NoError(t, err)
	r := &Reconciler{Client: mgr.GetClient(), Scheme: opScheme, Recorder: events.NewFakeRecorder(1000),
		PodReader: mgr.GetAPIReader()}
	require.NoError(t, r.SetupWithManager(mgr))
	mgrCtx, stop := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { assert.NoError(t, mgr.Start(mgrCtx)) })
	t.Cleanup(func() {
		stop()
		running.Wait()
	})

	workload := func(name string) *v1alpha1.ManagedWorkload {
		var w v1alpha1.ManagedWorkload
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: name}, &w))
		return &w
	}
	replicas := func(name string) int32 {
		var d appsv1.Deployment
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: name}, &d))
		return *d.Spec.Replicas
	}
	autoscaled := func(name string) bool {
		return meta.FindStatusCondition(workload(name).Status.Conditions, conditionAutoscaled) != nil
	}
	require.Eventually(t, func() bool {
		return workload("api").Status.Phase == v1alpha1.PhasePaused && replicas("api") == 0 && autoscaled("web")
	}, 30*time.Second, 100*time.Millisecond, "api is paused, and web is seen to be scaled by KEDA")

	require.NoError(t, c.Delete(ctx, crd))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(crd), kedaCRD()))
	}, 30*time.Second, 100*time.Millisecond, "KEDA's CRD is gone")

	poke := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"poke":"after-keda"}}}`))
	for _, name := range []string{"api", "web"} {
		require.NoError(t, c.Patch(ctx, workload(name), poke))
	}
	assert.Eventually(t, func() bool { return !autoscaled("api") && !autoscaled("web") },
		30*time.Second, 100*time.Millisecond, "every workload reconciles again, and no longer reports KEDA")

	require.NoError(t, c.Delete(ctx, workload("api")))
	assert.Eventually(t, func() bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "api"}, &v1alpha1.ManagedWorkload{})
		return apierrors.IsNotFound(err)
	}, 30*time.Second, 100*time.Millisecond, "the paused workload's ManagedWorkload is deleted")
	assert.Equal(t, int32(3), replicas("api"), "and its workload is handed back at the replicas it had")
}
