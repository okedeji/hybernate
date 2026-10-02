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
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	// backendPollInterval is how often a held connection checks for a Ready
	// pod to pass it to.
	backendPollInterval = 250 * time.Millisecond

	// wakeStampInterval stops a burst of connections from patching the
	// ManagedWorkload once each; one stamp is enough to start the wake.
	wakeStampInterval = 10 * time.Second

	dialTimeout = 5 * time.Second
)

// route is what the doorman knows about one listening port.
type route struct {
	workload types.NamespacedName
	service  string
	portName string
	maxWait  time.Duration
}

// Server listens on every allocated doorman port. It reads routes and the
// workloads' real endpoints from the cache, and wakes a workload by stamping
// its last-activity annotation, the same path a sandbox UI uses.
type Server struct {
	client   client.Client
	recorder events.EventRecorder
	host     string
	now      func() time.Time

	mu        sync.Mutex
	routes    map[int32]route
	listeners map[int32]net.Listener
	conns     map[net.Conn]struct{}
	lastWake  map[types.NamespacedName]time.Time

	wg     sync.WaitGroup
	synced atomic.Bool
}

// NewServer returns a doorman that listens on host, all interfaces if empty.
func NewServer(c client.Client, recorder events.EventRecorder, host string) *Server {
	return &Server{
		client:    c,
		recorder:  recorder,
		host:      host,
		now:       time.Now,
		routes:    map[int32]route{},
		listeners: map[int32]net.Listener{},
		conns:     map[net.Conn]struct{}{},
		lastWake:  map[types.NamespacedName]time.Time{},
	}
}

