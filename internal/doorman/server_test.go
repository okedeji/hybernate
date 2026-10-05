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
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

var apiKey = types.NamespacedName{Namespace: "dev", Name: "api"}

// caller is the pod a held request comes from.
var caller = netip.MustParseAddr("10.244.0.17")

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	return scheme
}

// freePort returns a port nothing is listening on, for the doorman to claim.
func freePort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return int32(port)
}

// echoServer is the woken workload: it echoes what it's sent, and counts
// the connections it accepts.
func echoServer(t *testing.T) (int32, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			})
		}
	})
	return int32(l.Addr().(*net.TCPAddr).Port), &accepted
}

func echoPort(t *testing.T) int32 {
	t.Helper()
	port, _ := echoServer(t)
	return port
}

// pausedWorkload is a workload paused an hour ago, with one route.
func pausedWorkload(name string, doormanPort int32, maxWait time.Duration) *v1alpha1.ManagedWorkload {
	pausedAt := metav1.NewTime(time.Now().Add(-time.Hour))
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dev"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
			Wake:   &v1alpha1.WakeSpec{MaxWait: &metav1.Duration{Duration: maxWait}},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase:   v1alpha1.PhasePaused,
			Pause:   &v1alpha1.PauseStatus{PreviousReplicas: 2, PausedAt: &pausedAt, WakeAnnotations: &v1alpha1.WakeAnnotations{}},
			Doorman: []v1alpha1.DoormanRoute{{Service: name, PortName: "http", DoormanPort: doormanPort}},
		},
	}
}

// readySlice is the "api" Service's own slice, as the EndpointSlice
// controller writes it, with one Ready pod on port.
func readySlice(port int32) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-real", Namespace: "dev",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "api",
				discoveryv1.LabelManagedBy:   endpointSliceController,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"127.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "dev", Name: "api-0"},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(port)}},
	}
}

// newTestServer is a doorman over a fake client holding objs. Tests run
// their backends on loopback, which the doorman otherwise never dials.
func newTestServer(t *testing.T, wrap func(client.WithWatch) client.WithWatch, objs ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	var c client.WithWatch
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	if wrap != nil {
		c = wrap(c)
	}
	s := NewServer(c, nil, Options{Host: "127.0.0.1"})
	s.backends.allowed = func(netip.Addr) bool { return true }
	return s, c
}

// run starts s and returns a function that stops it and waits for it.
func run(t *testing.T, s *Server) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	require.Eventually(t, func() bool { return s.Ready(nil) == nil }, 5*time.Second, 10*time.Millisecond)
	return stop
}

func startServer(t *testing.T, objs ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	return startServerWith(t, nil, objs...)
}

func startServerWith(t *testing.T, configure func(*Server), objs ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	s, c := newTestServer(t, nil, objs...)
	if configure != nil {
		configure(s)
	}
	run(t, s)
	return s, c
}

func dial(t *testing.T, port int32) net.Conn {
	t.Helper()
	var conn net.Conn
	require.Eventually(t, func() bool {
		var err error
		conn, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	_, err := conn.Write([]byte(data))
	require.NoError(t, err)
}

// echoed reads back what was sent, as the woken workload echoes it.
func echoed(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	got := make([]byte, len(want))
	_, err := io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

// closedWithin asserts the doorman closes conn, without a reply, within a
// second.
func closedWithin(t *testing.T, conn net.Conn) {
	const d = time.Second
	t.Helper()
	start := time.Now()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(d+2*time.Second)))
	n, err := conn.Read(make([]byte, 1))
	assert.Zero(t, n)
	require.Error(t, err)
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "the connection was still open after %s", d+2*time.Second)
	assert.Less(t, time.Since(start), d+time.Second)
}

func lastRequest(t *testing.T, c client.Client) string {
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	return w.Annotations[v1alpha1.AnnotationLastRequest]
}

func wakes(result string) float64 {
	return testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("dev", "api", result))
}

