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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
			Target:       v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: "api"},
			DesiredState: ptr.To(v1alpha1.DesiredStatePaused),
			Prediction:   v1alpha1.PredictionSpec{Confidence: 85},
		},
	}
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
		gomega.Expect(k8sClient.Create(ctx, envtestWorkload(ns))).To(gomega.Succeed())

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

		for range 3 {
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: "api"}})
			if err != nil {
				break
			}
		}
		gomega.Expect(apierrors.IsConflict(err)).To(gomega.BeTrue(), "got %v", err)
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
