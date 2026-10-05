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
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/gitops"
	"github.com/okedeji/hybernate/internal/metrics"
)

// namespaceWorkers is how many namespaces are scanned at once. Each reads
// its resources in turn, so a few at once hide the round trips, while the
// client's rate limit, shared by all of them, keeps the API server's load
// what it would be one at a time.
const namespaceWorkers = 4

// Why a workload's CPU couldn't be measured, when the Metrics API couldn't
// be read.
const (
	unmeasuredNoAPI     = "no Metrics API"
	unmeasuredForbidden = "pod metrics not allowed"
	unmeasuredFailed    = "pod metrics unreadable"
)

// namespaceScan is what scanning one namespace found.
type namespaceScan struct {
	namespace string
	workloads []Workload
	sources   map[workloadKey]workloadSource
	// services are its Services, with only what resolving a dependency
	// needs.
	services map[string]corev1.Service
	// historySince is where its CPU history begins; zero without any.
	historySince time.Time
	// noHistory are its workloads Prometheus has no CPU for.
	noHistory          []string
	metricsUnavailable bool
	problems           []readProblem
}

func (n *namespaceScan) problem(what reading, err error) {
	n.problems = append(n.problems, readProblem{namespace: n.namespace, what: what, err: err})
}

// scanNamespace scans one namespace's Deployments and StatefulSets. It makes
// the same few reads however many workloads the namespace has, and matches
// pods and their metrics to workloads in memory. What it can't read is
// recorded as a problem, and the scan carries on without it.
func (s *Scanner) scanNamespace(ctx context.Context, namespace string, pricing nodePricing,
	opts ClusterOptions) namespaceScan {
	scan := namespaceScan{namespace: namespace, sources: map[workloadKey]workloadSource{}}
	protected, err := s.protectedNamespace(ctx, namespace)
	if err != nil {
		scan.problem(readingNamespace, err)
	}
	workloads, err := s.listWorkloads(ctx, namespace)
	if err != nil {
		scan.problem(readingWorkloads, err)
		return scan
	}
	if len(workloads) == 0 {
		return scan
	}
	owners, err := s.readOwners(ctx, namespace)
	if err != nil {
		scan.problem(readingWorkloads, err)
		return scan
	}

	managed, err := s.managedInNamespace(ctx, namespace)
	if err != nil {
		scan.problem(readingManaged, err)
	}
	pods, err := s.readPods(ctx, namespace, owners)
	if err != nil {
		scan.problem(readingPods, err)
	}
	usage, unmeasured, err := s.readPodMetrics(ctx, namespace, pods)
	switch {
	case isUnavailable(err):
		scan.metricsUnavailable = true
	case err != nil:
		scan.problem(readingMetrics, err)
	}
	scan.services, err = s.readServices(ctx, namespace)
	if err != nil {
		scan.problem(readingServices, err)
	}
	history, err := s.readHistory(ctx, namespace, opts)
	if err != nil {
		scan.problem(readingHistory, err)
	}
	if first, ok := earliestSample(history); ok {
		scan.historySince = time.Unix(first, 0)
	}

	now := opts.Now()
	for _, wl := range workloads {
		obj := wl.obj
		mw := managed[string(wl.kind)+"/"+obj.GetName()]
		scan.sources[workloadKey{namespace, wl.kind, obj.GetName()}] = workloadSource{template: wl.template, managed: mw}
		n := int32(1)
		if wl.replicas != nil {
			n = *wl.replicas
		}
		w := Workload{
			Namespace: namespace,
			Kind:      wl.kind,
			Name:      obj.GetName(),
			Replicas:  n,
			State:     StateUnknown,
			Managed:   mw != nil,
			DryRun:    mw != nil && mw.Spec.DryRun,
			Protected: protected,
		}
		writer, _ := gitops.ReplicasWriter(obj.GetManagedFields())
		w.ReplicasFromGit = string(writer.Tool)
		rollouts := owners.rollouts[obj.GetUID()]
		if len(rollouts) > 0 {
			last := slices.MaxFunc(rollouts, func(a, b time.Time) int { return a.Compare(b) })
			w.LastDeployed = &last
		}
		spec := wl.template.Spec
		ownCPU, ownMemory := metrics.Requests(metrics.WorkloadContainers(spec))
		w.PodCPURequestMillis, w.PodMemoryRequestBytes = ownCPU, ownMemory

		var running []corev1.Pod
		if n == 0 {
			if pausedByHybernate(mw) {
				judgePaused(&w, mw, now)
			} else {
				w.State, w.Reason, w.ScaledByHand = StatePaused, "scaled to zero, not by Hybernate", true
			}
		} else {
			sel, err := wl.podSelector()
			if err != nil {
				w.Unmeasured = "invalid selector"
			} else {
				running = pods.of(obj.GetUID(), sel)
				w.PodCPURequestMillis, w.PodMemoryRequestBytes = metrics.PodRequests(running, spec)
				measure(&w, usage.of(running, sel), unmeasured, spec, ownCPU)
			}
			if mw != nil && mw.Status.Activity != nil {
				judgeManaged(&w, mw, now, thresholdFor(mw, opts))
			} else {
				judgeUnmanaged(&w, obj, now, thresholdFor(mw, opts), idleAfterFor(mw, opts))
			}
		}
		w.Clues = clues(w, now)
		w.rates, w.OnSpot = pricing.ratesFor(running, mw, opts)
		w.HourlyCost = hourlyCost(w, w.rates)
		w.MonthlyCost = w.HourlyCost * hoursPerMonth
		w.Measured = measured(mw, w.HourlyCost, now)
		w.SavedThisMonth = SavedThisMonth(mw)
		if history != nil && ownCPU > 0 && !w.ScaledByHand {
			since := replayStart(scan.historySince, obj.GetCreationTimestamp().Time, historyStep(opts.Window))
			w.History = replayWorkload(w, spec, history, owners.podsOf(wl), since, rollouts,
				thresholdFor(mw, opts), idleAfterFor(mw, opts), opts)
			if w.History == nil && w.State != StateUnknown {
				scan.noHistory = append(scan.noHistory, w.Namespace+"/"+w.Name)
			}
		}
		scan.workloads = append(scan.workloads, w)
	}
	return scan
}

