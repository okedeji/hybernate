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

package metrics

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/cost"
)

// ErrNoPodMetrics is returned when the Metrics API reports no pods for a
// workload: it has no running pods, or metrics-server isn't reporting them.
var ErrNoPodMetrics = errors.New("no pod metrics found")

// ErrNoScheduledPods is returned when none of a workload's pods is on a
// node, so there's nothing to price it at.
var ErrNoScheduledPods = errors.New("no pods on a node")

// callTimeout bounds each read. The pods and the Metrics API aren't cached,
// and a blackholed metrics-server would otherwise hold a reconcile worker,
// and every wake queued behind it, for as long as the connection lasts.
const callTimeout = 5 * time.Second

// Reader reads workload metrics from the Kubernetes Metrics API and the
// workload's pods, finding them by the target's selector and keeping only
// the ones the target itself runs.
type Reader struct {
	client client.Client
	pods   client.Reader
}

// NewReader returns a Reader. Pods are read through pods rather than c so
// they needn't be cached: they're read only when a workload pauses and
// when its pricing is refreshed, at most hourly, and a cluster's pods are
// the largest thing an operator could cache.
func NewReader(c client.Client, pods client.Reader) *Reader {
	return &Reader{client: c, pods: pods}
}

// WorkloadCPUMillis returns the CPU in millicores used by the workload's own
// containers across its pods, for deciding whether it's active. See
// WorkloadContainers.
func (r *Reader) WorkloadCPUMillis(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}
	selector, err := selectorFromTarget(target)
	if err != nil {
		return 0, err
	}
	var list metricsv1beta1.PodMetricsList
	if err := r.client.List(ctx, &list, client.InNamespace(workload.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return 0, fmt.Errorf("listing pod metrics for %s/%s: %w", workload.Namespace, workload.Spec.Target.Name, err)
	}
	pods := make([]metricsv1beta1.PodMetrics, 0, len(list.Items))
	for _, pod := range list.Items {
		if runBy(target, &pod) {
			pods = append(pods, pod)
		}
	}
	if len(pods) == 0 {
		return 0, fmt.Errorf("%w for %s/%s", ErrNoPodMetrics, workload.Namespace, workload.Spec.Target.Name)
	}
	cpuMillis, _ := Usage(pods, podSpecFromTarget(target))
	return float64(cpuMillis), nil
}

// CPURequestPerReplica returns the CPU request in millicores of one replica's
// own containers, the counterpart of WorkloadCPUMillis.
func (r *Reader) CPURequestPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}
	cpuMillis, _ := Requests(WorkloadContainers(podSpecFromTarget(target)))
	if cpuMillis == 0 {
		return 0, fmt.Errorf("no cpu requests found in pod template for %s %s", workload.Spec.Target.Kind, workload.Spec.Target.Name)
	}
	return float64(cpuMillis), nil
}

// PodRequestsPerReplica returns the CPU (millicores) and memory (bytes) that
// one replica's pod requests, sidecars injected at creation included: what
// pausing a replica frees. It reads a running pod, and falls back to the pod
// template when none is running.
func (r *Reader) PodRequestsPerReplica(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cpuMillis, memBytes float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, pods, err := r.targetPods(ctx, workload)
	if err != nil {
		return 0, 0, err
	}
	cpu, mem := PodRequests(pods, podSpecFromTarget(target))
	return float64(cpu), float64(mem), nil
}

// ListRates is what the nodes the workload's pods run on cost per vCPU and
// GiB at on-demand list prices, averaged over its pods, and false when none
// runs on a node the price table has. ErrNoScheduledPods means it has no pod
// on a node. Nodes are read as metadata only: their labels are all pricing
// needs.
func (r *Reader) ListRates(ctx context.Context, workload *v1alpha1.ManagedWorkload) (cost.Rates, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, pods, err := r.targetPods(ctx, workload)
	if err != nil {
		return cost.Rates{}, false, err
	}
	var rates []cost.Rates
	scheduled := 0
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" {
			continue
		}
		scheduled++
		node := &metav1.PartialObjectMetadata{}
		node.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Node"))
		if err := r.client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
			return cost.Rates{}, false, fmt.Errorf("reading node %s: %w", pod.Spec.NodeName, err)
		}
		if p, ok := cost.ListPriceOf(cost.NodeTypeOf(node.Labels)); ok {
			rates = append(rates, p.Rates)
		}
	}
	if scheduled == 0 {
		return cost.Rates{}, false, ErrNoScheduledPods
	}
	if len(rates) == 0 {
		return cost.Rates{}, false, nil
	}
	return cost.Mean(rates), true, nil
}

