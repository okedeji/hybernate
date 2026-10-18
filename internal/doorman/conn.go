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
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

const (
	dialTimeout = 5 * time.Second

	// silentWake is how long a caller may stay connected without sending
	// anything before it's taken for a client of a protocol where the
	// server speaks first, such as MySQL, and the workload is woken. A TCP
	// health check closes well before it.
	silentWake = 3 * time.Second

	// headTimeout bounds how long the doorman waits for the rest of an HTTP
	// request's headers once the first bytes have arrived.
	headTimeout = 500 * time.Millisecond

	// maxPeek is enough for nearly any request line and headers. Longer
	// ones are judged by what fits (see parseRequest).
	maxPeek = 16 << 10

	// lingerTimeout and maxLinger bound how long, and how much, the doorman
	// reads from a caller it has answered, so that closing doesn't reset
	// the connection before the caller reads the answer.
	lingerTimeout = time.Second
	maxLinger     = 256 << 10

	wakeRetryInterval = 2 * time.Second
	dialRetryInterval = time.Second

	// clientBufferSize is what a held connection reads its caller with. It's
	// small because thousands of connections can be held at once.
	clientBufferSize = 8 << 10

	// proxyBufferSize is what each direction of a proxied connection copies
	// through, for the same reason.
	proxyBufferSize = 8 << 10

	// proxyIdleTimeout closes a proxied connection that has carried nothing
	// either way for this long. One whose client or server pings, as
	// websockets and database pools do, stays open; one whose caller
	// vanished without closing it doesn't hold its place for ever.
	proxyIdleTimeout = 10 * time.Minute

	// drainedProxyLifetime is how long a proxied connection lasts once its
	// route is draining. The workload is awake by then and its Service
	// sends new connections to its pods directly, so closing it moves a
	// long-lived caller there.
	drainedProxyLifetime = 5 * time.Minute

	proxyCheckInterval = 30 * time.Second
)

// The results a connection the doorman accepted ends with, as counted by
// hybernate_doorman_wakes_total.
const (
	resultSuccess  = "success"
	resultPage     = "page"
	resultTimeout  = "timeout"
	resultError    = "error"
	resultCanceled = "canceled"
	resultIgnored  = "ignored"
	resultLimited  = "limited"
	resultRefused  = "refused"
)

// handle holds one connection: it wakes the workload, waits for a Ready pod,
// and passes the connection to it, or closes it once maxWait runs out.
func (s *Server) handle(ctx context.Context, port int32, rt route, conn net.Conn) {
	start := s.now()
	from := remoteAddr(conn)
	logger := log.FromContext(ctx).WithName("doorman").WithValues(
		"workload", rt.workload.Name, "namespace", rt.workload.Namespace, "service", rt.service)

	if !s.admission.hold(from, rt.workload, start) {
		s.record(rt, resultLimited, start)
		logger.V(1).Info("refusing connection: too many held", "source", from.String())
		return
	}
	opmetrics.DoormanHeldConnections.Inc()
	held := true
	unhold := func() {
		if held {
			held = false
			s.admission.release(from, rt.workload, s.now())
			opmetrics.DoormanHeldConnections.Dec()
		}
	}
	defer unhold()

	h := &heldConn{Server: s, port: port, route: rt, from: from, conn: conn}
	defer h.close()
	upstream, result := h.hold(ctx)
	s.record(rt, result, start)
	if upstream == nil {
		if result == resultTimeout || result == resultError {
			waited := s.now().Sub(start)
			logger.Info("closing held connection", "result", result, "waited", waited.Round(time.Millisecond).String())
			s.warnUnserved(ctx, rt, result, waited)
		}
		return
	}

	defer s.admission.unproxy(from, s.now())
	unhold()
	opmetrics.DoormanProxiedConnections.Inc()
	defer opmetrics.DoormanProxiedConnections.Dec()
	s.track(upstream)
	defer s.forget(upstream)
	s.proxy(port, rt, conn, h.reader(), upstream)
}

