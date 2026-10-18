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
	// configMaps are the data of the ConfigMaps its workloads take
	// variables from, by name.
	configMaps map[string]map[string]string
	// historySince is where its CPU history begins; zero without any.
	historySince time.Time
	// noHistory are its workloads Prometheus has no CPU for.
	noHistory []string
	// historyOutOfTime says the scan ran out of the time it gives history
	// before it had read the namespace's.
	historyOutOfTime bool
	// historyGaveUp is how long Prometheus, or the API server's proxy in
	// front of it, took to give up on the namespace's history on its own,
	// with time left; zero when it didn't.
	historyGaveUp      time.Duration
	metricsUnavailable bool
	problems           []readProblem
	// unscanned says the scan ran out of time before it scanned the
	// namespace, or before it finished.
	unscanned bool
}

// cutShort says the scan ended before reading all it could of the
// namespace: something failed for a reason other than access, which once
// the scan's time is up is that time.
func (n *namespaceScan) cutShort() bool {
	return slices.ContainsFunc(n.problems, func(p readProblem) bool {
		return !apierrors.IsForbidden(p.err) && !apierrors.IsNotFound(p.err)
	})
}

func (n *namespaceScan) problem(what reading, err error) {
	n.problems = append(n.problems, readProblem{namespace: n.namespace, what: what, err: err})
}

// scanNamespace scans one namespace's Deployments and StatefulSets. It makes
// the same few reads however many workloads the namespace has, and matches
// pods and their metrics to workloads in memory. What it can't read is
// recorded as a problem, and the scan carries on without it. History is read
// within historyCtx, and a namespace Prometheus doesn't answer for by then
// is judged from CPU right now. What reads listed across the cluster is
// taken from there rather than read again.
func (s *Scanner) scanNamespace(ctx, historyCtx context.Context, namespace string, reads *clusterReads,
	pricing nodePricing, opts ClusterOptions) namespaceScan {
	scan := namespaceScan{namespace: namespace, sources: map[workloadKey]workloadSource{}}
	namespaceLabels, err := s.namespaceLabels(ctx, reads, namespace)
	if err != nil {
		scan.problem(readingNamespace, err)
	}
	protected := v1alpha1.Protected(namespace, namespaceLabels, nil)
	workloads, err := s.listWorkloads(ctx, reads, namespace)
	if err != nil {
		scan.problem(readingWorkloads, err)
		return scan
	}
	if len(workloads) == 0 {
		return scan
	}
	owners := s.readOwners(ctx, reads, &scan)

	managed, err := s.managedInNamespace(ctx, reads, namespace)
	if err != nil {
		scan.problem(readingManaged, err)
	}
	pods, err := s.readPods(ctx, reads, namespace, owners)
	if err != nil {
		scan.problem(readingPods, err)
	}
	usage, unmeasured, err := s.readPodMetrics(ctx, reads, namespace, pods)
	switch {
	case isUnavailable(err):
		scan.metricsUnavailable = true
	case err != nil:
		scan.problem(readingMetrics, err)
	}
	scan.services, err = s.readServices(ctx, reads, namespace)
	if err != nil {
		scan.problem(readingServices, err)
	}
	scan.configMaps, err = s.readConfigMaps(ctx, reads, namespace, workloads)
	if err != nil {
		scan.problem(readingConfigMaps, err)
	}
	history := s.readHistoryOf(ctx, historyCtx, &scan, opts)

	platform := platformOf(namespace, namespaceLabels, workloads, managed)
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
			Platform:  platform[string(wl.kind)+"/"+obj.GetName()],
		}
		writer, _ := gitops.ReplicasWriter(obj.GetManagedFields())
		w.ReplicasFromGit = string(writer.Tool)
		rollouts := owners.rollouts[obj.GetUID()]
		if len(rollouts) > 0 {
			last := slices.MaxFunc(rollouts, func(a, b time.Time) int { return a.Compare(b) }).UTC()
			w.LastDeployed = &last
		}
		spec := wl.template.Spec
		w.PodCPURequestMillis, w.PodMemoryRequestBytes = metrics.Requests(metrics.WorkloadContainers(spec))
		ownCPU, _ := metrics.AppCPURequest(nil, spec)

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
				replica := metrics.ReplicaPod(obj, running)
				w.PodCPURequestMillis, w.PodMemoryRequestBytes = metrics.PodRequests(replica, spec)
				ownCPU, _ = metrics.AppCPURequest(replica, spec)
				measure(&w, usage.of(running, sel), unmeasured, spec, ownCPU)
			}
			if mw != nil && mw.Status.Activity != nil {
				judgeManaged(&w, mw, now, thresholdFor(mw, opts))
			} else {
				judgeUnmanaged(&w, obj, now, thresholdFor(mw, opts), idleAfterFor(mw, opts))
			}
		}
		w.WontPause = wontPause(obj)
		w.Clues = clues(w, now)
		w.rates, w.OnSpot = pricing.ratesFor(running, mw, opts)
		w.HourlyCost = hourlyCost(w, w.rates)
		w.MonthlyCost = w.HourlyCost * hoursPerMonth
		w.Measured = measured(mw, w.HourlyCost, now)
		w.SavedThisMonth = SavedThisMonth(mw, now)
		if history != nil && ownCPU > 0 && !w.ScaledByHand {
			w.History = replayWorkload(w, spec, ownCPU, history, owners.podsOf(wl), scan.historySince, rollouts,
				thresholdFor(mw, opts), idleAfterFor(mw, opts), opts)
			if w.History == nil && w.State != StateUnknown && w.Platform == "" {
				scan.noHistory = append(scan.noHistory, w.Namespace+"/"+w.Name)
			}
			if w.History != nil && w.State == StateUnknown && mw == nil {
				judgeFromHistory(&w, now)
			}
		}
		scan.workloads = append(scan.workloads, w)
	}
	return scan
}

