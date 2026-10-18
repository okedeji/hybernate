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
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

// apiPod is the pod readySlice names, with addrs.
func apiPod(addrs ...string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "dev"}}
	for _, addr := range addrs {
		pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: addr})
	}
	return pod
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

// newTestServer is a doorman over a fake client holding objs, and the pod
// readySlice names. Tests run their backends on loopback, which the doorman
// otherwise never dials, and stop without the drain a doorman pod gets,
// closing what they hold.
func newTestServer(t *testing.T, wrap func(client.WithWatch) client.WithWatch, objs ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	var c client.WithWatch
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(objs, apiPod("127.0.0.1"))...).Build()
	if wrap != nil {
		c = wrap(c)
	}
	s := NewServer(c, nil, Options{Host: "127.0.0.1"})
	s.backends.allowed = func(netip.Addr) bool { return true }
	s.acceptFor, s.grace = 0, 100*time.Millisecond
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
	// The result is counted once the connection is done with, just after
	// the caller has its answer.
	assert.Eventually(t, func() bool { return wakes(resultSuccess) == before+1 },
		5*time.Second, 10*time.Millisecond, "counted as resultSuccess")
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
		{name: "AWS's IPv6 metadata service", slice: func(s *discoveryv1.EndpointSlice) {
			s.AddressType = discoveryv1.AddressTypeIPv6
			s.Endpoints[0].Addresses = []string{"fd00:ec2::254"}
		}},
		{name: "Alibaba Cloud's metadata service", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"100.100.100.200"}
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
		{name: "an address the pod doesn't have", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.10"}
		}},
		{name: "a pod in another namespace", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.9"}
			s.Endpoints[0].TargetRef.Namespace = "prod"
		}},
		{name: "a pod that doesn't exist", slice: func(s *discoveryv1.EndpointSlice) {
			s.Endpoints[0].Addresses = []string{"10.0.0.9"}
			s.Endpoints[0].TargetRef.Name = "gone"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slice := readySlice(8080)
			tt.slice(slice)
			elsewhere := apiPod("10.0.0.9")
			elsewhere.Namespace = "prod"
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice, apiPod("10.0.0.9"), elsewhere).Build()

			addrs, _, err := newBackends(c, c).get(context.Background(), backendKey{
				service: serviceRef{namespace: "dev", name: "api"}, portName: "http",
			})

			require.NoError(t, err)
			assert.Empty(t, addrs)
		})
	}
}

// The doorman's cache keeps only part of each slice; that part gives the
// same backends as the whole.
func TestTrimEndpointSlice_KeepsWhatBackendsRead(t *testing.T) {
	full := readySlice(8080)
	full.Endpoints[0].Addresses = []string{"10.244.1.9", "10.244.1.10"}
	full.Endpoints[0].NodeName = ptr.To("node-1")
	full.Endpoints = append(full.Endpoints,
		discoveryv1.Endpoint{
			Addresses:  []string{"10.244.2.9"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: "api-1"},
		},
		discoveryv1.Endpoint{
			Addresses:  []string{"10.244.3.9"},
			Conditions: discoveryv1.EndpointConditions{Terminating: ptr.To(true)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: "api-2"},
		},
		discoveryv1.Endpoint{Addresses: []string{"10.244.4.9"}},
	)
	trimmed, err := trimEndpointSlice(full.DeepCopy())
	require.NoError(t, err)
	key := backendKey{service: serviceRef{namespace: "dev", name: "api"}, portName: "http"}
	backendsOf := func(slice *discoveryv1.EndpointSlice) []string {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice, apiPod("10.244.1.9")).Build()
		b := newBackends(c, c)
		b.allowed = func(netip.Addr) bool { return true }
		addrs, _, err := b.get(context.Background(), key)
		require.NoError(t, err)
		return addrs
	}

	assert.Equal(t, []string{"10.244.1.9:8080"}, backendsOf(full))
	assert.Equal(t, backendsOf(full), backendsOf(trimmed.(*discoveryv1.EndpointSlice)))
	assert.Equal(t, ReadyPodEndpoints([]discoveryv1.EndpointSlice{*full}),
		ReadyPodEndpoints([]discoveryv1.EndpointSlice{*trimmed.(*discoveryv1.EndpointSlice)}))
}

// Pod networks are allocated from private, unique-local and shared address
// space, so those stay routable around the metadata services refused.
func TestRoutable(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{addr: "10.244.1.9", want: true},
		{addr: "100.64.3.7", want: true},
		{addr: "fd12:3456::7", want: true},
		{addr: "2600:1f14::9", want: true},
		{addr: "127.0.0.1"},
		{addr: "169.254.169.254"},
		{addr: "fd00:ec2::254"},
		{addr: "fd00:ec2::253"},
		{addr: "100.100.100.200"},
		{addr: "::1"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, routable(netip.MustParseAddr(tt.addr)), tt.addr)
	}
}

func TestBackends_IPv6(t *testing.T) {
	slice := readySlice(8080)
	slice.AddressType = discoveryv1.AddressTypeIPv6
	slice.Endpoints[0].Addresses = []string{"fd00:10:244::7"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice, apiPod("10.244.0.7", "fd00:10:244::7")).Build()

	addrs, _, err := newBackends(c, c).get(context.Background(), backendKey{
		service: serviceRef{namespace: "dev", name: "api"}, portName: "http",
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"[fd00:10:244::7]:8080"}, addrs)
}