// targetPods returns the target and the pods it runs.
func (r *Reader) targetPods(ctx context.Context, workload *v1alpha1.ManagedWorkload) (client.Object, []corev1.Pod, error) {
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return nil, nil, err
	}
	selector, err := selectorFromTarget(target)
	if err != nil {
		return nil, nil, err
	}
	var list corev1.PodList
	if err := r.pods.List(ctx, &list, client.InNamespace(workload.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, nil, fmt.Errorf("listing pods for %s/%s: %w", workload.Namespace, workload.Spec.Target.Name, err)
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if runBy(target, &pod) {
			pods = append(pods, pod)
		}
	}
	return target, pods, nil
}

// TotalPVCBytes returns the capacity provisioned for the claims the target's
// pods mount: the ones its pod template names, and for a StatefulSet the
// ones its volumeClaimTemplates create for each replica, including replicas
// scaled away, whose claims stay.
func (r *Reader) TotalPVCBytes(ctx context.Context, workload *v1alpha1.ManagedWorkload) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.client.List(ctx, &pvcList, client.InNamespace(workload.Namespace)); err != nil {
		return 0, fmt.Errorf("listing pvcs for %s/%s: %w", workload.Namespace, workload.Spec.Target.Name, err)
	}

	var total float64
	for _, pvc := range pvcList.Items {
		if !mounts(target, pvc.Name) {
			continue
		}
		if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			total += float64(capacity.Value())
		}
	}
	return total, nil
}

// Replicas returns the current spec.replicas for the target workload.
func (r *Reader) Replicas(ctx context.Context, workload *v1alpha1.ManagedWorkload) (int32, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	target, err := r.getTarget(ctx, workload)
	if err != nil {
		return 0, err
	}
	return replicasFromTarget(target), nil
}

func replicasFromTarget(obj client.Object) int32 {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		if t.Spec.Replicas != nil {
			return *t.Spec.Replicas
		}
	case *appsv1.StatefulSet:
		if t.Spec.Replicas != nil {
			return *t.Spec.Replicas
		}
	}
	return 1
}

func (r *Reader) getTarget(ctx context.Context, workload *v1alpha1.ManagedWorkload) (client.Object, error) {
	ref := workload.Spec.Target
	nn := types.NamespacedName{Name: ref.Name, Namespace: workload.Namespace}

	var obj client.Object
	switch ref.Kind {
	case v1alpha1.TargetKindDeployment:
		obj = &appsv1.Deployment{}
	case v1alpha1.TargetKindStatefulSet:
		obj = &appsv1.StatefulSet{}
	default:
		return nil, fmt.Errorf("unsupported kind: %s", ref.Kind)
	}

	if err := r.client.Get(ctx, nn, obj); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, ref.Name, err)
	}

	return obj, nil
}

func podSpecFromTarget(obj client.Object) corev1.PodSpec {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		return t.Spec.Template.Spec
	case *appsv1.StatefulSet:
		return t.Spec.Template.Spec
	default:
		return corev1.PodSpec{}
	}
}

// WorkloadContainers returns the containers a workload's pod template
// defines: its containers and its native sidecars, the init containers that
// always restart and so run alongside them.
//
// Containers injected when a pod is created, such as a service mesh proxy,
// aren't in the template, so they have no request to measure usage against.
// Usage is counted only for these containers too: counting a sidecar's CPU
// without its request makes an idle workload look busy.
func WorkloadContainers(spec corev1.PodSpec) []corev1.Container {
	containers := make([]corev1.Container, 0, len(spec.Containers)+len(spec.InitContainers))
	containers = append(containers, spec.Containers...)
	for _, c := range spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			containers = append(containers, c)
		}
	}
	return containers
}

