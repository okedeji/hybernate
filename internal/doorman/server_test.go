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
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

func readySlice(service string, port int32) *discoveryv1.EndpointSlice {
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
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	s := NewServer(c, nil, "127.0.0.1")
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
		return w.Annotations[v1alpha1.AnnotationLastActivity] != ""
	}, 5*time.Second, 20*time.Millisecond, "the held connection must stamp the wake annotation")

	// The workload comes up: its real endpoints appear.
	require.NoError(t, c.Create(context.Background(), readySlice("api", echoServer(t))))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "hello\n", line, "bytes sent while held reach the woken workload")
}

func TestServer_IgnoresItsOwnSlice(t *testing.T) {
	port := freePort(t)
	own := readySlice("api", port)
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
	starting := readySlice("api", echoServer(t))
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

func TestServer_StopsListeningWhenRouteRemoved(t *testing.T) {
	port := freePort(t)
	w := pausedWorkload("api", port, time.Minute)
	s, c := startServer(t, w)
	dial(t, port)

	w.Status.Doorman = nil
	require.NoError(t, c.Update(context.Background(), w))
	require.NoError(t, s.syncRoutes(context.Background()))

	_, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), time.Second)
	assert.Error(t, err, "an awake workload's port is released")
}