// A StatefulSet's pod recreated under the same name has a new address,
// which is checked at once rather than when the old one's TTL runs out.
func TestBackends_ReadsAPodAgainWhenItsAddressChanges(t *testing.T) {
	slice := readySlice(8080)
	slice.Endpoints[0].Addresses = []string{"10.0.0.9"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(slice, apiPod("10.0.0.9")).Build()
	clock := &testClock{now: time.Now()}
	b := newBackends(c, c)
	b.now = clock.Now
	key := backendKey{service: serviceRef{namespace: "dev", name: "api"}, portName: "http"}
	ctx := context.Background()

	addrs, _, err := b.get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, []string{"10.0.0.9:8080"}, addrs)

	require.NoError(t, c.Delete(ctx, apiPod()))
	require.NoError(t, c.Create(ctx, apiPod("10.0.0.10")))
	slice.Endpoints[0].Addresses = []string{"10.0.0.10"}
	require.NoError(t, c.Update(ctx, slice))
	clock.Advance(podRecheck)
	b.changedAll()

	addrs, _, err = b.get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.10:8080"}, addrs)
}

func TestServer_ClosesAfterMaxWait(t *testing.T) {
	port := freePort(t)
	startServer(t, pausedWorkload("api", port, 300*time.Millisecond))
	before := wakes(resultTimeout)

	conn := dial(t, port)
	send(t, conn, "hello\n")

	closedWithin(t, conn)
	// The result is counted once the connection is done with, just after
	// the caller has its answer.
	assert.Eventually(t, func() bool { return wakes(resultTimeout) == before+1 },
		5*time.Second, 10*time.Millisecond, "counted as resultTimeout")
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
	// The page goes out before the wake lands, so a browser isn't kept
	// waiting on the API server.
	assert.Eventually(t, func() bool { return lastRequest(t, c) != "" },
		5*time.Second, 10*time.Millisecond, "the page still wakes the workload")
	// The result is counted once the connection is done with, just after
	// the caller has its answer.
	assert.Eventually(t, func() bool { return wakes(resultPage) == before+1 },
		5*time.Second, 10*time.Millisecond, "counted as resultPage")
}

// Single sign-on cookies can take a browser's headers past what the doorman
// reads to tell what a request is. It still gets the page.
func TestServer_ServesThePageToABrowserWithLargeCookies(t *testing.T) {
	port := freePort(t)
	_, _ = startServer(t, pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)
	send(t, conn, "GET /orders HTTP/1.1\r\nHost: shop.example.dev\r\nAccept: text/html\r\nSec-Fetch-Mode: navigate\r\n"+
		"Cookie: _oauth2_proxy="+strings.Repeat("a", 2*maxPeek)+"\r\n\r\n")
	resp, _ := readResponse(t, conn)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// A request whose headers run past what the doorman reads to judge it is
// held, and reaches the woken workload whole.
func TestServer_HoldsARequestWithLargeHeadersIntact(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))
	request := "GET /api HTTP/1.1\r\nHost: api\r\nAccept: */*\r\nCookie: sso=" + strings.Repeat("a", 2*maxPeek) + "\r\n\r\n"

	conn := dial(t, port)
	send(t, conn, request)
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))

	echoed(t, conn, request)
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
		{name: "a probe ending lines in a bare LF", request: "GET /healthz HTTP/1.1\nHost: 10.0.0.1\nUser-Agent: kube-probe/1.31\n\n", want503: true},
		{name: "a Prometheus scrape", request: "GET /metrics HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: Prometheus/2.53.0\r\n\r\n", want503: true},
		{name: "an ALB health check", request: "GET / HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: ELB-HealthChecker/2.0\r\n\r\n", want503: true},
		{name: "a blackbox exporter probe", request: "GET /health HTTP/1.1\r\nHost: api\r\nUser-Agent: Blackbox Exporter/0.25.0\r\n\r\n", want503: true},
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

// A scanner hunting for leaked secrets in hidden files gets a 404, and the
// paused workload sleeps on; a real request after it still wakes it.
func TestServer_HiddenFileRequestsDoNotWake(t *testing.T) {
	tests := []struct {
		name    string
		request string
	}{
		{name: "lines ending in CRLF", request: "GET /.env HTTP/1.1\r\nHost: shop.example.dev\r\nUser-Agent: Mozilla/5.0\r\n\r\n"},
		{name: "lines ending in a bare LF", request: "GET /.env HTTP/1.1\nHost: shop.example.dev\nUser-Agent: Mozilla/5.0\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := freePort(t)
			_, c := startServer(t, pausedWorkload("api", port, time.Minute))
			before := wakes(resultIgnored)

			conn := dial(t, port)
			send(t, conn, tt.request)
			resp, _ := readResponse(t, conn)

			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
			require.Eventually(t, func() bool { return wakes(resultIgnored) == before+1 }, 5*time.Second, 10*time.Millisecond)
			assert.Empty(t, lastRequest(t, c), "the workload stays paused")

			send(t, dial(t, port), "GET / HTTP/1.1\r\nHost: shop.example.dev\r\n\r\n")
			require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond,
				"a real request still wakes it")
		})
	}
}

