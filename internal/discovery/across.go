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

package discovery

import (
	"context"

	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
)

// listed is one kind of object listed across the cluster at once, by
// namespace. With neither byNamespace nor err, it wasn't, as when the user
// may only list it namespace by namespace, and each namespace lists its own.
type listed[T any] struct {
	byNamespace map[string][]T
	// err is why it couldn't be listed when listing it in each namespace
	// would fail the same way, as for an API the cluster doesn't serve.
	err error
}

// clusterReads are what a scan of every namespace lists across the cluster
// at once rather than in each namespace: a cluster of thousands of
// namespaces then takes a few lists a page at a time, not ten in each.
type clusterReads struct {
	namespaces   listed[corev1.Namespace]
	deployments  listed[appsv1.Deployment]
	statefulSets listed[appsv1.StatefulSet]
	replicaSets  listed[appsv1.ReplicaSet]
	revisions    listed[appsv1.ControllerRevision]
	managed      listed[v1alpha1.ManagedWorkload]
	pods         listed[corev1.Pod]
	podMetrics   listed[metricsv1beta1.PodMetrics]
	services     listed[corev1.Service]
	// configMaps are only those the workloads take variables from.
	configMaps listed[corev1.ConfigMap]
}

// readAcross lists each kind the scan reads across the cluster, keeping
// what's in the namespaces scanned. The ConfigMaps are listed once the
// workloads are known, keeping only those they take variables from.
func (s *Scanner) readAcross(ctx context.Context, namespaces []string) *clusterReads {
	scanned := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		scanned[ns] = true
	}
	inScanned := func(obj client.Object) bool { return scanned[obj.GetNamespace()] }
	r := &clusterReads{}
	var g errgroup.Group
	g.SetLimit(namespaceWorkers)
	g.Go(func() error {
		r.namespaces = listAcross(ctx, s.client, namespacesItems, func(ns *corev1.Namespace) bool {
			// A Namespace isn't in one, so it's filed under its own name.
			ns.Namespace = ns.Name
			return scanned[ns.Name]
		})
		return nil
	})
	g.Go(func() error {
		r.deployments = listAcross(ctx, s.client, deploymentsItems, keepIn[appsv1.Deployment](inScanned))
		return nil
	})
	g.Go(func() error {
		r.statefulSets = listAcross(ctx, s.client, statefulSetsItems, keepIn[appsv1.StatefulSet](inScanned))
		return nil
	})
	g.Go(func() error {
		r.replicaSets = listAcross(ctx, s.client, replicaSetsItems, func(rs *appsv1.ReplicaSet) bool {
			*rs = appsv1.ReplicaSet{ObjectMeta: ownership(rs.ObjectMeta)}
			return scanned[rs.Namespace]
		})
		return nil
	})
	g.Go(func() error {
		r.revisions = listAcross(ctx, s.client, revisionsItems, func(rev *appsv1.ControllerRevision) bool {
			*rev = appsv1.ControllerRevision{ObjectMeta: ownership(rev.ObjectMeta)}
			return scanned[rev.Namespace]
		})
		return nil
	})
	g.Go(func() error {
		r.managed = listAcross(ctx, s.client, managedItems, keepIn[v1alpha1.ManagedWorkload](inScanned))
		return nil
	})
	g.Go(func() error {
		r.pods = listAcross(ctx, s.pods, podsItems, func(pod *corev1.Pod) bool {
			trimPod(pod)
			return scanned[pod.Namespace]
		})
		return nil
	})
	g.Go(func() error {
		r.podMetrics = listAcross(ctx, s.client, podMetricsItems, keepIn[metricsv1beta1.PodMetrics](inScanned))
		return nil
	})
	g.Go(func() error {
		r.services = listAcross(ctx, s.client, servicesItems, func(svc *corev1.Service) bool {
			*svc = dependencyService(svc)
			return scanned[svc.Namespace]
		})
		return nil
	})
	_ = g.Wait() // each kind that failed is listed in each namespace instead

	referenced := map[string]map[string]bool{}
	for ns, list := range r.deployments.byNamespace {
		for i := range list {
			referenceConfigMaps(referenced, ns, list[i].Spec.Template.Spec)
		}
	}
	for ns, list := range r.statefulSets.byNamespace {
		for i := range list {
			referenceConfigMaps(referenced, ns, list[i].Spec.Template.Spec)
		}
	}
	if r.deployments.byNamespace != nil && r.statefulSets.byNamespace != nil {
		r.configMaps = listAcross(ctx, s.client, configMapsItems, func(cm *corev1.ConfigMap) bool {
			*cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: cm.Name}, Data: cm.Data}
			return referenced[cm.Namespace][cm.Name]
		})
	}
	return r
}

