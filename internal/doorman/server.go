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
	// routes, or without reaching the API server. Past it, it's serving
	// routes that may be gone or wrong: its cache answers from memory
	// however long its watches have been failing.
	readyStaleness = 30 * time.Second

	// apiCheckInterval and apiCheckTimeout pace the checks that the API
	// server, and so the doorman's cache, is reachable.
	apiCheckInterval = 10 * time.Second
	apiCheckTimeout  = 5 * time.Second

	// listenGrace is how long a route's port may fail to listen, retried
	// each sync, before the doorman reports itself not Ready, so the
	// operator routes paused workloads to doorman pods that can serve them.
	listenGrace = 30 * time.Second

	// shutdownGrace is how long a stopping doorman keeps its connections.
	// It fits in the manager's 30s graceful shutdown and the pod's
	// termination grace period.
	shutdownGrace = 25 * time.Second

	// shutdownAccept is how much of shutdownGrace a stopping doorman goes
	// on accepting connections. Only the operator moves paused workloads'
	// EndpointSlices to other doorman pods, and when both are upgraded
	// together, the new operator takes a while to win the lease and catch
	// up; until it has, connections still arrive here, and refusing them
	// would fail requests the doorman exists to hold.
	shutdownAccept = 15 * time.Second

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

	// APIReader reads what the doorman doesn't cache: the pods its
	// endpoints name, to check each is the pod it says. Defaults to the
	// client.
	APIReader client.Reader

	// HealthCheckAgents are User-Agent prefixes of health checkers and
	// scrapers to recognise besides the built-in ones, such as an in-house
	// uptime monitor's, so their requests don't wake workloads.
	HealthCheckAgents []string

	// CheckAPI, when set, reports whether the API server answers. The
	// doorman isn't Ready while it hasn't for readyStaleness.
	CheckAPI func(ctx context.Context) error
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

// sameAs reports whether two routes send a port's traffic to the same
// place. Anything else a route carries can change without a drain.
func (r route) sameAs(o route) bool {
	return r.workload == o.workload && r.service == o.service && r.portName == o.portName
}

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
	acceptFor time.Duration
	pollEvery time.Duration
	silent    time.Duration

	proxyIdle       time.Duration
	drainedLifetime time.Duration
	proxyCheck      time.Duration

	healthCheckAgents []string
	checkAPI          func(ctx context.Context) error

	backends  *backends
	admission *admission

	mu     sync.Mutex
	routes map[int32]route
	// drains are the routes that have left status, or been replaced, by
	// port, kept until their drain ends whatever claims the port since.
	drains    map[int32]route
	conflicts map[int32]bool
	listeners map[int32]net.Listener
	// listenFailed is when each routed port without a listener first
	// failed to get one.
	listenFailed map[int32]time.Time
	conns        map[net.Conn]struct{}
	wakeCalls    map[types.NamespacedName]*wakeCall
	stamped      map[types.NamespacedName]stampRecord
	lastWarn     map[types.NamespacedName]time.Time
	metered      map[types.NamespacedName]bool
	closed       bool

	accepting sync.WaitGroup
	handling  sync.WaitGroup
	lastSync  atomic.Int64
	lastAPI   atomic.Int64
	stopping  atomic.Bool
}

// NewServer returns a doorman that reads and writes through c.
func NewServer(c client.Client, recorder events.EventRecorder, opts Options) *Server {
	pods := opts.APIReader
	if pods == nil {
		pods = c
	}
	return &Server{
		client:    c,
		recorder:  recorder,
		host:      opts.Host,
		informers: opts.Informers,
		now:       time.Now,
		grace:     shutdownGrace,
		acceptFor: shutdownAccept,
		pollEvery: backendPollInterval,
		silent:    silentWake,
		backends:  newBackends(c, pods),

		proxyIdle:       proxyIdleTimeout,
		drainedLifetime: drainedProxyLifetime,
		proxyCheck:      proxyCheckInterval,

		healthCheckAgents: healthCheckAgents(opts.HealthCheckAgents),
		checkAPI:          opts.CheckAPI,
		admission:         newAdmission(defaultLimits),
		routes:            map[int32]route{},
		drains:            map[int32]route{},
		conflicts:         map[int32]bool{},
		listeners:         map[int32]net.Listener{},
		listenFailed:      map[int32]time.Time{},
		conns:             map[net.Conn]struct{}{},
		wakeCalls:         map[types.NamespacedName]*wakeCall{},
		stamped:           map[types.NamespacedName]stampRecord{},
		lastWarn:          map[types.NamespacedName]time.Time{},
		metered:           map[types.NamespacedName]bool{},
	}
}

