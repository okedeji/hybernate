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
	"bytes"
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

	// unservedEventInterval keeps a burst of timed-out connections, all
	// waiting on the same slow wake, to one warning event.
	unservedEventInterval = time.Minute

	dialTimeout = 5 * time.Second

	// peekTimeout bounds how long the doorman waits for a caller's first
	// bytes to tell a browser from other clients. Some protocols wait for
	// the server to speak first and send nothing; they're held as usual.
	// The wake is already underway, so the wait doesn't delay it.
	peekTimeout = 500 * time.Millisecond

	// maxPeek is enough for any browser's request line and headers.
	maxPeek = 16 << 10

	// routeDrain keeps a port open after its workload is Running and its
	// route is gone. Proxies and kube-proxy take a moment to stop sending
	// there; without it, a browser's last refresh of the waking-up page
	// would land on a closed port and show the proxy's error page, which
	// doesn't refresh.
	routeDrain = 30 * time.Second
)

// route is what the doorman knows about one listening port.
type route struct {
	workload types.NamespacedName
	service  string
	portName string
	maxWait  time.Duration
	page     bool

	// drainUntil is set once the route has left status: the workload is
	// awake, so connections are passed straight to it until then.
	drainUntil time.Time
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
	lastWarn  map[types.NamespacedName]time.Time

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
		lastWarn:  map[types.NamespacedName]time.Time{},
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
		page := w.Spec.Wake == nil || w.Spec.Wake.Page == nil || *w.Spec.Wake.Page
		for _, r := range w.Status.Doorman {
			routes[r.DoormanPort] = route{
				workload: types.NamespacedName{Namespace: w.Namespace, Name: w.Name},
				service:  r.Service,
				portName: r.PortName,
				maxWait:  maxWait,
				page:     page,
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for port, old := range s.routes {
		if _, ok := routes[port]; ok {
			continue
		}
		if old.drainUntil.IsZero() {
			old.drainUntil = now.Add(routeDrain)
		}
		if now.Before(old.drainUntil) {
			routes[port] = old
		}
	}
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
	draining := !rt.drainUntil.IsZero()

	opmetrics.DoormanHeldConnections.Inc()
	defer opmetrics.DoormanHeldConnections.Dec()
	start := s.now()

	waitCtx, cancel := context.WithTimeout(ctx, rt.maxWait)
	defer cancel()

	if !draining {
		if err := s.wake(waitCtx, rt); err != nil {
			logger.Error(err, "waking workload")
		}
	}

	var head []byte
	if rt.page && !draining {
		head = peek(conn)
		if req, ok := isPageLoad(head); ok {
			s.servePage(ctx, conn, rt, req)
			opmetrics.DoormanWakes.WithLabelValues(ns, name, "page").Inc()
			opmetrics.DoormanWaitSeconds.WithLabelValues("page").Observe(s.now().Sub(start).Seconds())
			return
		}
	}

	backend, err := s.waitForBackend(waitCtx, rt)
	if err != nil {
		result := "error"
		if errors.Is(err, context.DeadlineExceeded) {
			result = "timeout"
		}
		waited := s.now().Sub(start)
		opmetrics.DoormanWakes.WithLabelValues(ns, name, result).Inc()
		opmetrics.DoormanWaitSeconds.WithLabelValues(result).Observe(waited.Seconds())
		logger.Info("closing held connection", "result", result, "waited", waited.Round(time.Millisecond).String())
		s.warnUnserved(ctx, rt, result, waited)
		return
	}

	upstream, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", backend)
	if err != nil {
		opmetrics.DoormanWakes.WithLabelValues(ns, name, "error").Inc()
		logger.Error(err, "connecting to woken workload", "backend", backend)
		s.warnUnserved(ctx, rt, "error", s.now().Sub(start))
		return
	}
	opmetrics.DoormanWakes.WithLabelValues(ns, name, "success").Inc()
	opmetrics.DoormanWaitSeconds.WithLabelValues("success").Observe(s.now().Sub(start).Seconds())

	s.mu.Lock()
	s.conns[upstream] = struct{}{}
	s.mu.Unlock()
	defer s.forget(upstream)
	if len(head) > 0 {
		if _, err := upstream.Write(head); err != nil {
			logger.Error(err, "passing held bytes to woken workload", "backend", backend)
			return
		}
	}
	proxy(conn, upstream)
}

// peek reads what the caller has sent so far, up to the end of an HTTP
// request's headers, for at most peekTimeout. The bytes are passed on to the
// workload if the connection is held.
func peek(conn net.Conn) []byte {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout)) // a failure just means no deadline
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 0, 4<<10)
	chunk := make([]byte, 4<<10)
	for len(buf) < maxPeek && !bytes.Contains(buf, []byte("\r\n\r\n")) {
		n, err := conn.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// servePage answers a browser with the waking-up page, showing how far the
// wake has got.
func (s *Server) servePage(ctx context.Context, conn net.Conn, rt route, req *http.Request) {
	data := pageData{
		Address:   pageAddress(req, rt.workload.Namespace, rt.service),
		Service:   rt.service,
		Workload:  rt.workload.Name,
		Namespace: rt.workload.Namespace,
	}
	now := s.now()
	var w v1alpha1.ManagedWorkload
	if err := s.client.Get(ctx, rt.workload, &w); err == nil {
		data.Workload = w.Spec.Target.Name
		if w.Status.Phase == v1alpha1.PhaseResuming && w.Status.LastTransitionTime != nil {
			data.Elapsed = sinceLabel(now.Sub(w.Status.LastTransitionTime.Time))
		}
		if p := w.Status.Pause; p != nil && p.PausedAt != nil {
			data.PausedAgo = agoLabel(now.Sub(p.PausedAt.Time))
		}
	}
	if addr, err := s.readyBackend(ctx, rt); err == nil && addr != "" {
		data.Ready = true
	}
	_ = conn.SetWriteDeadline(time.Now().Add(dialTimeout)) // a failure just means no deadline
	if err := writePage(conn, req, data); err != nil {
		log.FromContext(ctx).V(1).Info("writing waking-up page", "error", err.Error(),
			"workload", rt.workload.Name, "namespace", rt.workload.Namespace)
	}
}

// wake stamps the workload's last-request annotation, at most once per
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
	w.Annotations[v1alpha1.AnnotationLastRequest] = now.UTC().Format(time.RFC3339)
	if err := s.client.Patch(ctx, &w, patch); err != nil {
		return fmt.Errorf("stamping last activity: %w", err)
	}
	if s.recorder != nil {
		s.recorder.Eventf(&w, nil, "Normal", "WokenByRequest", "Wake",
			"request on Service %s, waking", rt.service)
	}
	return nil
}