// A health checker the doorman doesn't know by default is recognised once
// its User-Agent is added.
func TestServer_AddedHealthCheckAgentsDoNotWake(t *testing.T) {
	port := freePort(t)
	_, c := startServerWith(t, func(s *Server) { s.healthCheckAgents = healthCheckAgents([]string{"MyCorpMonitor/"}) },
		pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)
	send(t, conn, "GET /healthz HTTP/1.1\r\nHost: 10.0.0.1\r\nUser-Agent: MyCorpMonitor/3.1\r\n\r\n")
	resp, _ := readResponse(t, conn)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Never(t, func() bool { return lastRequest(t, c) != "" }, 300*time.Millisecond, 20*time.Millisecond,
		"the workload stays paused")
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
	// The result is counted once the connection is done with, just after
	// the caller has its answer.
	assert.Eventually(t, func() bool { return wakes(resultLimited) == before+1 },
		5*time.Second, 10*time.Millisecond, "counted as resultLimited")
}

// A caller that goes away while held frees its place, whatever its
// protocol: closing a connection and shutting down its sending side are the
// same FIN on the wire, and a database driver whose connect timed out, or an
// ingress closing an HTTPS upstream, sends one when it gives up.
func TestServer_ACallerLeavingFreesItsPlace(t *testing.T) {
	tests := []struct {
		name  string
		send  string
		leave func(*net.TCPConn) error
	}{
		{name: "an HTTP client closing", send: "GET /api HTTP/1.1\r\nHost: api\r\n\r\n",
			leave: func(c *net.TCPConn) error { return c.Close() }},
		{name: "an HTTP client half-closing", send: "GET /api HTTP/1.1\r\nHost: api\r\n\r\n",
			leave: func(c *net.TCPConn) error { return c.CloseWrite() }},
		{name: "another protocol's client closing", send: "hello\n",
			leave: func(c *net.TCPConn) error { return c.Close() }},
		{name: "another protocol's client half-closing", send: "hello\n",
			leave: func(c *net.TCPConn) error { return c.CloseWrite() }},
		{name: "a reset", send: "hello\n", leave: func(c *net.TCPConn) error {
			if err := c.SetLinger(0); err != nil {
				return err
			}
			return c.Close()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := freePort(t)
			s, _ := startServer(t, pausedWorkload("api", port, time.Minute))
			before := wakes(resultCanceled)

			conn := dial(t, port)
			send(t, conn, tt.send)
			require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, tt.leave(conn.(*net.TCPConn)))

			require.Eventually(t, func() bool { return wakes(resultCanceled) == before+1 }, 5*time.Second, 10*time.Millisecond)
			// The place is freed as the connection's handler returns, just
			// after its result is counted.
			assert.Eventually(t, func() bool { return heldCount(s) == 0 }, 5*time.Second, 10*time.Millisecond)
		})
	}
}

// A caller that sent a request body before leaving frees its place too,
// rather than staying held until maxWait because its body was never read.
func TestServer_ACallerLeavingAfterSendingABodyFreesItsPlace(t *testing.T) {
	port := freePort(t)
	s, _ := startServer(t, pausedWorkload("api", port, time.Minute))
	before := wakes(resultCanceled)

	conn := dial(t, port)
	send(t, conn, "POST /upload HTTP/1.1\r\nHost: shop\r\nContent-Length: 102400\r\n\r\n"+strings.Repeat("x", 100<<10))
	require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool { return wakes(resultCanceled) == before+1 }, 5*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool { return heldCount(s) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.Eventually(t, func() bool { return bufferedBytes(s) == 0 }, 5*time.Second, 10*time.Millisecond,
		"its buffered body is let go")
}

// A body larger than a held connection may buffer still reaches the
// workload whole and in order once it's Ready.
func TestServer_HoldsALargeBodyIntact(t *testing.T) {
	port := freePort(t)
	s, c := startServer(t, pausedWorkload("api", port, time.Minute))
	body := strings.Repeat("0123456789abcdef", (3<<20)/16)
	request := "POST /upload HTTP/1.1\r\nHost: shop\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body

	conn := dial(t, port)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = conn.Write([]byte(request)) }) // fails only if the test has already failed
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return bufferedBytes(s) >= defaultLimits.maxBufferedPerConn/2 }, 5*time.Second, 10*time.Millisecond)
	assert.LessOrEqual(t, bufferedBytes(s), defaultLimits.maxBufferedPerConn, "a held connection buffers no more than its cap")
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	got := make([]byte, len(request))
	_, err := io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, request, string(got))
	wg.Wait()
	assert.Zero(t, bufferedBytes(s), "a connection passed on stops counting against held ones")
}

func bufferedBytes(s *Server) int {
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	return s.admission.buffered
}

func heldCount(s *Server) int {
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	return s.admission.held
}

// Scanning the doorman's ports wakes only as many workloads as one source
// may wake at once. A caller over the limit isn't dropped: it's held, and
// its workload is woken once the source's allowance refills.
func TestServer_WakesAreLimitedPerSource(t *testing.T) {
	first, second := freePort(t), freePort(t)
	other := pausedWorkload("billing", second, time.Minute)
	_, c := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.sourceWakeBurst = 1
		l.sourceWakeEvery = time.Hour
		s.admission = newAdmission(l)
	}, pausedWorkload("api", first, time.Minute), other)
	billing := types.NamespacedName{Namespace: "dev", Name: "billing"}

	send(t, dial(t, first), "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	conn := dial(t, second)
	send(t, conn, "hello\n")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2500*time.Millisecond)))
	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "the connection is held, not closed: %v", err)
	require.NoError(t, c.Get(context.Background(), billing, other))
	assert.Empty(t, other.Annotations[v1alpha1.AnnotationLastRequest], "the limit holds back the wake")
}

