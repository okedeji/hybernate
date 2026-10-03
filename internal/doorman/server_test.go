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
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/okedeji/hybernate/api/v1alpha1"
	opmetrics "github.com/okedeji/hybernate/internal/metrics"
)

// freePort returns a port nothing is listening on, for the doorman to claim.
func freePort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return int32(port)
}

// echoServer is the woken workload: it echoes lines back.
func echoServer(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return int32(l.Addr().(*net.TCPAddr).Port)
}

func pausedWorkload(name string, doormanPort int32, maxWait time.Duration) *v1alpha1.ManagedWorkload {
	return &v1alpha1.ManagedWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sandbox"},
		Spec: v1alpha1.ManagedWorkloadSpec{
			Target: v1alpha1.WorkloadRef{Kind: v1alpha1.TargetKindDeployment, Name: name},
			Wake:   &v1alpha1.WakeSpec{MaxWait: &metav1.Duration{Duration: maxWait}},
		},
		Status: v1alpha1.ManagedWorkloadStatus{
			Phase:   v1alpha1.PhasePaused,
			Doorman: []v1alpha1.DoormanRoute{{Service: name, PortName: "http", DoormanPort: doormanPort}},
		},
	}
}

// readySlice is the "api" Service's own slice, with one Ready pod on port.
func readySlice(port int32) *discoveryv1.EndpointSlice {
	const service = "api"
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: service + "-real", Namespace: "sandbox",
			Labels: map[string]string{discoveryv1.LabelServiceName: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"127.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(port)}},
	}
}

func startServer(t *testing.T, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	return startServerWithRecorder(t, nil, objs...)
}

func startServerWithRecorder(t *testing.T, recorder events.EventRecorder, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	return startServerWith(t, func(s *Server) { s.recorder = recorder }, objs...)
}

// startServerWith starts a doorman after configure has set it up.
func startServerWith(t *testing.T, configure func(*Server), objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	s := NewServer(c, nil, "127.0.0.1")
	configure(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return s.Ready(nil) == nil }, 5*time.Second, 10*time.Millisecond)
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

func TestServer_HoldsWakesAndPassesThrough(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))

	conn := dial(t, port)
	_, err := conn.Write([]byte("hello\n"))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var w v1alpha1.ManagedWorkload
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "sandbox", Name: "api"}, &w))
		return w.Annotations[v1alpha1.AnnotationLastRequest] != ""
	}, 5*time.Second, 20*time.Millisecond, "the held connection must stamp the request annotation")

	// The workload comes up: its real endpoints appear.
	require.NoError(t, c.Create(context.Background(), readySlice(echoServer(t))))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "hello\n", line, "bytes sent while held reach the woken workload")
}

func TestServer_IgnoresItsOwnSlice(t *testing.T) {
	port := freePort(t)
	own := readySlice(port)
	own.Name = SliceName("api")
	own.Labels[discoveryv1.LabelManagedBy] = ManagedBy
	s, _ := startServer(t, pausedWorkload("api", port, time.Minute), own)

	addr, err := s.readyBackend(context.Background(), route{
		workload: types.NamespacedName{Namespace: "sandbox", Name: "api"}, service: "api", portName: "http",
	})

	require.NoError(t, err)
	assert.Empty(t, addr, "the doorman must never pass a connection back to itself")
}

func TestServer_WaitsForAReadyPod(t *testing.T) {
	port := freePort(t)
	starting := readySlice(echoServer(t))
	starting.Endpoints[0].Conditions.Ready = ptr.To(false)
	s, _ := startServer(t, pausedWorkload("api", port, time.Minute), starting)

	addr, err := s.readyBackend(context.Background(), route{
		workload: types.NamespacedName{Namespace: "sandbox", Name: "api"}, service: "api", portName: "http",
	})

	require.NoError(t, err)
	assert.Empty(t, addr, "a pod that's still starting would refuse the connection")
}

func TestServer_ClosesAfterMaxWait(t *testing.T) {
	port := freePort(t)
	startServer(t, pausedWorkload("slow", port, 300*time.Millisecond))
	before := testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("sandbox", "slow", "timeout"))

	conn := dial(t, port)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := conn.Read(make([]byte, 1))

	assert.ErrorIs(t, err, io.EOF, "a workload that isn't Ready within maxWait gets its connection closed")
	assert.Equal(t, before+1, testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("sandbox", "slow", "timeout")))
}