// Requests sums the CPU (millicores) and memory (bytes) requested by
// containers.
func Requests(containers []corev1.Container) (cpuMillis, memBytes int64) {
	for _, c := range containers {
		cpuMillis += c.Resources.Requests.Cpu().MilliValue()
		memBytes += c.Resources.Requests.Memory().Value()
	}
	return cpuMillis, memBytes
}

// PodRequests returns what one of a workload's pods requests, from the first
// of pods that isn't being deleted. Injected sidecars appear only in a real
// pod, so the template is the fallback when no pod is running.
func PodRequests(pods []corev1.Pod, template corev1.PodSpec) (cpuMillis, memBytes int64) {
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil {
			return Requests(WorkloadContainers(pod.Spec))
		}
	}
	return Requests(WorkloadContainers(template))
}

// Usage sums the CPU and memory used by the WorkloadContainers of spec across
// the given pods.
func Usage(pods []metricsv1beta1.PodMetrics, spec corev1.PodSpec) (cpuMillis, memBytes int64) {
	own := map[string]bool{}
	for _, c := range WorkloadContainers(spec) {
		own[c.Name] = true
	}
	for _, pod := range pods {
		for _, c := range pod.Containers {
			if !own[c.Name] {
				continue
			}
			cpuMillis += c.Usage.Cpu().MilliValue()
			memBytes += c.Usage.Memory().Value()
		}
	}
	return cpuMillis, memBytes
}

func selectorFromTarget(obj client.Object) (labels.Selector, error) {
	var selector *metav1.LabelSelector
	switch t := obj.(type) {
	case *appsv1.Deployment:
		selector = t.Spec.Selector
	case *appsv1.StatefulSet:
		selector = t.Spec.Selector
	}
	if selector == nil || (len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0) {
		return nil, fmt.Errorf("no selector found on %s %s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	}
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("parsing the selector of %s: %w", obj.GetName(), err)
	}
	return s, nil
}

// maxGeneratedNamePrefix is how much of a generateName the API server keeps
// before adding its five random characters.
const maxGeneratedNamePrefix = 58

// runBy reports whether a pod, or the metrics of one, is run by target
// rather than by another workload whose selector overlaps it, such as a
// canary's. It goes by name, since metrics carry the pod's name and labels
// but not its owners: a Deployment's pods are named after the ReplicaSet
// that runs them, <deployment>-<pod-template-hash>, cut to the length the
// API server keeps of a generated name, and a StatefulSet's are
// <statefulset>-<ordinal>.
func runBy(target client.Object, pod metav1.Object) bool {
	switch target.(type) {
	case *appsv1.Deployment:
		hash := pod.GetLabels()[appsv1.DefaultDeploymentUniqueLabelKey]
		if hash == "" {
			return false
		}
		prefix := target.GetName() + "-" + hash + "-"
		if len(prefix) > maxGeneratedNamePrefix {
			prefix = prefix[:maxGeneratedNamePrefix]
		}
		return strings.HasPrefix(pod.GetName(), prefix)
	case *appsv1.StatefulSet:
		return isOrdinal(strings.CutPrefix(pod.GetName(), target.GetName()+"-"))
	}
	return false
}

// mounts reports whether target's pods mount the claim.
func mounts(target client.Object, claim string) bool {
	for _, v := range podSpecFromTarget(target).Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	sts, ok := target.(*appsv1.StatefulSet)
	if !ok {
		return false
	}
	for _, t := range sts.Spec.VolumeClaimTemplates {
		if isOrdinal(strings.CutPrefix(claim, t.Name+"-"+sts.Name+"-")) {
			return true
		}
	}
	return false
}

func isOrdinal(s string, found bool) bool {
	if !found {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}