func (s *Server) record(rt route, result string, start time.Time) {
	s.mu.Lock()
	s.metered[rt.workload] = true
	s.mu.Unlock()
	opmetrics.DoormanWakes.WithLabelValues(rt.workload.Namespace, rt.workload.Name, result).Inc()
	opmetrics.DoormanWaitSeconds.WithLabelValues(result).Observe(s.now().Sub(start).Seconds())
}

// heldConn is a connection from the moment it's accepted until it's passed
// to the workload or closed.
type heldConn struct {
	*Server
	port  int32
	route route
	from  netip.Addr
	conn  net.Conn

	// head is what the caller sent before the doorman decided what to do
	// with the connection; it's passed on first.
	head []byte

	// stream carries the rest of what the caller sends, read in the
	// background while the connection is held so that a caller who leaves
	// frees its place at once.
	stream *clientStream
}

// hold decides what becomes of the connection, and returns the connection
// to the workload when it's to be passed through, with a proxied place
// taken for it.
func (h *heldConn) hold(ctx context.Context) (net.Conn, string) {
	waitCtx, cancel := context.WithTimeout(ctx, h.route.maxWait)
	defer cancel()
	key := h.route.backendKey()

	if h.route.draining() {
		return h.passThrough(waitCtx)
	}

	// A workload with a Ready pod is awake, or its Service is served by
	// another workload's pods: the connection goes straight through, a
	// health check's included, since what it checks is up. That's decided
	// before reading anything, since a client of a protocol where the
	// server speaks first, such as MySQL, sends nothing until it has.
	if addrs, _, err := h.backends.get(waitCtx, key); err == nil && len(addrs) > 0 {
		if upstream, _ := h.dial(waitCtx, addrs); upstream != nil {
			return upstream, resultSuccess
		}
	}

	head, state := readHead(h.conn, h.silent)
	if state == headClosed {
		return nil, resultIgnored
	}
	h.head = head
	req, isHTTP := parseRequest(head)

	if isHTTP && isHealthCheck(req, h.healthCheckAgents) {
		_ = h.reply(writeUnavailable) // the caller may already have gone
		return nil, resultIgnored
	}
	if isHTTP && isHiddenFileRequest(req) {
		_ = h.reply(writeNotFound) // the caller may already have gone
		return nil, resultIgnored
	}

	// A browser gets the page at once, while the wake goes on without it;
	// one that fails, or is over the limits, is asked for again by the
	// page's next reload. A held connection retries its own.
	if isHTTP && h.route.page && isPageLoad(req) {
		h.handling.Go(func() {
			wakeCtx, cancel := context.WithTimeout(ctx, apiTimeout)
			defer cancel()
			_ = h.requestWake(ctx, wakeCtx, h.route, h.from) // logged by requestWake
		})
		h.servePage(waitCtx, req)
		return nil, resultPage
	}
	wakeErr := h.requestWake(ctx, waitCtx, h.route, h.from)
	h.streamClient()
	return h.await(ctx, waitCtx, wakeErr)
}

// await waits for a Ready pod to pass the connection to, retrying a wake or
// a dial that failed, until waitCtx ends, the caller leaves, or the route
// starts draining with nothing Ready.
func (h *heldConn) await(ctx, waitCtx context.Context, wakeErr error) (net.Conn, string) {
	key := h.route.backendKey()
	lastWake := h.now()
	var dialFailed, proxyLimited bool
	for {
		current := h.currentRoute(h.port, h.route)
		addrs, changed, err := h.backends.get(waitCtx, key)
		if len(addrs) > 0 {
			upstream, limited := h.dial(waitCtx, addrs)
			if upstream != nil {
				return upstream, resultSuccess
			}
			proxyLimited = limited
			dialFailed = !limited
		} else if current.draining() && err == nil {
			return nil, resultRefused
		}
		if wakeErr != nil && !current.draining() && h.now().Sub(lastWake) >= wakeRetryInterval {
			lastWake = h.now()
			wakeErr = h.requestWake(ctx, waitCtx, h.route, h.from)
		}

		retry := time.NewTimer(dialRetryInterval)
		if !dialFailed && !proxyLimited && wakeErr == nil && err == nil {
			retry.Stop()
		}
		select {
		case <-changed:
			retry.Stop()
		case <-retry.C:
		case <-h.stream.gone:
			retry.Stop()
			return nil, resultCanceled
		case <-waitCtx.Done():
			retry.Stop()
			switch {
			case ctx.Err() != nil:
				return nil, resultCanceled
			case errors.Is(wakeErr, errWakeLimited) || proxyLimited:
				return nil, resultLimited
			case dialFailed || wakeErr != nil:
				return nil, resultError
			default:
				return nil, resultTimeout
			}
		}
	}
}