func TestServer_RetriesALimitedWake(t *testing.T) {
	first, second := freePort(t), freePort(t)
	other := pausedWorkload("billing", second, time.Minute)
	_, c := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.sourceWakeBurst = 1
		l.sourceWakeEvery = time.Second
		s.admission = newAdmission(l)
	}, pausedWorkload("api", first, time.Minute), other)
	billing := types.NamespacedName{Namespace: "dev", Name: "billing"}

	send(t, dial(t, first), "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	send(t, dial(t, second), "hello\n")

	require.Eventually(t, func() bool {
		require.NoError(t, c.Get(context.Background(), billing, other))
		return other.Annotations[v1alpha1.AnnotationLastRequest] != ""
	}, 5*time.Second, 50*time.Millisecond, "woken once the source may wake again")
}

// A browser is shown the waking-up page even when its wake is held back,
// rather than a dropped connection its ingress turns into a 502.
func TestServer_ServesThePageWhenWakesAreLimited(t *testing.T) {
	first, second := freePort(t), freePort(t)
	_, c := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.sourceWakeBurst = 1
		l.sourceWakeEvery = time.Hour
		s.admission = newAdmission(l)
	}, pausedWorkload("api", first, time.Minute), pausedWorkload("billing", second, time.Minute))

	send(t, dial(t, first), "hello\n")
	require.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond)
	conn := dial(t, second)
	send(t, conn, browserGET)

	resp, body := readResponse(t, conn)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, body, "<html")
}

// A wake refused because the doorman as a whole is at its limit leaves the
// source's own allowance alone, so it may wake as soon as the doorman can.
func TestAdmission_AWakeRefusedOverallDoesNotSpendTheSources(t *testing.T) {
	a := newAdmission(limits{
		sourceWakeEvery: time.Hour, sourceWakeBurst: 1,
		wakeEvery: time.Second, wakeBurst: 1,
	})
	now := time.Now()
	first, second := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")

	require.True(t, a.allowWake(first, now))
	require.False(t, a.allowWake(second, now), "the doorman's allowance is spent")

	assert.True(t, a.allowWake(second, now.Add(time.Second)))
}

// Behind an ingress, every user comes from one address. A burst of
// connections to one workload can't take the places the others need.
func TestAdmission_HeldConnectionsAreCappedPerWorkload(t *testing.T) {
	a := newAdmission(limits{maxHeld: 100, maxHeldPerWorkload: 50, maxHeldPerSource: 50, maxHeldPerSourceWorkload: 2})
	now := time.Now()
	ingress := netip.MustParseAddr("10.0.0.1")
	shop := types.NamespacedName{Namespace: "dev", Name: "shop"}
	billing := types.NamespacedName{Namespace: "dev", Name: "billing"}

	require.True(t, a.hold(ingress, shop, now))
	require.True(t, a.hold(ingress, shop, now))
	assert.False(t, a.hold(ingress, shop, now), "shop is at its cap")
	assert.True(t, a.hold(ingress, billing, now), "billing still has room")

	a.release(ingress, shop, now)
	assert.True(t, a.hold(ingress, shop, now))
}

// A deleted workload's series go, as the operator drops its own, so a
// doorman that outlives many workloads doesn't report every one forever.
func TestServer_DropsADeletedWorkloadsSeries(t *testing.T) {
	port := freePort(t)
	w := pausedWorkload("gone", port, time.Minute)
	_, c := startServer(t, w)
	series := func() int { return testutil.CollectAndCount(opmetrics.DoormanWakes) }
	before := series()

	send(t, dial(t, port), "GET /healthz HTTP/1.1\r\nHost: gone\r\nUser-Agent: kube-probe/1.31\r\n\r\n")
	require.Eventually(t, func() bool { return series() == before+1 }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, c.Delete(context.Background(), w))
	assert.Eventually(t, func() bool { return series() == before }, 3*routeSyncInterval, 20*time.Millisecond)
}

// A workload that never becomes Ready, called from many sources, can't take
// the places other workloads need.
func TestAdmission_OneWorkloadCannotTakeEveryPlace(t *testing.T) {
	a := newAdmission(defaultLimits)
	now := time.Now()
	broken := types.NamespacedName{Namespace: "team-a", Name: "broken"}
	other := types.NamespacedName{Namespace: "team-b", Name: "api"}

	admitted := 0
	for i := range defaultLimits.maxHeld {
		if a.hold(netip.AddrFrom4([4]byte{10, 0, byte(i / 100), byte(i % 100)}), broken, now) {
			admitted++
		}
	}
	assert.Equal(t, defaultLimits.maxHeldPerWorkload, admitted)
	assert.True(t, a.hold(netip.MustParseAddr("10.9.9.9"), other, now), "another workload still has room")

	a.release(netip.AddrFrom4([4]byte{10, 0, 0, 0}), broken, now)
	assert.True(t, a.hold(netip.MustParseAddr("10.9.9.8"), broken, now), "a released place can be taken again")
}