func referenceConfigMaps(referenced map[string]map[string]bool, namespace string, spec corev1.PodSpec) {
	for _, name := range ConfigMapsReferenced(spec) {
		if referenced[namespace] == nil {
			referenced[namespace] = map[string]bool{}
		}
		referenced[namespace][name] = true
	}
}

// listAcross lists a kind across the cluster a page at a time, keeping the
// objects keep says to, as keep may leave them, by namespace. Only an API
// the cluster doesn't serve is an error worth keeping: anything else, a
// denial included, is for each namespace's own list to find out.
func listAcross[T any, L any, PL interface {
	*L
	client.ObjectList
}](ctx context.Context, r client.Reader, items func(PL) []T, keep func(*T) bool) listed[T] {
	out := map[string][]T{}
	err := listAll(ctx, r, func(list PL) {
		objs := items(list)
		for i := range objs {
			if !keep(&objs[i]) {
				continue
			}
			ns := any(&objs[i]).(client.Object).GetNamespace()
			out[ns] = append(out[ns], objs[i])
		}
	})
	switch {
	case err == nil:
		return listed[T]{byNamespace: out}
	case isUnavailable(err):
		return listed[T]{err: err}
	default:
		return listed[T]{}
	}
}

// eachIn calls each with every object of a kind in namespace: those listed
// across the cluster, when they were, or else those listing the namespace
// finds.
func eachIn[T any, L any, PL interface {
	*L
	client.ObjectList
}](ctx context.Context, r client.Reader, across listed[T], namespace string, items func(PL) []T,
	each func(*T)) error {
	switch {
	case across.err != nil:
		return across.err
	case across.byNamespace != nil:
		objs := across.byNamespace[namespace]
		for i := range objs {
			each(&objs[i])
		}
		return nil
	}
	return listAll(ctx, r, func(list PL) {
		objs := items(list)
		for i := range objs {
			each(&objs[i])
		}
	}, client.InNamespace(namespace))
}

func keepIn[T any](scanned func(client.Object) bool) func(*T) bool {
	return func(obj *T) bool { return scanned(any(obj).(client.Object)) }
}

// ownership is what of an object traces it to its owner and when it was
// made, all the scan reads of a ReplicaSet or ControllerRevision.
func ownership(m metav1.ObjectMeta) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name, OwnerReferences: m.OwnerReferences,
		CreationTimestamp: m.CreationTimestamp}
}

// trimPod keeps what the scan reads of a pod: what owns it, its labels,
// whether it's running, its node, and its containers' requests. A
// cluster's pods are the most it reads, and the rest of each is most of it.
func trimPod(pod *corev1.Pod) {
	pod.ManagedFields, pod.Annotations = nil, nil
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName, Containers: trimContainers(pod.Spec.Containers),
		InitContainers: trimContainers(pod.Spec.InitContainers)}
	pod.Status = corev1.PodStatus{Phase: pod.Status.Phase}
}

func trimContainers(containers []corev1.Container) []corev1.Container {
	for i, c := range containers {
		containers[i] = corev1.Container{Name: c.Name, Resources: c.Resources, RestartPolicy: c.RestartPolicy}
	}
	return containers
}

// dependencyService is what resolving a dependency needs of a Service.
func dependencyService(svc *corev1.Service) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name},
		Spec:       corev1.ServiceSpec{Selector: svc.Spec.Selector, ClusterIP: svc.Spec.ClusterIP},
	}
}

func namespacesItems(l *corev1.NamespaceList) []corev1.Namespace                  { return l.Items }
func deploymentsItems(l *appsv1.DeploymentList) []appsv1.Deployment               { return l.Items }
func statefulSetsItems(l *appsv1.StatefulSetList) []appsv1.StatefulSet            { return l.Items }
func replicaSetsItems(l *appsv1.ReplicaSetList) []appsv1.ReplicaSet               { return l.Items }
func revisionsItems(l *appsv1.ControllerRevisionList) []appsv1.ControllerRevision { return l.Items }
func managedItems(l *v1alpha1.ManagedWorkloadList) []v1alpha1.ManagedWorkload     { return l.Items }
func podsItems(l *corev1.PodList) []corev1.Pod                                    { return l.Items }
func podMetricsItems(l *metricsv1beta1.PodMetricsList) []metricsv1beta1.PodMetrics {
	return l.Items
}
func servicesItems(l *corev1.ServiceList) []corev1.Service       { return l.Items }
func configMapsItems(l *corev1.ConfigMapList) []corev1.ConfigMap { return l.Items }