func TestServer_HoldsWakesAndPassesThrough(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))
	before := wakes(resultSuccess)

	conn := dial(t, port)
	send(t, conn, "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond,
		"the held connection must stamp the request annotation")

	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))

	echoed(t, conn, "hello\n")
	assert.Equal(t, before+1, wakes(resultSuccess))
}

// The doorman never passes a connection to an address it can't trust to be
// one of the Service's pods: its own slice would loop back to it, and a
// hand-written slice could send it anywhere its pod can reach.
func TestBackends_TrustOnlyTheServicesPods(t *testing.T) {
	tests := []struct {
		name  string
		slice func(*discoveryv1.EndpointSlice)
	}{
		{name: "the doorman's own slice", slice: func(s *discoveryv1.EndpointSlice) {
			s.Labels[discoveryv1.LabelManagedBy] = ManagedBy
		}},
		{name: "a hand-written slice", slice: func(s *discoveryv1.EndpointSlice) {
			delete(s.Labels, discoveryv1.LabelManagedBy)
		}},
		{name: "a slice mirrored from Endpoints", slice: func(s *discoveryv1.EndpointSlice) {
			s.Labels[discoveryv1.LabelManagedBy] = "endpointslicemirroring-controller.k8s.io"
		}},
		{name: "a hostname", slice: func(s *discoveryv1.EndpointSlice) {
			s.AddressType = discoveryv1.AddressTypeFQDN
			s.Endpoints[0].Addresses = []string{"metadata.google.internal"}
		}},
		{name: "the node's metadata service", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"169.254.169.254"}
		}},
		{name: "loopback", slice: func(*discoveryv1.EndpointSlice) {}},
		{name: "not a pod", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.9"}
			s.Endpoints[0].TargetRef = nil
		}},
		{name: "a pod still starting", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.9"}
			s.Endpoints[0].Conditions.Ready = ptr.To(false)
		}},
		{name: "a pod shutting down", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.9"}
			s.Endpoints[0].Conditions.Ready = nil
			s.Endpoints[0].Conditions.Terminating = ptr.To(true)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slice := readySlice(8080)
			tt.slice(slice)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice).Build()

			addrs, _, err := newBackends(c).get(context.Background(), backendKey{
				service: serviceRef{namespace: "dev", name: "api"}, portName: "http",
			})

			require.NoError(t, err)
			assert.Empty(t, addrs)
		})
	}
}

func TestBackends_IPv6(t *testing.T) {
	slice := readySlice(8080)
	slice.AddressType = discoveryv1.AddressTypeIPv6
	slice.Endpoints[0].Addresses = []string{"fd00:10:244::7"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice).Build()

	addrs, _, err := newBackends(c).get(context.Background(), backendKey{
		service: serviceRef{namespace: "dev", name: "api"}, portName: "http",
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"[fd00:10:244::7]:8080"}, addrs)
}

func TestServer_ClosesAfterMaxWait(t *testing.T) {
	port := freePort(t)
	startServer(t, pausedWorkload("api", port, 300*time.Millisecond))
	before := wakes(resultTimeout)

	conn := dial(t, port)
	send(t, conn, "hello\n")

	closedWithin(t, conn)
	assert.Equal(t, before+1, wakes(resultTimeout))
}

// Every connection waiting on a slow wake times out together, so they share
// one warning.
func TestServer_WarnsOnceWhenRequestsAreNotServed(t *testing.T) {
	port := freePort(t)
	recorder := events.NewFakeRecorder(10)
	startServerWith(t, func(s *Server) { s.recorder = recorder }, pausedWorkload("api", port, 300*time.Millisecond))

	for range 3 {
		conn := dial(t, port)
		send(t, conn, "hello\n")
		closedWithin(t, conn)
	}

	var warnings []string
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, "RequestNotServed") {
			warnings = append(warnings, e)
		}
	}
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "Service api")
	assert.Contains(t, warnings[0], "maxWait")
}