// Start runs until ctx is done, then closes every listener and connection
// and waits for their goroutines.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("doorman")
	ticker := time.NewTicker(routeSyncInterval)
	defer ticker.Stop()
	defer s.shutdown()

	for {
		if err := s.syncRoutes(ctx); err != nil {
			logger.Error(err, "syncing doorman routes")
		} else {
			s.synced.Store(true)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Ready reports whether routes have been loaded. Until then the doorman pod
// stays out of its Service, so it isn't sent connections it can't route.
func (s *Server) Ready(_ *http.Request) error {
	if !s.synced.Load() {
		return errors.New("doorman routes not loaded yet")
	}
	return nil
}

func (s *Server) shutdown() {
	s.mu.Lock()
	for port, l := range s.listeners {
		_ = l.Close() // closing ends its accept loop; nothing to do if it fails
		delete(s.listeners, port)
	}
	for c := range s.conns {
		_ = c.Close() // ends its proxy copies; the client sees the connection drop
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// syncRoutes opens a listener for every route and closes listeners whose
// route is gone. Closing a listener doesn't affect connections it accepted.
func (s *Server) syncRoutes(ctx context.Context) error {
	var list v1alpha1.ManagedWorkloadList
	if err := s.client.List(ctx, &list); err != nil {
		return fmt.Errorf("listing managed workloads: %w", err)
	}
	routes := map[int32]route{}
	for _, w := range list.Items {
		maxWait := defaultMaxWait
		if w.Spec.Wake != nil && w.Spec.Wake.MaxWait != nil && w.Spec.Wake.MaxWait.Duration > 0 {
			maxWait = w.Spec.Wake.MaxWait.Duration
		}
		for _, r := range w.Status.Doorman {
			routes[r.DoormanPort] = route{
				workload: types.NamespacedName{Namespace: w.Namespace, Name: w.Name},
				service:  r.Service,
				portName: r.PortName,
				maxWait:  maxWait,
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = routes
	for port, l := range s.listeners {
		if _, ok := routes[port]; !ok {
			_ = l.Close() // stops accepting; held connections continue
			delete(s.listeners, port)
		}
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
		s.wg.Go(func() { s.accept(ctx, port, l) })
	}
	return errors.Join(errs...)
}

func (s *Server) accept(ctx context.Context, port int32, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer s.forget(conn)
			s.handle(ctx, port, conn)
		})
	}
}

func (s *Server) forget(conn net.Conn) {
	_ = conn.Close() // may already be closed by the proxy or shutdown
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// handle holds one connection: it wakes the workload, waits for a Ready pod,
// and proxies the connection to it, or closes it once maxWait runs out.
func (s *Server) handle(ctx context.Context, port int32, conn net.Conn) {
	s.mu.Lock()
	rt, ok := s.routes[port]
	s.mu.Unlock()
	if !ok {
		return
	}
	logger := log.FromContext(ctx).WithName("doorman").WithValues(
		"workload", rt.workload.Name, "namespace", rt.workload.Namespace, "service", rt.service)
	ns, name := rt.workload.Namespace, rt.workload.Name

	opmetrics.DoormanHeldConnections.Inc()
	defer opmetrics.DoormanHeldConnections.Dec()
	start := s.now()

	waitCtx, cancel := context.WithTimeout(ctx, rt.maxWait)
	defer cancel()

	if err := s.wake(waitCtx, rt); err != nil {
		logger.Error(err, "waking workload")
	}

	backend, err := s.waitForBackend(waitCtx, rt)
	if err != nil {
		result := "error"
		if errors.Is(err, context.DeadlineExceeded) {
			result = "timeout"
		}
		opmetrics.DoormanWakes.WithLabelValues(ns, name, result).Inc()
		opmetrics.DoormanWaitSeconds.WithLabelValues(result).Observe(s.now().Sub(start).Seconds())
		logger.Info("closing held connection", "result", result, "waited", s.now().Sub(start).Round(time.Millisecond).String())
		return
	}

	upstream, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", backend)
	if err != nil {
		opmetrics.DoormanWakes.WithLabelValues(ns, name, "error").Inc()
		logger.Error(err, "connecting to woken workload", "backend", backend)
		return
	}
	opmetrics.DoormanWakes.WithLabelValues(ns, name, "success").Inc()
	opmetrics.DoormanWaitSeconds.WithLabelValues("success").Observe(s.now().Sub(start).Seconds())

	s.mu.Lock()
	s.conns[upstream] = struct{}{}
	s.mu.Unlock()
	defer s.forget(upstream)
	proxy(conn, upstream)
}

// wake stamps the workload's last-activity annotation, at most once per
// wakeStampInterval, which wakes it through the operator's annotation path.
func (s *Server) wake(ctx context.Context, rt route) error {
	now := s.now()
	s.mu.Lock()
	if last, ok := s.lastWake[rt.workload]; ok && now.Sub(last) < wakeStampInterval {
		s.mu.Unlock()
		return nil
	}
	s.lastWake[rt.workload] = now
	s.mu.Unlock()

	var w v1alpha1.ManagedWorkload
	if err := s.client.Get(ctx, rt.workload, &w); err != nil {
		return fmt.Errorf("getting managed workload: %w", err)
	}
	patch := client.MergeFrom(w.DeepCopy())
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	w.Annotations[v1alpha1.AnnotationLastActivity] = now.UTC().Format(time.RFC3339)
	if err := s.client.Patch(ctx, &w, patch); err != nil {
		return fmt.Errorf("stamping last activity: %w", err)
	}
	if s.recorder != nil {
		s.recorder.Eventf(&w, nil, "Normal", "WokenByRequest", "Wake",
			"request on Service %s, waking", rt.service)
	}
	return nil
}

// waitForBackend polls until the Service has a Ready pod, and returns its
// address. It reads the Service's real EndpointSlices, never the doorman's,
// so it can't route a connection back to itself.
func (s *Server) waitForBackend(ctx context.Context, rt route) (string, error) {
	ticker := time.NewTicker(backendPollInterval)
	defer ticker.Stop()
	for {
		addr, err := s.readyBackend(ctx, rt)
		if err != nil {
			return "", err
		}
		if addr != "" {
			return addr, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) readyBackend(ctx context.Context, rt route) (string, error) {
	var list discoveryv1.EndpointSliceList
	if err := s.client.List(ctx, &list, client.InNamespace(rt.workload.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: rt.service}); err != nil {
		return "", fmt.Errorf("listing endpoints for service %s: %w", rt.service, err)
	}
	for _, slice := range list.Items {
		if slice.Labels[discoveryv1.LabelManagedBy] == ManagedBy {
			continue
		}
		port, ok := slicePort(slice, rt.portName)
		if !ok {
			continue
		}
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if len(ep.Addresses) > 0 {
				return net.JoinHostPort(ep.Addresses[0], strconv.Itoa(int(port))), nil
			}
		}
	}
	return "", nil
}

func slicePort(slice discoveryv1.EndpointSlice, name string) (int32, bool) {
	for _, p := range slice.Ports {
		if p.Port == nil {
			continue
		}
		if (p.Name == nil && name == "") || (p.Name != nil && *p.Name == name) {
			return *p.Port, true
		}
	}
	return 0, false
}

// proxy copies in both directions until either side is done. Closing the
// write half on EOF lets the other direction finish its response.
func proxy(downstream, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src) // an error just ends this direction
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite() // signals EOF; nothing to do if already closed
		}
	}
	go pipe(upstream, downstream)
	go pipe(downstream, upstream)
	wg.Wait()
}