// readHistoryOf reads a namespace's history within historyCtx, telling the
// scan running out of the time it gives history from Prometheus timing out
// on its own, and both from Prometheus failing. The time is shared with the
// namespace's reads from the API server, which come first, so it isn't
// queried once that's spent.
func (s *Scanner) readHistoryOf(ctx, historyCtx context.Context, scan *namespaceScan,
	opts ClusterOptions) []containerCPU {
	if opts.History != nil && historyCtx.Err() != nil && ctx.Err() == nil {
		scan.historyOutOfTime = true
		return nil
	}
	start := time.Now()
	history, err := s.readHistory(historyCtx, scan.namespace, opts)
	switch {
	case err != nil && ctx.Err() == nil && historyCtx.Err() != nil && isTimeout(err):
		scan.historyOutOfTime = true
	case err != nil && historyCtx.Err() == nil && isTimeout(err):
		scan.historyGaveUp = time.Since(start)
	case err != nil:
		scan.problem(readingHistory, err)
	}
	if first, ok := earliestSample(history); ok {
		scan.historySince = time.Unix(first, 0).UTC()
	}
	return history
}

// wontPause says why the operator refuses to pause a workload whatever its
// activity: a StatefulSet whose PVCs are deleted when it's scaled down
// would lose its data to a pause.
func wontPause(obj client.Object) string {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok {
		return ""
	}
	if p := sts.Spec.PersistentVolumeClaimRetentionPolicy; p != nil &&
		p.WhenScaled == appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		return "PVCs deleted on scale-down"
	}
	return ""
}

// measure sets a workload's CPU use from its pods' metrics, as a share of
// ownCPU, what one of its pods requests for its own containers, or says why
// it couldn't.
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

func (s *Scanner) listWorkloads(ctx context.Context, reads *clusterReads, namespace string) ([]workload, error) {
	var out []workload
	err := eachIn(ctx, s.client, reads.deployments, namespace, deploymentsItems, func(d *appsv1.Deployment) {
		if d.Labels[v1alpha1.LabelIgnore] != v1alpha1.True {
			out = append(out, deploymentWorkload(d))
		}
	})
	if err != nil {
		return nil, err
	}
	err = eachIn(ctx, s.client, reads.statefulSets, namespace, statefulSetsItems, func(sts *appsv1.StatefulSet) {
		if sts.Labels[v1alpha1.LabelIgnore] != v1alpha1.True {
			out = append(out, statefulSetWorkload(sts))
		}
	})
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
	// ownerOf is whatever controls each ReplicaSet, by its name, or ""
	// when nothing does.
	ownerOf map[string]types.UID
	// replicaSetsRead says the ReplicaSets were read, so a pod whose
	// ReplicaSet isn't among them isn't a Deployment's.
	replicaSetsRead bool
}

// readOwners reads what traces pods and rollouts to their workloads. What
// it can't read is recorded as a problem, and the scan carries on without
// it: without ReplicaSets, Deployments' rollouts aren't seen and their pods
// are matched by their selectors.
func (s *Scanner) readOwners(ctx context.Context, reads *clusterReads, scan *namespaceScan) owners {
	o := owners{rollouts: map[types.UID][]time.Time{}, replicaSets: map[types.UID][]string{},
		deploymentOf: map[string]types.UID{}, ownerOf: map[string]types.UID{}}
	namespace := scan.namespace
	err := eachIn(ctx, s.client, reads.replicaSets, namespace, replicaSetsItems, func(rs *appsv1.ReplicaSet) {
		ref := controllerRef(rs.OwnerReferences)
		if ref == nil {
			o.ownerOf[rs.Name] = ""
			return
		}
		o.ownerOf[rs.Name] = ref.UID
		o.rollouts[ref.UID] = append(o.rollouts[ref.UID], rs.CreationTimestamp.Time)
		if ref.Kind == "Deployment" {
			o.replicaSets[ref.UID] = append(o.replicaSets[ref.UID], rs.Name)
			o.deploymentOf[rs.Name] = ref.UID
		}
	})
	if err != nil {
		scan.problem(readingReplicaSets, err)
	} else {
		o.replicaSetsRead = true
	}
	err = eachIn(ctx, s.client, reads.revisions, namespace, revisionsItems, func(rev *appsv1.ControllerRevision) {
		if ref := controllerRef(rev.OwnerReferences); ref != nil {
			o.rollouts[ref.UID] = append(o.rollouts[ref.UID], rev.CreationTimestamp.Time)
		}
	})
	if err != nil && !isNotServed(err) {
		scan.problem(readingControllerRevisions, err)
	}
	return o
}