const browserGET = "GET /orders HTTP/1.1\r\nHost: shop.example.dev\r\nAccept: text/html\r\nSec-Fetch-Mode: navigate\r\n\r\n"

func readResponse(t *testing.T, conn net.Conn) (*http.Response, string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

func TestServer_ServesThePageToABrowser(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))
	before := wakes(resultPage)

	conn := dial(t, port)
	send(t, conn, browserGET)
	resp, body := readResponse(t, conn)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "answered at once, with no pod Ready yet")
	assert.Contains(t, body, "<title>Waking up shop.example.dev</title>")
	assert.NotContains(t, body, "dev/api", "the page names nothing inside the cluster")
	assert.NotEmpty(t, lastRequest(t, c), "the page still wakes the workload")
	assert.Equal(t, before+1, wakes(resultPage))
}

// A workload that has a Ready pod is up, even if it isn't Running yet
// because another replica can't be scheduled: a browser gets the app, not
// the page, and nothing is woken.
func TestServer_PassesABrowserThroughWhenAPodIsReady(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))

	conn := dial(t, port)
	send(t, conn, browserGET)

	echoed(t, conn, browserGET)
	assert.Empty(t, lastRequest(t, c))
}

// A request that isn't a page load is held, and the bytes read to tell it
// apart reach the woken workload untouched.
func TestServer_HoldsOtherRequestsWithTheirBytes(t *testing.T) {
	tests := []struct {
		name    string
		page    *bool
		request string
	}{
		{name: "an API call", request: "GET /api HTTP/1.1\r\nHost: api\r\nAccept: */*\r\n\r\n"},
		{name: "a browser, with the page turned off", page: ptr.To(false), request: browserGET},
		{name: "TLS", request: "\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03"},
		{name: "Postgres", request: "\x00\x00\x00\x08\x04\xd2\x16\x2f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := freePort(t)
			w := pausedWorkload("api", port, time.Minute)
			w.Spec.Wake.Page = tt.page
			_, c := startServer(t, w)

			conn := dial(t, port)
			send(t, conn, tt.request)
			require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
			require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))

			echoed(t, conn, tt.request)
		})
	}
}

// Load balancer health checks, kubelet probes and Prometheus scrapes reach
// a paused workload's Service on a schedule. Waking it for them would undo
// every pause.
func TestServer_ProbesAndScrapesDoNotWake(t *testing.T) {
	tests := []struct {
		name     string
		request  string
		want503  bool
		closeNow bool
	}{
		{name: "a TCP health check", closeNow: true},
		{name: "a kubelet probe", request: "GET /healthz HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: kube-probe/1.31\r\n\r\n", want503: true},
		{name: "a Prometheus scrape", request: "GET /metrics HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: Prometheus/2.53.0\r\n\r\n", want503: true},
		{name: "an ALB health check", request: "GET / HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: ELB-HealthChecker/2.0\r\n\r\n", want503: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := freePort(t)
			_, c := startServer(t, pausedWorkload("api", port, time.Minute))
			before := wakes(resultIgnored)

			conn := dial(t, port)
			if tt.closeNow {
				require.NoError(t, conn.Close())
			} else {
				send(t, conn, tt.request)
			}
			if tt.want503 {
				resp, _ := readResponse(t, conn)
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			}

			require.Eventually(t, func() bool { return wakes(resultIgnored) == before+1 }, 5*time.Second, 10*time.Millisecond)
			assert.Empty(t, lastRequest(t, c), "the workload stays paused")
		})
	}
}

// Prometheus remote write comes from the same agent as a scrape, but it's
// data for the workload: it's held and wakes it, rather than being told
// the workload is down and dropped.
func TestServer_RemoteWriteWakes(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))
	write := "POST /api/v1/write HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: Prometheus/2.53.0\r\n" +
		"Content-Length: 5\r\n\r\nhello"

	conn := dial(t, port)
	send(t, conn, write)

	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	echoed(t, conn, write)
}

