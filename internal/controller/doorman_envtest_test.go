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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/doorman"
)

// The fake client used elsewhere holds every object, so it can't show what
// the operator's real cache leaves out. These run against an API server,
// through a cache configured exactly as the operator's.
var _ = ginkgo.Describe("Doorman routing through the operator's cache", func() {
	var (
		ns, doormanNS string
		r             *Reconciler
	)

	ginkgo.BeforeEach(func() {
		ns, doormanNS = envtestNamespace(), envtestNamespace()

		doormanSlice := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: "hybernate-doorman-abc", Namespace: doormanNS,
				Labels: map[string]string{discoveryv1.LabelServiceName: "hybernate-doorman"},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.0.0.7"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			}},
		}
		gomega.Expect(k8sClient.Create(ctx, doormanSlice)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: ns},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "shop"},
				Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
			},
		})).To(gomega.Succeed())

		operatorCache, err := cache.New(cfg, cache.Options{
			Scheme: scheme.Scheme,
			ByObject: map[client.Object]cache.ByObject{
				&discoveryv1.EndpointSlice{}: EndpointSliceCache(nil, doormanNS),
			},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cacheCtx, stop := context.WithCancel(ctx)
		ginkgo.DeferCleanup(stop)
		go func() {
			defer ginkgo.GinkgoRecover()
			gomega.Expect(operatorCache.Start(cacheCtx)).To(gomega.Succeed())
		}()
		gomega.Expect(operatorCache.WaitForCacheSync(ctx)).To(gomega.BeTrue())
		cached, err := client.New(cfg, client.Options{Scheme: scheme.Scheme, Cache: &client.CacheOptions{Reader: operatorCache}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		r = &Reconciler{
			Client:           cached,
			PodReader:        k8sClient,
			Scheme:           scheme.Scheme,
			Recorder:         events.NewFakeRecorder(100),
			DoormanService:   "hybernate-doorman",
			DoormanNamespace: doormanNS,
		}
	})

	canary := func() (*v1alpha1.ManagedWorkload, *appsv1.Deployment) {
		w := envtestWorkload(ns)
		w.Name = "canary"
		w.Spec.Target.Name = "canary"
		w.Spec.DesiredState = nil
		gomega.Expect(k8sClient.Create(ctx, w)).To(gomega.Succeed())
		w.Status.Phase = v1alpha1.PhasePaused
		target := envtestDeployment(ns, 0)
		target.Name = "canary"
		target.Spec.Template.Labels = map[string]string{"app": "shop", "track": "canary"}
		return w, target
	}
	routedSlices := func() []discoveryv1.EndpointSlice {
		var list discoveryv1.EndpointSliceList
		gomega.Expect(k8sClient.List(ctx, &list, client.InNamespace(ns),
			client.MatchingLabels{discoveryv1.LabelManagedBy: doorman.ManagedBy})).To(gomega.Succeed())
		return list.Items
	}

	ginkgo.It("leaves a Service to the other workload's Ready pods", func() {
		stable := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: "shop-abcde", Namespace: ns,
				Labels: map[string]string{
					discoveryv1.LabelServiceName: "shop",
					discoveryv1.LabelManagedBy:   "endpointslice-controller.k8s.io",
				},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.244.1.9"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
				TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: "stable-0"},
			}},
			Ports: []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(int32(8080))}},
		}
		gomega.Expect(k8sClient.Create(ctx, stable)).To(gomega.Succeed())
		w, target := canary()

		recheck := r.routeDoorman(ctx, w, target)

		gomega.Expect(routedSlices()).To(gomega.BeEmpty(), "live traffic would go through the doorman")
		gomega.Expect(conditionFalseWith(w, conditionWakeOnRequest, "ServedByOtherPods")).To(gomega.BeTrue())
		gomega.Expect(recheck).To(gomega.Equal(servedRecheck))
	})

	ginkgo.It("routes a Service with no other Ready pods", func() {
		w, target := canary()

		gomega.Expect(r.routeDoorman(ctx, w, target)).To(gomega.BeZero())

		gomega.Expect(routedSlices()).To(gomega.HaveLen(1))
		gomega.Expect(w.Status.Doorman).To(gomega.HaveLen(1))
	})
})