func TestServer_MaxWaitIsBounded(t *testing.T) {
	w := pausedWorkload("api", freePort(t), 0)
	w.Spec.Wake.MaxWait = &metav1.Duration{Duration: 10000 * time.Hour}
	s, _ := newTestServer(t, nil, w)
	ctx := context.Background()
	require.NoError(t, s.syncRoutes(ctx, ctx))
	t.Cleanup(func() {
		for _, l := range s.listeners {
			_ = l.Close()
		}
	})

	require.Len(t, s.routes, 1)
	for _, rt := range s.routes {
		assert.Equal(t, v1alpha1.WakeMaxWaitLimit, rt.maxWait)
	}
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
	// The result is counted once the connection is done with, just after
	// the caller has its answer.
	assert.Eventually(t, func() bool { return wakes(resultRefused) == before+2 },
		5*time.Second, 10*time.Millisecond, "counted as resultRefused")
}

// A stopping doorman leaves its Service at once, but goes on holding and
// passing through new connections for a while, since only the operator can
// send them elsewhere. Then it stops accepting, and closes what's left at
// the end of the grace period.
func TestServer_ShutdownDrainsConnections(t *testing.T) {
	port := freePort(t)
	s, c := newTestServer(t, nil, pausedWorkload("api", port, time.Minute))
	s.acceptFor = 500 * time.Millisecond
	s.grace = time.Second
	stop := run(t, s)

	before := dial(t, port)
	send(t, before, "before\n")
	require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	require.Eventually(t, func() bool { return s.Ready(nil) != nil }, time.Second, 10*time.Millisecond,
		"a stopping doorman leaves its Service")

	during := dial(t, port)
	send(t, during, "during\n")
	require.Eventually(t, func() bool { return heldCount(s) == 2 }, time.Second, 10*time.Millisecond,
		"a connection arriving as the doorman stops is held")
	require.NoError(t, c.Create(context.Background(), readySlice(echoPort(t))))
	echoed(t, before, "before\n")
	echoed(t, during, "during\n")

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "no new connections once the accepting part of the drain is over")
	send(t, during, "still\n")
	echoed(t, during, "still\n")

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown didn't finish after the grace period")
	}
	closedWithin(t, during)
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pausedWorkload("api", port, time.Minute), apiPod("127.0.0.1")).Build()
	s := NewServer(c, nil, Options{Host: "127.0.0.1", Informers: informers})
	s.backends.allowed = func(netip.Addr) bool { return true }
	s.acceptFor, s.grace = 0, 100*time.Millisecond
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
		{name: "removed since the pause, which recorded one", want: true,
			workload: paused("", map[string]string{v1alpha1.AnnotationLastRequest: before})},
		{name: "emptied since the pause, which recorded one", want: true, workload: func() *v1alpha1.ManagedWorkload {
			w := paused("", map[string]string{v1alpha1.AnnotationLastRequest: before})
			w.Annotations = map[string]string{v1alpha1.AnnotationLastRequest: ""}
			return w
		}()},
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

// Someone removed the last-request annotation the pause recorded. A request
// must still wake the workload, with a value the operator sees as new.
func TestServer_WakesAfterTheRecordedStampWasRemoved(t *testing.T) {
	tests := []struct {
		name     string
		recorded string
	}{
		{name: "recorded earlier", recorded: "2026-10-03T08:00:00Z"},
		{name: "recorded in the same second", recorded: "2026-10-03T09:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 10, 3, 9, 0, 0, 500, time.UTC)
			w := pausedWorkload("api", freePort(t), time.Minute)
			w.Status.Pause.WakeAnnotations = &v1alpha1.WakeAnnotations{
				Workload: map[string]string{v1alpha1.AnnotationLastRequest: tt.recorded},
			}
			s, c := newTestServer(t, nil, w)
			s.now = func() time.Time { return now }

			require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

			stamped := lastRequest(t, c)
			assert.NotEmpty(t, stamped)
			assert.NotEqual(t, tt.recorded, stamped, "the operator wakes only for a value other than the recorded one")
		})
	}
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
	assert.Equal(t, 0, wokenEvents(recorder))
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

func wokenEvents(r *events.FakeRecorder) int {
	var n int
	for len(r.Events) > 0 {
		if strings.Contains(<-r.Events, "WokenByRequest") {
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

	assert.Equal(t, 1, wokenEvents(first))
	assert.Equal(t, 0, wokenEvents(second))
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

func proxiedCount(s *Server) int {
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	return s.admission.proxied
}

// Connections to an awake workload are capped apart from held ones, so a
// caller opening them as fast as it can doesn't run the doorman out of
// memory. One over the cap is held until a place frees.
func TestServer_ProxiedConnectionsAreCapped(t *testing.T) {
	port := freePort(t)
	s, _ := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.maxProxied = 1
		s.admission = newAdmission(l)
	}, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))

	first := dial(t, port)
	send(t, first, "first\n")
	echoed(t, first, "first\n")
	require.Equal(t, 1, proxiedCount(s))

	second := dial(t, port)
	send(t, second, "second\n")
	require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond,
		"a connection over the cap is held")
	require.NoError(t, second.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err := second.Read(make([]byte, 1))
	require.Error(t, err, "nothing is passed through over the cap")

	require.NoError(t, first.Close())
	echoed(t, second, "second\n")
	assert.Zero(t, heldCount(s))
}