// measure sets a workload's CPU use from its pods' metrics, as a share of
// what the pods measured request, or says why it couldn't.
func measure(w *Workload, podMetrics []metricsv1beta1.PodMetrics, unmeasured string, spec corev1.PodSpec, ownCPU int64) {
	switch {
	case unmeasured != "":
		w.Unmeasured = unmeasured
	case ownCPU == 0:
		w.Unmeasured = "no CPU requests"
	case len(podMetrics) == 0:
		w.Unmeasured = "no metrics yet"
	default:
		used, _ := metrics.Usage(podMetrics, spec)
		percent := int(float64(used) / float64(ownCPU*int64(len(podMetrics))) * 100)
		w.CPUPercent, w.CPUMillisUsed = &percent, used
	}
}

func (s *Scanner) listWorkloads(ctx context.Context, namespace string) ([]workload, error) {
	var out []workload
	err := listAll(ctx, s.client, func(list *appsv1.DeploymentList) {
		for i := range list.Items {
			if list.Items[i].Labels[v1alpha1.LabelIgnore] != v1alpha1.True {
				out = append(out, deploymentWorkload(&list.Items[i]))
			}
		}
	}, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}
	err = listAll(ctx, s.client, func(list *appsv1.StatefulSetList) {
		for i := range list.Items {
			if list.Items[i].Labels[v1alpha1.LabelIgnore] != v1alpha1.True {
				out = append(out, statefulSetWorkload(&list.Items[i]))
			}
		}
	}, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// owners traces a namespace's pods and rollouts to the workloads that own
// them.
type owners struct {
	// rollouts are when each workload rolled out, by its UID: one for each
	// ReplicaSet or ControllerRevision it keeps, as many as its revision
	// history limit keeps.
	rollouts map[types.UID][]time.Time
	// replicaSets are each Deployment's ReplicaSets' names, by its UID.
	replicaSets map[types.UID][]string
	// deploymentOf is the Deployment that owns each ReplicaSet, by its name.
	deploymentOf map[string]types.UID
}

func (s *Scanner) readOwners(ctx context.Context, namespace string) (owners, error) {
	o := owners{rollouts: map[types.UID][]time.Time{}, replicaSets: map[types.UID][]string{},
		deploymentOf: map[string]types.UID{}}
	err := listAll(ctx, s.client, func(list *appsv1.ReplicaSetList) {
		for _, rs := range list.Items {
			ref := controllerRef(rs.OwnerReferences)
			if ref == nil {
				continue
			}
			o.rollouts[ref.UID] = append(o.rollouts[ref.UID], rs.CreationTimestamp.Time)
			if ref.Kind == "Deployment" {
				o.replicaSets[ref.UID] = append(o.replicaSets[ref.UID], rs.Name)
				o.deploymentOf[rs.Name] = ref.UID
			}
		}
	}, client.InNamespace(namespace))
	if err != nil {
		return owners{}, err
	}
	err = listAll(ctx, s.client, func(list *appsv1.ControllerRevisionList) {
		for _, rev := range list.Items {
			if ref := controllerRef(rev.OwnerReferences); ref != nil {
				o.rollouts[ref.UID] = append(o.rollouts[ref.UID], rev.CreationTimestamp.Time)
			}
		}
	}, client.InNamespace(namespace))
	if err != nil && !isNotServed(err) {
		return owners{}, err
	}
	return o, nil
}

// controllerOf is the workload a pod belongs to: the Deployment behind its
// ReplicaSet, or the StatefulSet or other controller that owns it. A pod no
// controller owns has none.
func (o owners) controllerOf(pod *corev1.Pod) (types.UID, bool) {
	ref := controllerRef(pod.OwnerReferences)
	if ref == nil {
		return "", false
	}
	if ref.Kind == "ReplicaSet" {
		if uid, ok := o.deploymentOf[ref.Name]; ok {
			return uid, true
		}
	}
	return ref.UID, true
}

// podsOf matches the names of a workload's pods in CPU history, which
// records pods long gone, so by the names their controllers give them.
func (o owners) podsOf(wl workload) podNames {
	if wl.kind == v1alpha1.TargetKindStatefulSet {
		return statefulSetPods(wl.obj.GetName())
	}
	return replicaSetPods(o.replicaSets[wl.obj.GetUID()])
}

func controllerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// podIndex is a namespace's pods by the workload that owns them.
type podIndex struct {
	byOwner map[types.UID][]corev1.Pod
	// orphans are pods no controller owns, which a workload whose selector
	// matches them would adopt.
	orphans []corev1.Pod
	// names are every pod's name, so metrics can be matched to their pod.
	names map[string]bool
	read  bool
}

func (s *Scanner) readPods(ctx context.Context, namespace string, o owners) (podIndex, error) {
	index := podIndex{byOwner: map[types.UID][]corev1.Pod{}, names: map[string]bool{}}
	err := listAll(ctx, s.pods, func(list *corev1.PodList) {
		for _, pod := range list.Items {
			pod.ManagedFields = nil
			index.names[pod.Name] = true
			if uid, ok := o.controllerOf(&pod); ok {
				index.byOwner[uid] = append(index.byOwner[uid], pod)
			} else {
				index.orphans = append(index.orphans, pod)
			}
		}
	}, client.InNamespace(namespace))
	if err != nil {
		return podIndex{}, err
	}
	index.read = true
	return index, nil
}

// of are the pods a workload has: those it controls, and any no controller
// owns that its selector matches, which its controller would adopt.
func (p podIndex) of(uid types.UID, sel labels.Selector) []corev1.Pod {
	own := slices.Clone(p.byOwner[uid])
	for _, pod := range p.orphans {
		if sel.Matches(labels.Set(pod.Labels)) {
			own = append(own, pod)
		}
	}
	return own
}

// podUsage is a namespace's pod metrics by pod.
type podUsage struct {
	byPod map[string]metricsv1beta1.PodMetrics
	// orphans are metrics for pods the scan didn't see, as when it can't
	// read pods, which are matched to a workload by its selector.
	orphans []metricsv1beta1.PodMetrics
}

// readPodMetrics reads the namespace's pod metrics, and says why CPU can't be
// measured when they can't be read.
func (s *Scanner) readPodMetrics(ctx context.Context, namespace string, pods podIndex) (podUsage, string, error) {
	usage := podUsage{byPod: map[string]metricsv1beta1.PodMetrics{}}
	err := listAll(ctx, s.client, func(list *metricsv1beta1.PodMetricsList) {
		for _, m := range list.Items {
			if pods.names[m.Name] {
				usage.byPod[m.Name] = m
			} else {
				usage.orphans = append(usage.orphans, m)
			}
		}
	}, client.InNamespace(namespace))
	switch {
	case err == nil:
		return usage, "", nil
	case isUnavailable(err):
		return podUsage{}, unmeasuredNoAPI, err
	case apierrors.IsForbidden(err):
		return podUsage{}, unmeasuredForbidden, err
	default:
		return podUsage{}, unmeasuredFailed, err
	}
}

// of are the metrics of a workload's pods.
func (u podUsage) of(pods []corev1.Pod, sel labels.Selector) []metricsv1beta1.PodMetrics {
	var out []metricsv1beta1.PodMetrics
	for _, pod := range pods {
		if m, ok := u.byPod[pod.Name]; ok {
			out = append(out, m)
		}
	}
	for _, m := range u.orphans {
		if sel.Matches(labels.Set(m.Labels)) {
			out = append(out, m)
		}
	}
	return out
}

// readServices reads a namespace's Services, keeping only what resolving a
// dependency needs.
func (s *Scanner) readServices(ctx context.Context, namespace string) (map[string]corev1.Service, error) {
	out := map[string]corev1.Service{}
	err := listAll(ctx, s.client, func(list *corev1.ServiceList) {
		for _, svc := range list.Items {
			out[svc.Name] = corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name},
				Spec:       corev1.ServiceSpec{Selector: svc.Spec.Selector, ClusterIP: svc.Spec.ClusterIP},
			}
		}
	}, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// managedInNamespace returns the ManagedWorkload covering each workload,
// keyed by "Kind/name". Without Hybernate installed there are none, which
// isn't an error.
func (s *Scanner) managedInNamespace(ctx context.Context, namespace string) (map[string]*v1alpha1.ManagedWorkload, error) {
	out := map[string]*v1alpha1.ManagedWorkload{}
	err := listAll(ctx, s.client, func(list *v1alpha1.ManagedWorkloadList) {
		for i := range list.Items {
			mw := &list.Items[i]
			out[string(mw.Spec.Target.Kind)+"/"+mw.Spec.Target.Name] = mw
		}
	}, client.InNamespace(namespace))
	if err != nil && !isNotServed(err) {
		return out, err
	}
	return out, nil
}

// protectedNamespace says the namespace is labelled protected. The
// operator's own protected name patterns aren't visible to the scan, so
// only the label counts here.
func (s *Scanner) protectedNamespace(ctx context.Context, namespace string) (bool, error) {
	var ns corev1.Namespace
	if err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return false, err
	}
	return v1alpha1.Protected(ns.Name, ns.Labels, nil), nil
}
