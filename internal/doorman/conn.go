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

	// maxPeek is enough for any browser's request line and headers.
	maxPeek = 16 << 10

	wakeRetryInterval = 2 * time.Second
	dialRetryInterval = time.Second

	// clientBufferSize is what a held connection reads its caller with. It's
	// small because thousands of connections can be held at once.
	clientBufferSize = 8 << 10
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

	if !s.admission.hold(from, start) {
		s.record(rt, resultLimited, start)
		logger.V(1).Info("refusing connection: too many held", "source", from.String())
		return
	}
	opmetrics.DoormanHeldConnections.Inc()
	held := true
	unhold := func() {
		if held {
			held = false
			s.admission.release(from, s.now())
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

	unhold()
	opmetrics.DoormanProxiedConnections.Inc()
	defer opmetrics.DoormanProxiedConnections.Dec()
	s.track(upstream)
	defer s.forget(upstream)
	proxy(conn, h.reader(), upstream)
}

func (s *Server) record(rt route, result string, start time.Time) {
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
// to the workload when it's to be passed through.
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
		if upstream := dialAny(waitCtx, addrs); upstream != nil {
			return upstream, resultSuccess
		}
	}

	head, state := readHead(h.conn, h.silent)
	if state == headClosed {
		return nil, resultIgnored
	}
	h.head = head
	req, isHTTP := parseRequest(head)

	if isHTTP && isHealthCheck(req) {
		_ = h.conn.SetWriteDeadline(time.Now().Add(dialTimeout)) // a failure just means no deadline
		_ = writeUnavailable(h.conn)                             // the caller may already have gone
		return nil, resultIgnored
	}

	wakeErr := h.requestWake(waitCtx, h.route, h.from)
	if errors.Is(wakeErr, errWakeLimited) {
		return nil, resultLimited
	}
	if isHTTP && h.route.page && isPageLoad(req) {
		h.servePage(waitCtx, req)
		return nil, resultPage
	}
	h.streamClient()
	return h.await(ctx, waitCtx, wakeErr)
}

// await waits for a Ready pod to pass the connection to, retrying a wake or
// a dial that failed, until waitCtx ends, the caller leaves, or the route
// starts draining with nothing Ready.
func (h *heldConn) await(ctx, waitCtx context.Context, wakeErr error) (net.Conn, string) {
	key := h.route.backendKey()
	lastWake := h.now()
	var dialFailed bool
	for {
		current := h.currentRoute(h.port, h.route)
		addrs, changed, err := h.backends.get(waitCtx, key)
		if len(addrs) > 0 {
			if upstream := dialAny(waitCtx, addrs); upstream != nil {
				return upstream, resultSuccess
			}
			dialFailed = true
		} else if current.draining() && err == nil {
			return nil, resultRefused
		}
		if wakeErr != nil && !current.draining() && h.now().Sub(lastWake) >= wakeRetryInterval {
			lastWake = h.now()
			if wakeErr = h.requestWake(waitCtx, h.route, h.from); errors.Is(wakeErr, errWakeLimited) {
				return nil, resultLimited
			}
		}

		retry := time.NewTimer(dialRetryInterval)
		if !dialFailed && wakeErr == nil && err == nil {
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
	if upstream := dialAny(ctx, addrs); upstream != nil {
		return upstream, resultSuccess
	}
	return nil, resultError
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
	_ = h.conn.SetWriteDeadline(time.Now().Add(dialTimeout)) // a failure just means no deadline
	if err := writePage(h.conn, req, data); err != nil {
		log.FromContext(ctx).V(1).Info("writing waking-up page", "error", err.Error(),
			"workload", h.route.workload.Name, "namespace", h.route.workload.Namespace)
	}
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
	if !looksLikeHTTP(head) {
		return head, headSent
	}
	_ = conn.SetReadDeadline(time.Now().Add(headTimeout)) // a failure just means no deadline
	for len(head) < maxPeek && !bytes.Contains(head, []byte("\r\n\r\n")) {
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

// proxy copies in both directions until both sides are done. Closing the
// write half on EOF lets the other direction finish its response.
func proxy(downstream net.Conn, fromDownstream io.Reader, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(upstream, fromDownstream) // an error just ends this direction
		closeWrite(upstream)
	})
	wg.Go(func() {
		_, _ = io.Copy(downstream, upstream) // an error just ends this direction
		closeWrite(downstream)
	})
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite() // signals EOF; nothing to do if already closed
	}
}