// Every connection waiting on a slow wake times out together, so they share
// one warning.
func TestServer_WarnsOnceWhenRequestsAreNotServed(t *testing.T) {
	port := freePort(t)
	recorder := events.NewFakeRecorder(10)
	startServerWithRecorder(t, recorder, pausedWorkload("slow", port, 300*time.Millisecond))

	for range 3 {
		conn := dial(t, port)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err := conn.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
	}

	var warnings []string
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, "RequestNotServed") {
			warnings = append(warnings, e)
		}
	}
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "Service slow")
	assert.Contains(t, warnings[0], "maxWait")
}

const browserGET = "GET /orders HTTP/1.1\r\nHost: api\r\nAccept: text/html\r\nSec-Fetch-Mode: navigate\r\n\r\n"

func TestServer_ServesThePageToABrowser(t *testing.T) {
	port := freePort(t)
	w := pausedWorkload("api", port, time.Minute)
	w.Status.Pause = &v1alpha1.PauseStatus{PausedAt: ptr.To(metav1.NewTime(time.Now().Add(-3 * time.Hour)))}
	_, c := startServer(t, w)
	before := testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("sandbox", "api", "page"))

	conn := dial(t, port)
	_, err := conn.Write([]byte(browserGET))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "answered at once, with no pod Ready yet")
	assert.Contains(t, string(body), "<title>Waking up api</title>")
	assert.Contains(t, string(body), "<header>api</header>", "the address the browser asked for")
	assert.Contains(t, string(body), "<dd>3 hours ago</dd>", "how long it was paused")
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "sandbox", Name: "api"}, w))
	assert.NotEmpty(t, w.Annotations[v1alpha1.AnnotationLastRequest], "the page still wakes the workload")
	assert.Equal(t, before+1, testutil.ToFloat64(opmetrics.DoormanWakes.WithLabelValues("sandbox", "api", "page")))
}

// In the moment between a pod being Ready and the operator routing traffic
// back to it, the page says the workload is ready and reloads into it.
func TestServer_PageSaysReadyOnceAPodIsReady(t *testing.T) {
	port := freePort(t)
	_, c := startServer(t, pausedWorkload("api", port, time.Minute))
	require.NoError(t, c.Create(context.Background(), readySlice(echoServer(t))))

	conn := dial(t, port)
	_, err := conn.Write([]byte(browserGET))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), "<title>api is ready</title>")
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := freePort(t)
			w := pausedWorkload("api", port, time.Minute)
			w.Spec.Wake.Page = tt.page
			_, c := startServer(t, w)

			conn := dial(t, port)
			_, err := conn.Write([]byte(tt.request))
			require.NoError(t, err)
			require.NoError(t, c.Create(context.Background(), readySlice(echoServer(t))))

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			got := make([]byte, len(tt.request))
			_, err = io.ReadFull(conn, got)
			require.NoError(t, err)
			assert.Equal(t, tt.request, string(got))
		})
	}
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

// Proxies keep sending to the doorman for a moment after the workload is
// awake, so its port drains, passing connections straight to the workload,
// before it's released.
func TestServer_DrainsARemovedRouteThenReleasesIt(t *testing.T) {
	port := freePort(t)
	clock := &testClock{now: time.Now()}
	w := pausedWorkload("api", port, time.Minute)
	s, c := startServerWith(t, func(s *Server) { s.now = clock.Now }, w)
	ctx := context.Background()

	require.NoError(t, c.Create(ctx, readySlice(echoServer(t))))
	w.Status.Doorman = nil
	require.NoError(t, c.Update(ctx, w))
	require.NoError(t, s.syncRoutes(ctx))

	conn := dial(t, port)
	_, err := conn.Write([]byte(browserGET))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	got := make([]byte, len(browserGET))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, browserGET, string(got), "a browser reaches the awake workload, not the page")

	clock.Advance(routeDrain)
	require.NoError(t, s.syncRoutes(ctx))
	_, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	assert.Error(t, err, "the port is released once it has drained")
}