// passThrough handles a connection to a draining route: it goes to a Ready
// pod if there is one, or is refused at once, as it would be without the
// doorman. It never wakes the workload, which has stopped being routed for
// a reason: it's awake, or no longer wakes on request.
func (h *heldConn) passThrough(ctx context.Context) (net.Conn, string) {
	addrs, _, err := h.backends.get(ctx, h.route.backendKey())
	if err != nil || len(addrs) == 0 {
		return nil, resultRefused
	}
	upstream, limited := h.dial(ctx, addrs)
	switch {
	case upstream != nil:
		return upstream, resultSuccess
	case limited:
		return nil, resultLimited
	default:
		return nil, resultError
	}
}

// dial connects to one of addrs, once a proxied place is free for the
// connection. It reports whether none was.
func (h *heldConn) dial(ctx context.Context, addrs []string) (net.Conn, bool) {
	if !h.admission.proxy(h.from, h.now()) {
		return nil, true
	}
	upstream := dialAny(ctx, addrs)
	if upstream == nil {
		h.admission.unproxy(h.from, h.now())
	}
	return upstream, false
}

// streamClient starts reading the caller in the background.
func (h *heldConn) streamClient() {
	h.stream = newClientStream(h.conn, h.admission)
	h.handling.Go(h.stream.run)
}

// reader is everything the caller sends, in order.
func (h *heldConn) reader() io.Reader {
	var rest io.Reader = h.conn
	if h.stream != nil {
		rest = h.stream
	}
	return io.MultiReader(bytes.NewReader(h.head), rest)
}

func (h *heldConn) close() {
	if h.stream != nil {
		h.stream.close()
	}
}

// servePage answers a browser with the waking-up page.
func (h *heldConn) servePage(ctx context.Context, req *http.Request) {
	data := pageData{Address: pageAddress(req)}
	getCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	var w v1alpha1.ManagedWorkload
	if err := h.client.Get(getCtx, h.route.workload, &w); err == nil &&
		w.Status.Phase == v1alpha1.PhaseResuming && w.Status.LastTransitionTime != nil {
		data.Elapsed = sinceLabel(h.now().Sub(w.Status.LastTransitionTime.Time))
	}
	if err := h.reply(func(w io.Writer) error { return writePage(w, req, data) }); err != nil {
		log.FromContext(ctx).V(1).Info("writing waking-up page", "error", err.Error(),
			"workload", h.route.workload.Name, "namespace", h.route.workload.Namespace)
	}
}

// reply answers the caller and closes the connection. What it sent and the
// doorman didn't read, such as the rest of headers past maxPeek, is read and
// dropped first: closing with it unread resets the connection, and a reset
// can discard the answer before the caller reads it.
func (h *heldConn) reply(write func(io.Writer) error) error {
	_ = h.conn.SetWriteDeadline(time.Now().Add(dialTimeout)) // a failure just means no deadline
	err := write(h.conn)
	closeWrite(h.conn)
	_ = h.conn.SetReadDeadline(time.Now().Add(lingerTimeout)) // a failure just means no deadline
	_, _ = io.CopyN(io.Discard, h.conn, maxLinger)            // ends at the caller's close or the deadline
	return err
}

// headState is how a caller began.
type headState int

const (
	headSent headState = iota
	headSilent
	headClosed
)