// Once a pod is Ready, as while a woken workload finishes resuming, a
// scrape or health check reaches it rather than being told it's down.
func TestServer_PassesAProbeThroughWhenAPodIsReady(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))
	scrape := "GET /metrics HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: Prometheus/2.53.0\r\n\r\n"

	conn := dial(t, port)
	send(t, conn, scrape)

	echoed(t, conn, scrape)
	assert.Empty(t, lastRequest(t, c), "a scrape never wakes the workload")
}

// Some protocols, such as MySQL, wait for the server to speak first. A
// client that stays connected without sending anything wakes the workload.
func TestServer_WakesForAClientWaitingOnTheServer(t *testing.T) {
	port := freePort(t)
	_, c := startServerWith(t, func(s *Server) { s.silent = 100 * time.Millisecond }, pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)

	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	send(t, conn, "hello\n")
	echoed(t, conn, "hello\n")
}

// A server-first protocol through a Service the doorman still routes, but
// whose workload has a Ready pod, gets the server's greeting at once rather
// than after the doorman has waited for the client to speak.
func TestServer_PassesAServerFirstClientStraightThrough(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "220 ready\r\n")
			_ = conn.Close()
		}
	}()
	port := freePort(t)
	startServerWith(t, func(s *Server) { s.silent = 2 * time.Second },
		pausedWorkload("api", port, time.Minute), readySlice(int32(l.Addr().(*net.TCPAddr).Port)))

	conn := dial(t, port)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	greeting, err := bufio.NewReader(conn).ReadString('\n')

	require.NoError(t, err, "the greeting waited for the client to speak first")
	assert.Equal(t, "220 ready\r\n", greeting)
}

// failPatches fails the first n patches, like an API server having a moment.
func failPatches(n int32, patches *atomic.Int32) func(client.WithWatch) client.WithWatch {
	var fails atomic.Int32
	fails.Store(n)
	return func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch,
			obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patches != nil {
				patches.Add(1)
			}
			if fails.Add(-1) >= 0 {
				return errors.New("etcdserver: request timed out")
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}})
	}
}

func TestServer_RetriesAFailedWake(t *testing.T) {
	port := freePort(t)
	s, c := newTestServer(t, failPatches(1, nil), pausedWorkload("api", port, time.Minute))
	run(t, s)

	conn := dial(t, port)
	send(t, conn, "hello\n")

	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond,
		"one failed stamp is retried while the connection is held")
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	echoed(t, conn, "hello\n")
}

// A pod can be listed Ready before it accepts connections, for example with
// publishNotReadyAddresses. A refused dial is retried until maxWait.
func TestServer_RetriesAFailedDial(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute), readySlice(freePort(t)))

	conn := dial(t, port)
	send(t, conn, "hello\n")
	time.Sleep(300 * time.Millisecond) // a failed dial or two

	var slice discoveryv1.EndpointSlice
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "dev", Name: "api-real"}, &slice))
	slice.Ports[0].Port = ptr.To(echoPort(t))
	require.NoError(t, c.Update(context.Background(), &slice))

	echoed(t, conn, "hello\n")
}

func TestDialAny_SpreadsConnectionsOverPods(t *testing.T) {
	first, firstCount := echoServer(t)
	second, secondCount := echoServer(t)
	addrs := []string{
		net.JoinHostPort("127.0.0.1", strconv.Itoa(int(first))),
		net.JoinHostPort("127.0.0.1", strconv.Itoa(int(second))),
	}

	for range 40 {
		conn := dialAny(context.Background(), addrs)
		require.NotNil(t, conn)
		require.NoError(t, conn.Close())
	}

	require.Eventually(t, func() bool { return firstCount.Load()+secondCount.Load() == 40 }, 5*time.Second, 10*time.Millisecond)
	assert.Positive(t, firstCount.Load())
	assert.Positive(t, secondCount.Load())
}

