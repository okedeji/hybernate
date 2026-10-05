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

package doorman

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

const (
	defaultMaxWait = 2 * time.Minute

	// routeSyncInterval is how often listeners are matched to the routes in
	// ManagedWorkload status. A connection to a newly paused workload in the
	// gap is refused, as it would be without the doorman.
	routeSyncInterval = 2 * time.Second

	// backendPollInterval is how often held connections look for a Ready
	// pod when nothing tells the doorman that endpoints changed. With an
	// informer, backendResyncInterval is only a safety net.
	backendPollInterval   = 250 * time.Millisecond
	backendResyncInterval = 5 * time.Second

	// readyStaleness is how long the doorman stays Ready without loading its
	// routes. Past it, it's serving routes that may be gone or wrong.
	readyStaleness = 30 * time.Second

	// shutdownGrace is how long connections are kept after the doorman
	// stops accepting new ones. It fits in the manager's 30s graceful
	// shutdown and the pod's termination grace period.
	shutdownGrace = 25 * time.Second

	acceptRetry = 100 * time.Millisecond
)

// Informers is the part of a controller-runtime cache the doorman uses to
// hear the moment a Service's endpoints change.
type Informers interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// Options configure a Server.
type Options struct {
	// Host is the address to listen on; every interface when empty.
	Host string

	// Informers, when set, wake held connections as soon as their
	// workload's endpoints change instead of at the next poll.
	Informers Informers
}

// route is what the doorman knows about one listening port.
type route struct {
	workload types.NamespacedName
	service  string
	portName string
	maxWait  time.Duration
	page     bool

	// drainUntil is set once the route has left status. Until then, the
	// port passes connections to the workload's Ready pods, if any, without
	// waking it.
	drainUntil time.Time
}

func (r route) draining() bool { return !r.drainUntil.IsZero() }

func (r route) backendKey() backendKey {
	return backendKey{service: serviceRef{namespace: r.workload.Namespace, name: r.service}, portName: r.portName}
}

// Server listens on every allocated doorman port. It reads routes and the
// workloads' real endpoints from the cache, and wakes a workload by stamping
// its last-request annotation, the same path a developer portal uses.
type Server struct {
	client    client.Client
	recorder  events.EventRecorder
	host      string
	informers Informers
	now       func() time.Time
	grace     time.Duration
	pollEvery time.Duration
	silent    time.Duration

	backends  *backends
	admission *admission

	mu        sync.Mutex
	routes    map[int32]route
	conflicts map[int32]bool
	listeners map[int32]net.Listener
	conns     map[net.Conn]struct{}
	wakeCalls map[types.NamespacedName]*wakeCall
	stamped   map[types.NamespacedName]stampRecord
	lastWarn  map[types.NamespacedName]time.Time
	closed    bool

	accepting sync.WaitGroup
	handling  sync.WaitGroup
	lastSync  atomic.Int64
	stopping  atomic.Bool
}

// NewServer returns a doorman that reads and writes through c.
func NewServer(c client.Client, recorder events.EventRecorder, opts Options) *Server {
	return &Server{
		client:    c,
		recorder:  recorder,
		host:      opts.Host,
		informers: opts.Informers,
		now:       time.Now,
		grace:     shutdownGrace,
		pollEvery: backendPollInterval,
		silent:    silentWake,
		backends:  newBackends(c),
		admission: newAdmission(defaultLimits),
		routes:    map[int32]route{},
		conflicts: map[int32]bool{},
		listeners: map[int32]net.Listener{},
		conns:     map[net.Conn]struct{}{},
		wakeCalls: map[types.NamespacedName]*wakeCall{},
		stamped:   map[types.NamespacedName]stampRecord{},
		lastWarn:  map[types.NamespacedName]time.Time{},
	}
}