// readHead reads what the caller sends first: the first bytes, and for
// HTTP, the rest of the request's headers. A caller that sends nothing has
// either closed the connection or waited out silent.
func readHead(conn net.Conn, silent time.Duration) ([]byte, headState) {
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }() // a failure just leaves the deadline
	buf := make([]byte, 4<<10)
	_ = conn.SetReadDeadline(time.Now().Add(silent)) // a failure just means no deadline
	n, err := conn.Read(buf)
	if n == 0 {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, headSilent
		}
		return nil, headClosed
	}
	head := buf[:n]
	_ = conn.SetReadDeadline(time.Now().Add(headTimeout)) // a failure just means no deadline
	for startsMethod(head) {
		n, err := conn.Read(buf[len(head):])
		head = buf[:len(head)+n]
		if err != nil {
			break
		}
	}
	if !looksLikeHTTP(head) {
		return head, headSent
	}
	for len(head) < maxPeek && !headersEnded(head) {
		if len(head) == cap(head) {
			head = append(head, make([]byte, 4<<10)...)[:len(head)]
		}
		n, err := conn.Read(head[len(head):min(cap(head), maxPeek)])
		head = head[:len(head)+n]
		if err != nil {
			break
		}
	}
	return head, headSent
}

// dialAny connects to one of addrs, trying them in random order so held
// connections spread over the workload's pods.
func dialAny(ctx context.Context, addrs []string) net.Conn {
	dialer := net.Dialer{Timeout: dialTimeout}
	for _, i := range rand.Perm(len(addrs)) {
		conn, err := dialer.DialContext(ctx, "tcp", addrs[i])
		if err == nil {
			return conn
		}
	}
	return nil
}

func remoteAddr(conn net.Conn) netip.Addr {
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return tcp.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}

// proxy copies in both directions until both sides are done, or until
// closeIdle ends the connection. Closing the write half on EOF lets the
// other direction finish its response.
func (s *Server) proxy(port int32, accepted route, downstream net.Conn, fromDownstream io.Reader, upstream net.Conn) {
	var active atomic.Int64
	active.Store(s.now().UnixNano())
	var copies sync.WaitGroup
	copies.Go(func() {
		s.copyActive(upstream, fromDownstream, &active)
		closeWrite(upstream)
	})
	copies.Go(func() {
		s.copyActive(downstream, upstream, &active)
		closeWrite(downstream)
	})

	done := make(chan struct{})
	var watch sync.WaitGroup
	watch.Go(func() { s.closeIdle(done, port, accepted, &active, downstream, upstream) })
	copies.Wait()
	close(done)
	watch.Wait()
}

// copyActive copies src to dst through a buffer of proxyBufferSize,
// noting when bytes last moved. Hiding the connections' ReadFrom and
// WriteTo keeps io from allocating its own, larger buffer.
func (s *Server) copyActive(dst io.Writer, src io.Reader, active *atomic.Int64) {
	w := activityWriter{w: dst, active: active, now: s.now}
	_, _ = io.CopyBuffer(w, struct{ io.Reader }{src}, make([]byte, proxyBufferSize)) // an error just ends this direction
}

type activityWriter struct {
	w      io.Writer
	active *atomic.Int64
	now    func() time.Time
}

func (a activityWriter) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	if n > 0 {
		a.active.Store(a.now().UnixNano())
	}
	return n, err
}

// closeIdle closes a proxied connection that has been idle for proxyIdle,
// or has outlived its route's drain by drainedLifetime, until done.
func (s *Server) closeIdle(done <-chan struct{}, port int32, accepted route, active *atomic.Int64, conns ...net.Conn) {
	ticker := time.NewTicker(s.proxyCheck)
	defer ticker.Stop()
	var drainingSince time.Time
	if accepted.draining() {
		drainingSince = s.now()
	}
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		now := s.now()
		if drainingSince.IsZero() && s.currentRoute(port, accepted).draining() {
			drainingSince = now
		}
		idle := now.Sub(time.Unix(0, active.Load())) >= s.proxyIdle
		drained := !drainingSince.IsZero() && now.Sub(drainingSince) >= s.drainedLifetime
		if idle || drained {
			for _, c := range conns {
				_ = c.Close() // ends both copies; the caller sees the connection drop
			}
			return
		}
	}
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite() // signals EOF; nothing to do if already closed
	}
}