// Many connections held for one workload wake it once, read its endpoints
// together rather than one by one, and all reach it once it's Ready.
func TestServer_ManyHeldConnections(t *testing.T) {
	const held = 100
	port := freePort(t)
	var lists, patches atomic.Int32
	wrap := func(c client.WithWatch) client.WithWatch {
		c = failPatches(0, &patches)(c)
		return interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch,
			list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*discoveryv1.EndpointSliceList); ok {
				lists.Add(1)
			}
			return cl.List(ctx, list, opts...)
		}})
	}
	s, c := newTestServer(t, wrap, pausedWorkload("api", port, time.Minute))
	run(t, s)

	conns := make([]net.Conn, held)
	for i := range conns {
		conns[i] = dial(t, port)
		send(t, conns[i], "hello "+strconv.Itoa(i)+"\n")
	}
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(opmetrics.DoormanHeldConnections) >= held
	}, 5*time.Second, 10*time.Millisecond)
	listsBefore := lists.Load()
	time.Sleep(time.Second)
	assert.Less(t, lists.Load()-listsBefore, int32(20), "endpoints are read once per poll, not once per connection")

	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Go(func() { echoed(t, conn, "hello "+strconv.Itoa(i)+"\n") })
	}
	wg.Wait()
	assert.Equal(t, int32(1), patches.Load(), "one stamp wakes the workload for every connection")
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(opmetrics.DoormanProxiedConnections) >= held
	}, 5*time.Second, 10*time.Millisecond, "proxied connections are counted apart from held ones")
}

func TestServer_HeldConnectionsAreCapped(t *testing.T) {
	port := freePort(t)
	s, _ := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.maxHeld = 2
		s.admission = newAdmission(l)
	}, pausedWorkload("api", port, time.Minute))
	before := wakes(resultLimited)

	for range 2 {
		send(t, dial(t, port), "hello\n")
	}
	require.Eventually(t, func() bool { return heldCount(s) == 2 }, 5*time.Second, 10*time.Millisecond)
	third := dial(t, port)

	closedWithin(t, third)
	assert.Equal(t, before+1, wakes(resultLimited))
}

// A caller that goes away while held frees its place.
func TestServer_ACallerLeavingFreesItsPlace(t *testing.T) {
	port := freePort(t)
	s, _ := startServer(t, pausedWorkload("api", port, time.Minute))
	before := wakes(resultCanceled)

	conn := dial(t, port)
	send(t, conn, "hello\n")
	require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool { return wakes(resultCanceled) == before+1 }, 5*time.Second, 10*time.Millisecond)
	assert.Zero(t, heldCount(s))
}

func heldCount(s *Server) int {
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	return s.admission.held
}

// Scanning the doorman's ports wakes only as many workloads as one source
// may wake.
func TestServer_WakesAreLimitedPerSource(t *testing.T) {
	first, second := freePort(t), freePort(t)
	other := pausedWorkload("billing", second, time.Minute)
	_, c := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.sourceWakeBurst = 1
		l.sourceWakeEvery = time.Hour
		s.admission = newAdmission(l)
	}, pausedWorkload("api", first, time.Minute), other)
	before := testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("dev", "billing", resultLimited))

	send(t, dial(t, first), "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	conn := dial(t, second)
	send(t, conn, "hello\n")

	closedWithin(t, conn)
	assert.Equal(t, before+1, testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("dev", "billing", resultLimited)))
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "dev", Name: "billing"}, other))
	assert.Empty(t, other.Annotations[v1alpha1.AnnotationLastRequest])
}