// controllerOf is the workload a pod belongs to: the Deployment behind its
// ReplicaSet, or the StatefulSet or other controller that owns it. A pod no
// controller owns has none, and nor does one whose ReplicaSet couldn't be
// read, so a selector can adopt either.
func (o owners) controllerOf(pod *corev1.Pod) (types.UID, bool) {
	ref := controllerRef(pod.OwnerReferences)
	if ref == nil {
		return "", false
	}
	if ref.Kind == "ReplicaSet" {
		if uid, ok := o.deploymentOf[ref.Name]; ok {
			return uid, true
		}
		if !o.replicaSetsRead {
			return "", false
		}
	}
	return ref.UID, true
}

// podsOf matches the names of a workload's pods in CPU history, which
// records pods long gone, so by the names their controllers give them. A
// Deployment's are its ReplicaSets' pods, and those of ReplicaSets since
// deleted, which only their names tell apart; a ReplicaSet that still
// exists and isn't the Deployment's keeps its pods out.
func (o owners) podsOf(wl workload) podNames {
	name := wl.obj.GetName()
	if wl.kind == v1alpha1.TargetKindStatefulSet {
		return statefulSetPods(name)
	}
	uid := wl.obj.GetUID()
	current := replicaSetPods(o.replicaSets[uid])
	return func(pod string) bool {
		if current(pod) {
			return true
		}
		rs, ok := deploymentReplicaSet(name, pod)
		if !ok {
			return false
		}
		owner, exists := o.ownerOf[rs]
		return !exists || owner == uid
	}
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
	// matches them would adopt, and pods whose ReplicaSet couldn't be read,
	// matched the same way.
	orphans []corev1.Pod
	// names are every pod's name, so metrics can be matched to their pod.
	names map[string]bool
	read  bool
}

func (s *Scanner) readPods(ctx context.Context, reads *clusterReads, namespace string, o owners) (podIndex, error) {
	index := podIndex{byOwner: map[types.UID][]corev1.Pod{}, names: map[string]bool{}}
	err := eachIn(ctx, s.pods, reads.pods, namespace, podsItems, func(pod *corev1.Pod) {
		trimPod(pod)
		index.names[pod.Name] = true
		if uid, ok := o.controllerOf(pod); ok {
			index.byOwner[uid] = append(index.byOwner[uid], *pod)
		} else {
			index.orphans = append(index.orphans, *pod)
		}
	})
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
func (s *Scanner) readPodMetrics(ctx context.Context, reads *clusterReads, namespace string, pods podIndex) (
	podUsage, string, error) {
	usage := podUsage{byPod: map[string]metricsv1beta1.PodMetrics{}}
	err := eachIn(ctx, s.client, reads.podMetrics, namespace, podMetricsItems, func(m *metricsv1beta1.PodMetrics) {
		if pods.names[m.Name] {
			usage.byPod[m.Name] = *m
		} else {
			usage.orphans = append(usage.orphans, *m)
		}
	})
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
func (s *Scanner) readServices(ctx context.Context, reads *clusterReads, namespace string) (
	map[string]corev1.Service, error) {
	out := map[string]corev1.Service{}
	err := eachIn(ctx, s.client, reads.services, namespace, servicesItems, func(svc *corev1.Service) {
		out[svc.Name] = dependencyService(svc)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// managedInNamespace returns the ManagedWorkload covering each workload,
// keyed by "Kind/name". Without Hybernate installed there are none, which
// isn't an error.
func (s *Scanner) managedInNamespace(ctx context.Context, reads *clusterReads, namespace string) (
	map[string]*v1alpha1.ManagedWorkload, error) {
	out := map[string]*v1alpha1.ManagedWorkload{}
	err := eachIn(ctx, s.client, reads.managed, namespace, managedItems, func(mw *v1alpha1.ManagedWorkload) {
		out[string(mw.Spec.Target.Kind)+"/"+mw.Spec.Target.Name] = mw
	})
	if err != nil && !isNotServed(err) {
		return out, err
	}
	return out, nil
}

// namespaceLabels reads a namespace's labels, which say whether it's
// protected. The operator's own protected name patterns aren't visible to
// the scan, so only the label counts for that.
func (s *Scanner) namespaceLabels(ctx context.Context, reads *clusterReads, namespace string) (
	map[string]string, error) {
	if listed := reads.namespaces.byNamespace; listed != nil {
		if found := listed[namespace]; len(found) > 0 {
			return found[0].Labels, nil
		}
		return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), namespace)
	}
	var ns corev1.Namespace
	if err := s.client.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return nil, err
	}
	return ns.Labels, nil
}
