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
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	"github.com/okedeji/hybernate/internal/doorman"
)

const (
	// doormanSliceWrites is how many doorman slices are rewritten at once
	// when the doorman's pods change.
	doormanSliceWrites = 16

	// doormanSliceSyncTimeout bounds one rewrite of every doorman slice.
	doormanSliceSyncTimeout = time.Minute

	// doormanSliceQPS is the rate the slices are rewritten at: a thousand
	// in ten seconds, inside the 15 seconds a stopping doorman pod goes on
	// accepting connections.
	doormanSliceQPS = 100
)

// doormanSliceSync points every doorman EndpointSlice at the doorman's
// Ready pods as soon as they change. In a rolling update of the doorman,
// an old pod stops accepting connections within seconds of terminating.
// Reconciling every paused workload in full through a handful of workers
// would leave many Services pointing at it for minutes, refusing requests
// that should wake their workloads; rewriting only the slices' endpoints,
// many at once, takes seconds. Slices of an IP family with no Ready doorman
// pod are left to the workloads' own reconciles, which stop routing them
// (see findWorkloadsForDoorman).
type doormanSliceSync struct {
	reader client.Reader
	// writer has a rate limit of its own, so a rollout's burst of writes
	// isn't queued behind the operator's other calls.
	writer    client.Client
	namespace string
	service   string
}

func (s *doormanSliceSync) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, doormanSliceSyncTimeout)
	defer cancel()
	endpoints, err := readDoormanEndpoints(ctx, s.reader, s.namespace, s.service)
	if err != nil {
		return reconcile.Result{}, err
	}
	var list discoveryv1.EndpointSliceList
	if err := s.reader.List(ctx, &list, client.MatchingLabels{discoveryv1.LabelManagedBy: doorman.ManagedBy}); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing doorman slices: %w", err)
	}

	var g errgroup.Group
	g.SetLimit(doormanSliceWrites)
	var mu sync.Mutex
	var errs []error
	moved := 0
	for i := range list.Items {
		slice := &list.Items[i]
		want := endpoints[slice.AddressType]
		if len(want) == 0 || equality.Semantic.DeepEqual(slice.Endpoints, want) {
			continue
		}
		moved++
		g.Go(func() error {
			patch := client.MergeFrom(slice.DeepCopy())
			slice.Endpoints = want
			if err := s.writer.Patch(ctx, slice, patch); client.IgnoreNotFound(err) != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("moving doorman slice %s/%s: %w", slice.Namespace, slice.Name, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait() // every write's error is collected in errs
	if err := errors.Join(errs...); err != nil {
		return reconcile.Result{}, err
	}
	if moved > 0 {
		log.FromContext(ctx).Info("Moved doorman slices to the doorman's Ready pods",
			"slices", moved, "namespace", s.namespace, "service", s.service)
	}
	return reconcile.Result{}, nil
}

// setupDoormanSliceSync registers the doormanSliceSync, unless the doorman
// is disabled.
func (r *Reconciler) setupDoormanSliceSync(mgr ctrl.Manager) error {
	if r.DoormanService == "" {
		return nil
	}
	cfg := rest.CopyConfig(mgr.GetConfig())
	cfg.QPS, cfg.Burst = doormanSliceQPS, 2*doormanSliceQPS
	writer, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return fmt.Errorf("creating the doorman slices' client: %w", err)
	}
	syncer := &doormanSliceSync{
		reader:    mgr.GetClient(),
		writer:    client.WithFieldOwner(writer, v1alpha1.FieldManager),
		namespace: r.DoormanNamespace,
		service:   r.DoormanService,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("doormanslices").
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(syncer.request),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isDoormanEndpoints))).
		Complete(syncer)
}

// request is the one request every change to the doorman's endpoints
// queues, so a burst of changes is synced once.
func (s *doormanSliceSync) request(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: s.namespace, Name: s.service}}}
}

// doormanFamilies remembers the IP families the doorman last had Ready
// pods in.
type doormanFamilies struct {
	mu       sync.Mutex
	known    bool
	families []discoveryv1.AddressType
}

// changed records the families endpoints has, and reports whether they
// differ from those last recorded. The first call always reports a change.
func (f *doormanFamilies) changed(endpoints map[discoveryv1.AddressType][]discoveryv1.Endpoint) bool {
	var current []discoveryv1.AddressType
	for family, eps := range endpoints {
		if len(eps) > 0 {
			current = append(current, family)
		}
	}
	slices.Sort(current)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.known && slices.Equal(f.families, current) {
		return false
	}
	f.known, f.families = true, current
	return true
}