// Two routes claiming one port would send one workload's callers to the
// other; the doorman serves neither.
func TestServer_RefusesAPortClaimedTwice(t *testing.T) {
	port := freePort(t)
	a := pausedWorkload("api", port, time.Minute)
	b := pausedWorkload("billing", port, time.Minute)
	b.Namespace = "other-team"
	s, _ := startServer(t, a, b)

	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	assert.Error(t, err)
	assert.Equal(t, float64(1), testutil.ToFloat64(opmetrics.DoormanPortConflicts))

	b.Status.Doorman[0].DoormanPort = freePort(t)
	require.NoError(t, s.client.Update(context.Background(), b))
	require.NoError(t, s.syncRoutes(context.Background(), context.Background()))
	dial(t, port)
	assert.Zero(t, testutil.ToFloat64(opmetrics.DoormanPortConflicts))
}

// testClock is a clock tests move by hand, safe to read from the doorman's
// connection goroutines.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func removeRoute(t *testing.T, s *Server, c client.Client) {
	t.Helper()
	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	w.Status.Doorman = nil
	require.NoError(t, c.Update(context.Background(), &w))
	require.NoError(t, s.syncRoutes(context.Background(), context.Background()))
}

// Proxies keep sending to the doorman for a moment after the workload is
// awake, so its port drains, passing connections straight to the workload,
// before it's released.
func TestServer_DrainsARemovedRouteThenReleasesIt(t *testing.T) {
	port := freePort(t)
	clock := &testClock{now: time.Now()}
	s, c := startServerWith(t, func(s *Server) { s.now = clock.Now }, pausedWorkload("api", port, time.Minute))
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	removeRoute(t, s, c)

	conn := dial(t, port)
	send(t, conn, browserGET)
	echoed(t, conn, browserGET)

	clock.Advance(RouteDrain)
	require.NoError(t, s.syncRoutes(context.Background(), context.Background()))
	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	assert.Error(t, err, "the port is released once it has drained")
}

// A route that leaves status while its workload is still at zero, because
// it opted out of waking on request or stopped being managed, has nothing
// to pass connections to. They're refused at once rather than held without
// a wake.
func TestServer_RefusesPromptlyWhenADrainingRouteHasNothingReady(t *testing.T) {
	port := freePort(t)
	s, c := startServer(t, pausedWorkload("api", port, time.Minute))
	before := wakes(resultRefused)
	held := dial(t, port)
	send(t, held, "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)

	removeRoute(t, s, c)

	closedWithin(t, held)
	closedWithin(t, dial(t, port))
	assert.Equal(t, before+2, wakes(resultRefused))
}

// On shutdown the doorman stops accepting, keeps the connections it carries
// for the grace period, and only then closes them.
func TestServer_ShutdownDrainsConnections(t *testing.T) {
	port := freePort(t)
	s, _ := newTestServer(t, nil, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))
	s.grace = 500 * time.Millisecond
	stop := run(t, s)

	conn := dial(t, port)
	send(t, conn, "before\n")
	echoed(t, conn, "before\n")

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	require.Eventually(t, func() bool { return s.Ready(nil) != nil }, time.Second, 10*time.Millisecond,
		"a stopping doorman leaves its Service")
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	}, time.Second, 10*time.Millisecond, "no new connections")
	send(t, conn, "during\n")
	echoed(t, conn, "during\n")

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown didn't finish after the grace period")
	}
	closedWithin(t, conn)
}

func TestServer_ReadinessFollowsRouteSync(t *testing.T) {
	clock := &testClock{now: time.Now()}
	var failing atomic.Bool
	s, _ := newTestServer(t, func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch,
			list client.ObjectList, opts ...client.ListOption) error {
			if failing.Load() {
				return errors.New("cache not synced")
			}
			return cl.List(ctx, list, opts...)
		}})
	}, pausedWorkload("api", freePort(t), time.Minute))
	s.now = clock.Now
	ctx := context.Background()
	defer func() {
		for _, l := range s.listeners {
			_ = l.Close()
		}
	}()

	require.Error(t, s.Ready(nil), "not Ready before routes load")
	require.NoError(t, s.syncRoutes(ctx, ctx))
	require.NoError(t, s.Ready(nil))

	failing.Store(true)
	require.Error(t, s.syncRoutes(ctx, ctx))
	clock.Advance(readyStaleness + time.Second)
	assert.Error(t, s.Ready(nil), "routes that can't be loaded may be wrong")

	failing.Store(false)
	require.NoError(t, s.syncRoutes(ctx, ctx))
	assert.NoError(t, s.Ready(nil))
}