func TestAdmission_ProxiedConnectionsAreCappedPerSource(t *testing.T) {
	l := defaultLimits
	l.maxProxied, l.maxProxiedPerSource = 3, 2
	a := newAdmission(l)
	now := time.Now()
	other := netip.MustParseAddr("10.244.0.18")

	require.True(t, a.proxy(caller, now))
	require.True(t, a.proxy(caller, now))
	assert.False(t, a.proxy(caller, now), "one source can't take every place")
	require.True(t, a.proxy(other, now))
	assert.False(t, a.proxy(other, now), "the overall cap holds")

	a.unproxy(caller, now)
	assert.True(t, a.proxy(other, now))
}

// A proxied connection that carries nothing either way for proxyIdle is
// closed, so half-open ones don't pile up. One that carries something in
// either direction stays open.
func TestServer_ClosesIdleProxiedConnections(t *testing.T) {
	port := freePort(t)
	s, _ := startServerWith(t, func(s *Server) {
		s.proxyIdle, s.proxyCheck = 400*time.Millisecond, 20*time.Millisecond
	}, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))

	active := dial(t, port)
	idle := dial(t, port)
	send(t, idle, "hello\n")
	echoed(t, idle, "hello\n")
	for range 8 {
		send(t, active, "ping\n")
		echoed(t, active, "ping\n")
		time.Sleep(100 * time.Millisecond)
	}

	closedWithin(t, idle)
	send(t, active, "still\n")
	echoed(t, active, "still\n")
	assert.Eventually(t, func() bool { return proxiedCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)
}

// Once the workload is awake and its route drains, the connections still
// proxied through the doorman are closed after drainedLifetime, so callers
// with long-lived connections reconnect to its pods directly.
func TestServer_ClosesProxiedConnectionsOnceTheRouteDrains(t *testing.T) {
	port := freePort(t)
	s, c := startServerWith(t, func(s *Server) {
		s.drainedLifetime, s.proxyCheck = 200*time.Millisecond, 20*time.Millisecond
	}, pausedWorkload("api", port, time.Minute), readySlice(echoPort(t)))

	conn := dial(t, port)
	send(t, conn, "hello\n")
	echoed(t, conn, "hello\n")
	time.Sleep(300 * time.Millisecond)
	send(t, conn, "routed\n")
	echoed(t, conn, "routed\n")

	removeRoute(t, s, c)
	start := time.Now()
	for time.Since(start) < 150*time.Millisecond {
		send(t, conn, "ping\n")
		echoed(t, conn, "ping\n")
	}
	closedWithin(t, conn)
}

func refused(t *testing.T, port int32) bool {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	if err == nil {
		_ = conn.Close()
	}
	return err != nil
}

// A port given to another workload while its old route drains is served
// for neither until the drain is over, however many syncs that takes:
// traffic still sent to it for the old workload must not reach the new
// one's pods, which may be in another namespace.
func TestServer_APortReassignedWhileDrainingWaitsForTheDrain(t *testing.T) {
	port := freePort(t)
	clock := &testClock{now: time.Now()}
	s, c := startServerWith(t, func(s *Server) { s.now = clock.Now }, pausedWorkload("api", port, time.Minute))
	ctx := context.Background()
	removeRoute(t, s, c)

	other := pausedWorkload("billing", port, time.Minute)
	other.Namespace = "other-team"
	require.NoError(t, c.Create(ctx, other))
	for range 3 {
		clock.Advance(routeSyncInterval)
		require.NoError(t, s.syncRoutes(ctx, ctx))
		assert.True(t, refused(t, port), "the port is served for neither while the old route drains")
		assert.Equal(t, float64(1), testutil.ToFloat64(opmetrics.DoormanPortConflicts))
	}

	clock.Advance(RouteDrain)
	require.NoError(t, s.syncRoutes(ctx, ctx))
	dial(t, port)
	assert.Zero(t, testutil.ToFloat64(opmetrics.DoormanPortConflicts))
}

// A port the same workload moves to another of its Services drains like
// any other change: the old Service's in-flight traffic must not reach the
// new Service's pods.
func TestServer_APortMovedToAnotherServiceDrains(t *testing.T) {
	port := freePort(t)
	clock := &testClock{now: time.Now()}
	s, c := startServerWith(t, func(s *Server) { s.now = clock.Now }, pausedWorkload("api", port, time.Minute))
	ctx := context.Background()

	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(ctx, apiKey, &w))
	w.Status.Doorman[0].Service = "api-v2"
	require.NoError(t, c.Update(ctx, &w))
	require.NoError(t, s.syncRoutes(ctx, ctx))
	assert.True(t, refused(t, port), "the port is served for neither while the old route drains")

	clock.Advance(RouteDrain)
	require.NoError(t, s.syncRoutes(ctx, ctx))
	dial(t, port)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, "api-v2", s.routes[port].service)
}

// A workload that takes its own draining route back, as when it's paused
// again moments after waking, is served again at once.
func TestServer_ARouteTakenBackStopsDraining(t *testing.T) {
	port := freePort(t)
	s, c := startServer(t, pausedWorkload("api", port, time.Minute))
	ctx := context.Background()
	removeRoute(t, s, c)

	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(ctx, apiKey, &w))
	w.Status.Doorman = pausedWorkload("api", port, time.Minute).Status.Doorman
	require.NoError(t, c.Update(ctx, &w))
	require.NoError(t, s.syncRoutes(ctx, ctx))

	dial(t, port)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.False(t, s.routes[port].draining())
	assert.Empty(t, s.drains)
}