// Start serves until ctx is done. Then it stops accepting connections,
// gives those it has up to the grace period to finish, closes the rest, and
// waits for their goroutines.
func (s *Server) Start(ctx context.Context) error {
	// Connections outlive ctx by the grace period, so they get their own.
	connCtx, cancelConns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelConns()

	poll := s.pollEvery
	if s.informers != nil {
		stop, err := s.watchEndpoints(ctx)
		if err != nil {
			return err
		}
		defer stop()
		poll = backendResyncInterval
	}
	syncTicker := time.NewTicker(routeSyncInterval)
	defer syncTicker.Stop()
	pollTicker := time.NewTicker(poll)
	defer pollTicker.Stop()

	s.sync(ctx, connCtx)
	for {
		select {
		case <-ctx.Done():
			s.shutdown(cancelConns)
			return nil
		case <-pollTicker.C:
			s.backends.changedAll()
		case <-syncTicker.C:
			s.sync(ctx, connCtx)
		}
	}
}

func (s *Server) sync(ctx, connCtx context.Context) {
	if err := s.syncRoutes(ctx, connCtx); err != nil {
		log.FromContext(ctx).WithName("doorman").Error(err, "syncing doorman routes")
	}
	s.admission.prune(s.now())
}

// watchEndpoints wakes the connections held for a Service whenever one of
// its EndpointSlices changes.
func (s *Server) watchEndpoints(ctx context.Context) (func(), error) {
	informer, err := s.informers.GetInformer(ctx, &discoveryv1.EndpointSlice{})
	if err != nil {
		return nil, fmt.Errorf("getting the endpointslice informer: %w", err)
	}
	changed := func(obj any) {
		if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
			s.backends.changed(serviceRef{namespace: slice.Namespace, name: slice.Labels[discoveryv1.LabelServiceName]})
		}
	}
	registration, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    changed,
		UpdateFunc: func(_, obj any) { changed(obj) },
		DeleteFunc: changed,
	})
	if err != nil {
		return nil, fmt.Errorf("watching endpointslices: %w", err)
	}
	return func() { _ = informer.RemoveEventHandler(registration) }, nil // the informer stops with the cache anyway
}

// Ready reports whether the doorman's routes are current. A doorman that
// can't load them, or is shutting down, leaves its Service, and the operator
// stops sending paused workloads' traffic to it.
func (s *Server) Ready(_ *http.Request) error {
	if s.stopping.Load() {
		return errors.New("doorman is shutting down")
	}
	last := s.lastSync.Load()
	if last == 0 {
		return errors.New("doorman routes not loaded yet")
	}
	if age := s.now().Sub(time.Unix(0, last)); age > readyStaleness {
		return fmt.Errorf("doorman routes last loaded %s ago", age.Round(time.Second))
	}
	return nil
}

func (s *Server) shutdown(cancelConns context.CancelFunc) {
	s.stopping.Store(true)
	s.mu.Lock()
	for port, l := range s.listeners {
		_ = l.Close() // ends its accept loop; nothing to do if it fails
		delete(s.listeners, port)
	}
	s.mu.Unlock()
	s.accepting.Wait()

	done := make(chan struct{})
	go func() {
		s.handling.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.grace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}

	cancelConns()
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.Close() // ends its proxy copies; the client sees the connection drop
	}
	s.mu.Unlock()
	<-done
}