// healthCheckAgents is the built-in User-Agent prefixes and the extra ones
// asked for, without blanks, which would match every request.
func healthCheckAgents(extra []string) []string {
	agents := slices.Clone(defaultHealthCheckAgents)
	for _, agent := range extra {
		if agent = strings.TrimSpace(agent); agent != "" {
			agents = append(agents, agent)
		}
	}
	return agents
}

// Start serves until ctx is done. Then it reports itself not Ready, so the
// operator moves traffic to other doorman pods, and drains: it goes on
// serving while traffic may still arrive, stops accepting connections, gives
// those it has the rest of the grace period to finish, closes what's left,
// and waits for their goroutines.
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

	var checking sync.WaitGroup
	if s.checkAPI != nil {
		checking.Go(func() { s.watchAPI(ctx) })
	}
	s.sync(ctx, connCtx)
	s.serve(ctx, connCtx, ctx.Done(), syncTicker.C, pollTicker.C)
	checking.Wait()

	s.stopping.Store(true)
	grace := s.grace
	if s.listening() {
		drainCtx := context.WithoutCancel(ctx)
		accepting, stopAccepting := context.WithTimeout(drainCtx, s.acceptFor)
		defer stopAccepting()
		s.serve(drainCtx, connCtx, accepting.Done(), syncTicker.C, pollTicker.C)
		grace -= s.acceptFor
	}
	s.shutdown(cancelConns, grace)
	return nil
}

func (s *Server) listening() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listeners) > 0
}