// Callers who leave are noticed even once large bodies have spent the
// shared buffer budget: one that sent less than a chunk is read to its EOF
// without the budget, and one blocked on it is read again once it frees.
func TestServer_CallersLeavingAreNoticedWhenTheBufferBudgetIsSpent(t *testing.T) {
	port := freePort(t)
	s, _ := startServerWith(t, func(s *Server) {
		l := defaultLimits
		l.maxBuffered = 4 * clientBufferSize
		s.admission = newAdmission(l)
	}, pausedWorkload("api", port, time.Minute))
	upload := "POST /upload HTTP/1.1\r\nHost: shop\r\nContent-Length: 36864\r\n\r\n" + strings.Repeat("x", 36<<10)
	before := wakes(resultCanceled)

	large := dial(t, port)
	var writes sync.WaitGroup
	writes.Go(func() { _, _ = large.Write([]byte(upload)) }) // ends when the doorman closes the connection
	require.Eventually(t, func() bool { return bufferedBytes(s) == 4*clientBufferSize }, 5*time.Second, 10*time.Millisecond,
		"the budget is spent")
	blocked := dial(t, port)
	writes.Go(func() { _, _ = blocked.Write([]byte(upload)) }) // ends when the doorman closes the connection

	small := dial(t, port)
	send(t, small, "GET /api HTTP/1.1\r\nHost: api\r\n\r\n")
	require.Eventually(t, func() bool { return heldCount(s) == 3 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, small.Close())
	require.Eventually(t, func() bool { return wakes(resultCanceled) == before+1 }, 5*time.Second, 10*time.Millisecond,
		"a caller that sent less than a chunk is noticed leaving")

	require.NoError(t, large.Close())
	require.Eventually(t, func() bool { return wakes(resultCanceled) == before+2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, blocked.Close())
	require.Eventually(t, func() bool { return wakes(resultCanceled) == before+3 }, 5*time.Second, 10*time.Millisecond,
		"a caller waiting on the budget is read again once it frees")
	assert.Eventually(t, func() bool { return heldCount(s) == 0 && bufferedBytes(s) == 0 }, 5*time.Second, 10*time.Millisecond)
	writes.Wait()
}

// The buffer budget counts the memory a held connection keeps, in whole
// chunks, not just the bytes in them.
func TestServer_TheBufferBudgetCountsWholeChunks(t *testing.T) {
	port := freePort(t)
	s, _ := startServer(t, pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)
	send(t, conn, "POST /upload HTTP/1.1\r\nHost: shop\r\nContent-Length: 12288\r\n\r\n"+strings.Repeat("x", 12288))
	require.Eventually(t, func() bool { return heldCount(s) == 1 }, 5*time.Second, 10*time.Millisecond)

	assert.Eventually(t, func() bool { return bufferedBytes(s) == clientBufferSize }, 5*time.Second, 10*time.Millisecond,
		"a second chunk, though barely used, counts in full")
}

// The doorman's cache answers from memory however long its watches have
// been failing, so readiness also needs the API server to answer: without
// it, the routes it serves may be stale.
func TestServer_ReadinessFollowsTheAPIServer(t *testing.T) {
	clock := &testClock{now: time.Now()}
	var down atomic.Bool
	s, _ := newTestServer(t, nil, pausedWorkload("api", freePort(t), time.Minute))
	s.now = clock.Now
	s.checkAPI = func(context.Context) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		for _, l := range s.listeners {
			_ = l.Close()
		}
	})
	require.NoError(t, s.syncRoutes(ctx, ctx))
	assert.Error(t, s.Ready(nil), "not Ready before the API server is reached")

	wg.Go(func() { s.watchAPI(ctx) })
	require.Eventually(t, func() bool { return s.Ready(nil) == nil }, 5*time.Second, 10*time.Millisecond)

	down.Store(true)
	clock.Advance(readyStaleness + time.Second)
	require.NoError(t, s.syncRoutes(ctx, ctx), "the cache still answers")
	assert.ErrorContains(t, s.Ready(nil), "API server last reached")
}

// A doorman that can't listen on a route's port refuses every connection
// to it. After listenGrace, it stops being Ready, so the operator routes
// paused workloads to doorman pods that can serve them.
func TestServer_ReadinessFollowsListenFailures(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })
	port := int32(taken.Addr().(*net.TCPAddr).Port)
	clock := &testClock{now: time.Now()}
	s, c := newTestServer(t, nil, pausedWorkload("api", port, time.Minute))
	s.now = clock.Now
	ctx := context.Background()
	t.Cleanup(func() {
		for _, l := range s.listeners {
			_ = l.Close()
		}
	})

	require.Error(t, s.syncRoutes(ctx, ctx))
	assert.NoError(t, s.Ready(nil), "a moment's failure is retried")
	assert.Equal(t, float64(1), testutil.ToFloat64(opmetrics.DoormanListenFailures))

	clock.Advance(listenGrace + time.Second)
	require.Error(t, s.syncRoutes(ctx, ctx))
	assert.ErrorContains(t, s.Ready(nil), "can't listen on doorman port")

	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(ctx, apiKey, &w))
	w.Status.Doorman = nil
	require.NoError(t, c.Update(ctx, &w))
	require.Error(t, s.syncRoutes(ctx, ctx), "a draining route is still served")
	clock.Advance(RouteDrain)
	require.NoError(t, s.syncRoutes(ctx, ctx))
	assert.NoError(t, s.Ready(nil), "a port no longer routed doesn't count")
	assert.Zero(t, testutil.ToFloat64(opmetrics.DoormanListenFailures))
}