// With an informer, a held connection is passed through the moment its
// workload's endpoints change, without waiting for a poll.
func TestServer_InformerWakesHeldConnections(t *testing.T) {
	port := freePort(t)
	scheme := testScheme(t)
	informers := &informertest.FakeInformers{Scheme: scheme}
	informer, err := informers.FakeInformerFor(context.Background(), &discoveryv1.EndpointSlice{})
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pausedWorkload("api", port, time.Minute)).Build()
	s := NewServer(c, nil, Options{Host: "127.0.0.1", Informers: informers})
	s.backends.allowed = func(netip.Addr) bool { return true }
	run(t, s)

	conn := dial(t, port)
	send(t, conn, "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	slice := readySlice(echoPort(t))
	require.NoError(t, c.Create(context.Background(), slice))
	start := time.Now()
	informer.Add(slice)

	echoed(t, conn, "hello\n")
	assert.Less(t, time.Since(start), backendResyncInterval/2, "passed through on the change, not the resync")
}

func TestNeedsWake(t *testing.T) {
	pausedAt := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	before := pausedAt.Add(-2 * time.Second).Format(time.RFC3339)
	after := pausedAt.Add(time.Minute).Format(time.RFC3339)
	paused := func(stamp string, recorded map[string]string) *v1alpha1.ManagedWorkload {
		w := pausedWorkload("api", 20000, time.Minute)
		w.Status.Pause.PausedAt = ptr.To(metav1.NewTime(pausedAt))
		w.Status.Pause.WakeAnnotations = &v1alpha1.WakeAnnotations{Workload: recorded}
		if stamp != "" {
			w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: stamp}
		}
		return w
	}

	tests := []struct {
		name     string
		workload *v1alpha1.ManagedWorkload
		want     bool
	}{
		{name: "never stamped", workload: paused("", nil), want: true},
		{name: "stamped just before the pause, which recorded it", want: true,
			workload: paused(before, map[string]string{v1alpha1.AnnotationLastRequest: before})},
		{name: "stamped since the pause", workload: paused(after, map[string]string{v1alpha1.AnnotationLastRequest: before})},
		{name: "stamped since a pause that recorded nothing", workload: paused(after, nil)},
		{name: "a pause with no record, stamped before it", want: true, workload: func() *v1alpha1.ManagedWorkload {
			w := paused(before, nil)
			w.Status.Pause.WakeAnnotations = nil
			return w
		}()},
		{name: "a pause with no record, stamped since", workload: func() *v1alpha1.ManagedWorkload {
			w := paused(after, nil)
			w.Status.Pause.WakeAnnotations = nil
			return w
		}()},
		{name: "already waking", workload: func() *v1alpha1.ManagedWorkload {
			w := paused("", nil)
			w.Status.Phase = v1alpha1.PhaseResuming
			return w
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, needsWake(tt.workload))
		})
	}
}

// A stamp from just before the pause, within the last few seconds, used to
// be taken for another replica's stamp of the same burst, and the wake was
// lost. The pause recorded it, so it wakes nothing: a new one is needed.
func TestServer_StampsAgainAfterAStampThePauseRecorded(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Second).Format(time.RFC3339)
	w := pausedWorkload("api", freePort(t), time.Minute)
	w.Status.Pause.PausedAt = ptr.To(metav1.NewTime(now.Add(-time.Second)))
	w.Status.Pause.WakeAnnotations = &v1alpha1.WakeAnnotations{Workload: map[string]string{v1alpha1.AnnotationLastRequest: recent}}
	w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: recent}
	s, c := newTestServer(t, nil, w)
	s.now = func() time.Time { return now }

	require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

	assert.Equal(t, now.Format(time.RFC3339), lastRequest(t, c))
}