// serve keeps routes and backends current until stop.
func (s *Server) serve(ctx, connCtx context.Context, stop <-chan struct{}, syncs, polls <-chan time.Time) {
	for {
		select {
		case <-stop:
			return
		case <-polls:
			s.backends.changedAll()
		case <-syncs:
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

// watchAPI checks the API server every apiCheckInterval until ctx is done.
func (s *Server) watchAPI(ctx context.Context) {
	ticker := time.NewTicker(apiCheckInterval)
	defer ticker.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, apiCheckTimeout)
		err := s.checkAPI(checkCtx)
		cancel()
		if err == nil {
			s.lastAPI.Store(s.now().UnixNano())
		} else if ctx.Err() == nil {
			log.FromContext(ctx).WithName("doorman").Error(err, "checking the API server")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Ready reports whether the doorman's routes are current and it serves
// them all. A doorman that can't load them, can't reach the API server its
// cache follows, can't listen on a route's port, or is shutting down,
// leaves its Service, and the operator stops sending paused workloads'
// traffic to it.
func (s *Server) Ready(_ *http.Request) error {
	if s.stopping.Load() {
		return errors.New("doorman is shutting down")
	}
	now := s.now()
	last := s.lastSync.Load()
	if last == 0 {
		return errors.New("doorman routes not loaded yet")
	}
	if age := now.Sub(time.Unix(0, last)); age > readyStaleness {
		return fmt.Errorf("doorman routes last loaded %s ago", age.Round(time.Second))
	}
	if s.checkAPI != nil {
		reached := s.lastAPI.Load()
		if reached == 0 {
			return errors.New("API server not reached yet")
		}
		if age := now.Sub(time.Unix(0, reached)); age > readyStaleness {
			return fmt.Errorf("API server last reached %s ago, so routes may be stale", age.Round(time.Second))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for port, since := range s.listenFailed {
		if age := now.Sub(since); age > listenGrace {
			return fmt.Errorf("can't listen on doorman port %d for %s", port, age.Round(time.Second))
		}
	}
	return nil
}

// shutdown stops accepting connections, waits up to grace for those it has,
// then closes the rest.
func (s *Server) shutdown(cancelConns context.CancelFunc, grace time.Duration) {
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
	timer := time.NewTimer(max(grace, 0))
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
// one would send another workload's traffic to it. A route that leaves,
// or whose port goes to another Service, drains for RouteDrain, and until
// it's over, it counts as one of the port's claims.
func (s *Server) syncRoutes(ctx, connCtx context.Context) error {
	var list v1alpha1.ManagedWorkloadList
	if err := s.client.List(ctx, &list); err != nil {
		return fmt.Errorf("listing managed workloads: %w", err)
	}
	now := s.now()
	s.lastSync.Store(now.UnixNano())

	claims := map[int32][]route{}
	existing := make(map[types.NamespacedName]bool, len(list.Items))
	for _, w := range list.Items {
		existing[types.NamespacedName{Namespace: w.Namespace, Name: w.Name}] = true
		maxWait := defaultMaxWait
		if w.Spec.Wake != nil && w.Spec.Wake.MaxWait != nil && w.Spec.Wake.MaxWait.Duration > 0 {
			maxWait = min(w.Spec.Wake.MaxWait.Duration, v1alpha1.WakeMaxWaitLimit)
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
	s.forgetDeleted(existing)
	routes, conflicts := s.resolve(claims, now)
	s.reportConflicts(ctx, conflicts)
	s.routes = routes

	keep := map[serviceRef]bool{}
	for _, rt := range routes {
		keep[rt.backendKey().service] = true
	}
	s.backends.retain(keep)
	return s.syncListeners(connCtx, now)
}

// resolve drains the routes claims no longer has, and returns the route to
// serve on each port and the ports claimed more than once. s.mu must be
// held.
func (s *Server) resolve(claims map[int32][]route, now time.Time) (map[int32]route, map[int32][]route) {
	for port, old := range s.routes {
		if old.draining() {
			continue
		}
		if rs := claims[port]; len(rs) == 1 && rs[0].sameAs(old) {
			continue
		}
		old.drainUntil = now.Add(RouteDrain)
		s.drains[port] = old
		s.backends.changed(old.backendKey().service)
	}
	for port, d := range s.drains {
		rs := claims[port]
		if !now.Before(d.drainUntil) || (len(rs) == 1 && rs[0].sameAs(d)) {
			delete(s.drains, port)
		}
	}

	routes := map[int32]route{}
	conflicts := map[int32][]route{}
	for port, rs := range claims {
		if d, ok := s.drains[port]; ok {
			rs = append([]route{d}, rs...)
		}
		if len(rs) > 1 {
			conflicts[port] = rs
			continue
		}
		routes[port] = rs[0]
	}
	for port, d := range s.drains {
		if _, claimed := claims[port]; !claimed {
			routes[port] = d
		}
	}
	return routes, conflicts
}

// syncListeners matches the listeners to s.routes. s.mu must be held.
func (s *Server) syncListeners(connCtx context.Context, now time.Time) error {
	routes := s.routes
	for port, l := range s.listeners {
		if _, ok := routes[port]; !ok {
			_ = l.Close() // stops accepting; held connections continue
			delete(s.listeners, port)
		}
	}
	for port := range s.listenFailed {
		if _, ok := routes[port]; !ok {
			delete(s.listenFailed, port)
		}
	}
	defer func() { opmetrics.DoormanListenFailures.Set(float64(len(s.listenFailed))) }()
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
			if _, ok := s.listenFailed[port]; !ok {
				s.listenFailed[port] = now
			}
			errs = append(errs, fmt.Errorf("listening on doorman port %d: %w", port, err))
			continue
		}
		delete(s.listenFailed, port)
		s.listeners[port] = l
		s.accepting.Go(func() { s.accept(connCtx, port, l) })
	}
	return errors.Join(errs...)
}

// forgetDeleted drops the series of workloads that no longer exist, as the
// operator drops its own, and what this replica remembers about them. s.mu
// must be held.
func (s *Server) forgetDeleted(existing map[types.NamespacedName]bool) {
	for w := range s.metered {
		if !existing[w] {
			opmetrics.DeleteWorkload(w.Namespace, w.Name)
			delete(s.metered, w)
		}
	}
	for w := range s.stamped {
		if !existing[w] {
			delete(s.stamped, w)
		}
	}
	for w := range s.lastWarn {
		if !existing[w] {
			delete(s.lastWarn, w)
		}
	}
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
			claimant := rt.workload.String() + " service " + rt.service
			if rt.draining() {
				claimant += " (draining)"
			}
			claimants = append(claimants, claimant)
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
	if rt, ok := s.routes[port]; ok && rt.sameAs(accepted) {
		return rt
	}
	if !accepted.draining() {
		accepted.drainUntil = s.now()
	}
	return accepted
}