// hangPatches makes every patch wait for release or its context, like an
// API server that has stopped answering.
func hangPatches(release <-chan struct{}) func(client.WithWatch) client.WithWatch {
	return func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch,
			obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			select {
			case <-release:
				return cl.Patch(ctx, obj, patch, opts...)
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	}
}

// A stamp waiting on an API server that doesn't answer is cut short when
// shutdown closes the connections, so the doorman stops within its grace
// period rather than apiTimeout later.
func TestServer_ShutdownDoesNotWaitOutAStamp(t *testing.T) {
	port := freePort(t)
	s, _ := newTestServer(t, hangPatches(nil), pausedWorkload("api", port, time.Minute))
	stop := run(t, s)
	conn := dial(t, port)
	send(t, conn, "hello\n")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.wakeCalls) == 1
	}, 5*time.Second, 10*time.Millisecond, "the stamp is under way")

	start := time.Now()
	stop()
	assert.Less(t, time.Since(start), apiTimeout/2)
}

// A browser gets the waking-up page at once, however slow the API server
// is to take the wake, which goes on without it.
func TestServer_ServesThePageWithoutWaitingForTheWake(t *testing.T) {
	port := freePort(t)
	release := make(chan struct{})
	s, c := newTestServer(t, hangPatches(release), pausedWorkload("api", port, time.Minute))
	run(t, s)

	conn := dial(t, port)
	start := time.Now()
	send(t, conn, browserGET)
	resp, _ := readResponse(t, conn)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Less(t, time.Since(start), time.Second, "the page doesn't wait on the stamp")
	assert.Empty(t, lastRequest(t, c))

	close(release)
	assert.Eventually(t, func() bool { return lastRequest(t, c) != "" }, 5*time.Second, 20*time.Millisecond,
		"the wake lands once the API server answers")
}

// staleOnce answers the first Get with the workload as it was when made,
// and every later one with the workload as it is.
func staleOnce(t *testing.T, c client.Client) interceptor.Funcs {
	t.Helper()
	var snapshot v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &snapshot))
	var served atomic.Bool
	return interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if served.CompareAndSwap(false, true) {
			snapshot.DeepCopyInto(obj.(*v1alpha1.ManagedWorkload))
			return nil
		}
		return cl.Get(ctx, key, obj, opts...)
	}}
}

// A conflict from a change other than another replica's stamp, such as
// the operator writing status, doesn't cost the wake its event.
func TestServer_AnnouncesAWakeDespiteAConflictFromAnotherWriter(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pausedWorkload("api", freePort(t), time.Minute)).Build()
	stale := interceptor.NewClient(c, staleOnce(t, c))
	var w v1alpha1.ManagedWorkload
	require.NoError(t, c.Get(context.Background(), apiKey, &w))
	w.Labels = map[string]string{"team": "payments"}
	require.NoError(t, c.Update(context.Background(), &w))

	recorder := events.NewFakeRecorder(10)
	s := NewServer(stale, recorder, Options{})
	require.NoError(t, s.stamp(context.Background(), route{workload: apiKey, service: "api"}, caller))

	assert.Equal(t, 1, wokenEvents(recorder))
	assert.NotEmpty(t, lastRequest(t, c))
}

// A replica that loses the race to another's stamp leaves it be, and
// doesn't announce the wake again.
func TestServer_LeavesAnotherReplicasStamp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pausedWorkload("api", freePort(t), time.Minute)).Build()
	stale := interceptor.NewClient(c, staleOnce(t, c))
	rt := route{workload: apiKey, service: "api"}

	first := NewServer(c, nil, Options{})
	first.now = func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) }
	require.NoError(t, first.stamp(context.Background(), rt, caller))
	stamped := lastRequest(t, c)

	recorder := events.NewFakeRecorder(10)
	second := NewServer(stale, recorder, Options{})
	second.now = func() time.Time { return time.Date(2026, 10, 3, 9, 0, 1, 0, time.UTC) }
	require.NoError(t, second.stamp(context.Background(), rt, netip.MustParseAddr("10.244.0.18")))

	assert.Equal(t, stamped, lastRequest(t, c))
	assert.Zero(t, wokenEvents(recorder))
}

func TestStartsMethod(t *testing.T) {
	tests := []struct {
		head string
		want bool
	}{
		{head: "G", want: true},
		{head: "GE", want: true},
		{head: "GET", want: true},
		{head: "OPTIO", want: true},
		{head: "GET "},
		{head: "GET /"},
		{head: "GX"},
		{head: "\x16\x03"},
		{head: ""},
	}
	for _, tt := range tests {
		t.Run(strconv.Quote(tt.head), func(t *testing.T) {
			assert.Equal(t, tt.want, startsMethod([]byte(tt.head)))
		})
	}
}

// A request whose first segment stops short of its method's space, as a
// proxy or a slow link can split it, is still read as HTTP, so a browser
// gets the page rather than being held.
func TestServer_ReadsARequestSplitInsideItsMethod(t *testing.T) {
	port := freePort(t)
	startServer(t, pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)
	send(t, conn, browserGET[:2])
	time.Sleep(100 * time.Millisecond)
	send(t, conn, browserGET[2:])

	resp, _ := readResponse(t, conn)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