// The operator wakes a paused workload when its last-request value differs
// from the one recorded at the pause, so a stamp in the same second as that
// one must still be a new value.
func TestServer_StampsANewValueInTheSameSecond(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 500, time.UTC)
	same := now.Format(time.RFC3339)
	w := pausedWorkload("api", freePort(t), time.Minute)
	w.Status.Pause.WakeAnnotations = &v1alpha1.WakeAnnotations{Workload: map[string]string{v1alpha1.AnnotationLastRequest: same}}
	w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: same}
	s, c := newTestServer(t, nil, w)
	s.now = func() time.Time { return now }

	require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

	stamped := lastRequest(t, c)
	assert.NotEqual(t, same, stamped)
	parsed, err := time.Parse(time.RFC3339, stamped)
	require.NoError(t, err, "still an RFC 3339 time")
	assert.True(t, parsed.Equal(now))
}

// A replica that sees another's stamp since the pause leaves it alone.
func TestServer_LeavesAPendingWakeAlone(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	pending := now.Add(-time.Minute).Format(time.RFC3339)
	w := pausedWorkload("api", freePort(t), time.Minute)
	w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: pending}
	recorder := events.NewFakeRecorder(10)
	s, c := newTestServer(t, nil, w)
	s.recorder = recorder
	s.now = func() time.Time { return now }

	require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

	assert.Equal(t, pending, lastRequest(t, c))
	assert.Equal(t, 0, eventCount(recorder, "WokenByRequest"))
}

// staleReads makes Get return a copy of the workload as it was when the
// test began, like a replica whose cache hasn't caught up.
func staleReads(t *testing.T, c client.Client) interceptor.Funcs {
	t.Helper()
	var snapshot v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &snapshot))
	return interceptor.Funcs{Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object,
		_ ...client.GetOption) error {
		snapshot.DeepCopyInto(obj.(*v1alpha1.ManagedWorkload))
		return nil
	}}
}

func eventCount(r *events.FakeRecorder, reason string) int {
	var n int
	for len(r.Events) > 0 {
		if strings.Contains(<-r.Events, reason) {
			n++
		}
	}
	return n
}

// Two doorman replicas can each get a connection from the same burst, the
// second reading a copy from before the first stamped. Only the first
// reports the wake.
func TestServer_ReplicasReportOneWake(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pausedWorkload("api", freePort(t), time.Minute)).Build()
	stale := interceptor.NewClient(c, staleReads(t, c))
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	rt := route{workload: apiKey, service: "api"}

	first, second := events.NewFakeRecorder(10), events.NewFakeRecorder(10)
	for _, replica := range []struct {
		c client.Client
		r *events.FakeRecorder
	}{{c, first}, {stale, second}} {
		s := NewServer(replica.c, replica.r, Options{})
		s.now = func() time.Time { return now }
		require.NoError(t, s.stamp(context.Background(), rt, caller))
	}

	assert.Equal(t, 1, eventCount(first, "WokenByRequest"))
	assert.Equal(t, 0, eventCount(second, "WokenByRequest"))
}

// A conflict from some other change must not lose the wake.
func TestServer_StampsDespiteAConflict(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pausedWorkload("api", freePort(t), time.Minute)).Build()
	stale := interceptor.NewClient(c, staleReads(t, c))

	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	w.Labels = map[string]string{"team": "payments"}
	require.NoError(t, c.Update(context.Background(), &w))

	s := NewServer(stale, nil, Options{})
	require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationLastRequest], "the workload is still woken")
	assert.Equal(t, "10.244.0.17", w.Annotations[v1alpha1.AnnotationLastRequestFrom], "and where the request came from")
	assert.Equal(t, "payments", w.Labels["team"], "the other change is kept")
}