// warnUnserved emits a warning event on the workload for a held connection
// that was closed without reaching it, at most once per unservedEventInterval.
// The metrics count every one; the event is what shows in kubectl describe.
func (s *Server) warnUnserved(ctx context.Context, rt route, result string, waited time.Duration) {
	if s.recorder == nil || ctx.Err() != nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	if last, ok := s.lastWarn[rt.workload]; ok && now.Sub(last) < unservedEventInterval {
		s.mu.Unlock()
		return
	}
	s.lastWarn[rt.workload] = now
	s.mu.Unlock()

	// The connection's context may be what ran out, so the lookup gets its own.
	getCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dialTimeout)
	defer cancel()
	var w v1alpha1.ManagedWorkload
	if err := s.client.Get(getCtx, rt.workload, &w); err != nil {
		log.FromContext(ctx).Error(err, "getting managed workload for event",
			"workload", rt.workload.Name, "namespace", rt.workload.Namespace)
		return
	}
	reason := "the workload wasn't Ready within maxWait; the wake continues"
	if result != "timeout" {
		reason = "the workload couldn't be reached"
	}
	s.recorder.Eventf(&w, nil, "Warning", "RequestNotServed", "Wake",
		"a request on Service %s was closed after %s: %s", rt.service, waited.Round(time.Second), reason)
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