// syncRoutes opens a listener for every route and closes listeners whose
// route is gone. Closing a listener doesn't affect connections it accepted.
// A port claimed by more than one route is served for none of them: picking
// one would send another workload's traffic to it.
func (s *Server) syncRoutes(ctx, connCtx context.Context) error {
	var list v1alpha1.ManagedWorkloadList
	if err := s.client.List(ctx, &list); err != nil {
		return fmt.Errorf("listing managed workloads: %w", err)
	}
	now := s.now()
	s.lastSync.Store(now.UnixNano())

	claims := map[int32][]route{}
	for _, w := range list.Items {
		maxWait := defaultMaxWait
		if w.Spec.Wake != nil && w.Spec.Wake.MaxWait != nil && w.Spec.Wake.MaxWait.Duration > 0 {
			maxWait = w.Spec.Wake.MaxWait.Duration
		}
		page := w.Spec.Wake == nil || w.Spec.Wake.Page == nil || *w.Spec.Wake.Page
		for _, r := range w.Status.Doorman {
			claims[r.DoormanPort] = append(claims[r.DoormanPort], route{
				workload: types.NamespacedName{Namespace: w.Namespace, Name: w.Name},
				service:  r.Service,
				portName: r.PortName,
				maxWait:  maxWait,
				page:     page,
			})
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	routes := map[int32]route{}
	conflicts := map[int32][]route{}
	for port, rs := range claims {
		if len(rs) > 1 {
			conflicts[port] = rs
			continue
		}
		routes[port] = rs[0]
	}
	for port, old := range s.routes {
		if _, ok := conflicts[port]; ok {
			continue
		}
		current, ok := routes[port]
		if ok && current.workload == old.workload {
			continue
		}
		if !old.draining() {
			old.drainUntil = now.Add(RouteDrain)
			s.backends.changed(old.backendKey().service)
		}
		if !now.Before(old.drainUntil) {
			continue
		}
		if ok {
			conflicts[port] = []route{old, current}
			delete(routes, port)
			continue
		}
		routes[port] = old
	}
	s.reportConflicts(ctx, conflicts)
	s.routes = routes

	keep := map[serviceRef]bool{}
	for _, rt := range routes {
		keep[rt.backendKey().service] = true
	}
	s.backends.retain(keep)

	for port, l := range s.listeners {
		if _, ok := routes[port]; !ok {
			_ = l.Close() // stops accepting; held connections continue
			delete(s.listeners, port)
		}
	}
	if s.stopping.Load() {
		return nil
	}
	var errs []error
	for port := range routes {
		if _, ok := s.listeners[port]; ok {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(int(port))))
		if err != nil {
			errs = append(errs, fmt.Errorf("listening on doorman port %d: %w", port, err))
			continue
		}
		s.listeners[port] = l
		s.accepting.Go(func() { s.accept(connCtx, port, l) })
	}
	return errors.Join(errs...)
}

// reportConflicts logs each port newly claimed twice, once, and keeps the
// gauge of ports in conflict current. s.mu must be held.
func (s *Server) reportConflicts(ctx context.Context, conflicts map[int32][]route) {
	logger := log.FromContext(ctx).WithName("doorman")
	current := map[int32]bool{}
	for port, rs := range conflicts {
		current[port] = true
		if s.conflicts[port] {
			continue
		}
		claimants := make([]string, 0, len(rs))
		for _, rt := range rs {
			claimants = append(claimants, rt.workload.String()+" service "+rt.service)
		}
		slices.Sort(claimants)
		logger.Error(errors.New("doorman port claimed more than once"), "not serving the port",
			"port", port, "claimants", strings.Join(claimants, ", "))
	}
	s.conflicts = current
	opmetrics.DoormanPortConflicts.Set(float64(len(current)))
}

func (s *Server) accept(ctx context.Context, port int32, l net.Listener) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// Out of file descriptors, most likely; held connections
			// closing will free some.
			time.Sleep(acceptRetry)
			continue
		}
		s.mu.Lock()
		rt, ok := s.routes[port]
		if ok {
			s.conns[conn] = struct{}{}
		}
		s.mu.Unlock()
		if !ok {
			_ = conn.Close() // the route went between accept and here
			continue
		}
		s.handling.Go(func() {
			defer s.forget(conn)
			s.handle(ctx, port, rt, conn)
		})
	}
}

// track registers a connection to the workload, so shutdown can close it.
// One dialled as shutdown closes everything is closed at once.
func (s *Server) track(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = conn.Close() // the proxy then ends at once
		return
	}
	s.conns[conn] = struct{}{}
}

func (s *Server) forget(conn net.Conn) {
	_ = conn.Close() // may already be closed by the proxy or shutdown
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// currentRoute is the route now on port for the workload a connection was
// accepted for. A port that has gone, or gone to another workload, counts as
// draining: the connection must not wake anything.
func (s *Server) currentRoute(port int32, accepted route) route {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt, ok := s.routes[port]; ok && rt.workload == accepted.workload && rt.service == accepted.service {
		return rt
	}
	if !accepted.draining() {
		accepted.drainUntil = s.now()
	}
	return accepted
}
