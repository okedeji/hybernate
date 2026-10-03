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

package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	restfake "k8s.io/client-go/rest/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// series is one pod container's CPU, in cores, at each step from start.
type series struct {
	pod, container string
	cores          func(t time.Time) float64
}

// fakePrometheus answers the queries a scan makes, from series per
// namespace, recording the range queries it was asked.
type fakePrometheus struct {
	byNamespace map[string][]series
	// from is where the history it keeps begins.
	from    time.Time
	queries []string
}

func (f *fakePrometheus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type result struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	resp := map[string]any{"status": "success"}
	switch {
	case strings.HasSuffix(r.URL.Path, "/api/v1/query"):
		resp["data"] = map[string]any{"resultType": "vector", "result": []any{}}
	case strings.HasSuffix(r.URL.Path, "/api/v1/query_range"):
		query := r.URL.Query()
		f.queries = append(f.queries, query.Get("query"))
		start, _ := strconv.ParseInt(query.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(query.Get("end"), 10, 64)
		step, _ := strconv.ParseInt(query.Get("step"), 10, 64)
		var results []result
		for ns, list := range f.byNamespace {
			if !strings.Contains(query.Get("query"), `namespace="`+ns+`"`) {
				continue
			}
			for _, s := range list {
				res := result{Metric: map[string]string{"pod": s.pod, "container": s.container}}
				for at := start; at <= end; at += step {
					t := time.Unix(at, 0)
					if t.Before(f.from) {
						continue
					}
					res.Values = append(res.Values, [2]any{float64(at), strconv.FormatFloat(s.cores(t), 'f', -1, 64)})
				}
				results = append(results, res)
			}
		}
		resp["data"] = map[string]any{"resultType": "matrix", "result": results}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func newFakePrometheus(t *testing.T, f *fakePrometheus) *Prometheus {
	t.Helper()
	server := httptest.NewServer(http.StripPrefix("/thanos", f))
	t.Cleanup(server.Close)
	p, err := NewPrometheusURL(server.URL+"/thanos", server.Client())
	require.NoError(t, err)
	return p
}

func TestPrometheusURL_ReadsANamespacesCPU(t *testing.T) {
	f := &fakePrometheus{byNamespace: map[string][]series{
		"sandbox": {{pod: "api-7d9f8c6b5-abcde", container: "app", cores: func(time.Time) float64 { return 0.25 }}},
	}}
	p := newFakePrometheus(t, f)
	end := scanTime.Truncate(minStep)

	history, err := p.namespaceCPU(context.Background(), "sandbox", end.Add(-time.Hour), end, minStep)

	require.NoError(t, err, "the path prefix in the URL is kept")
	require.Len(t, history, 1)
	assert.Equal(t, "api-7d9f8c6b5-abcde", history[0].pod)
	assert.Len(t, history[0].samples, 13)
	assert.InDelta(t, 0.25, history[0].samples[end.Unix()], 0.0001)
	require.Len(t, f.queries, 1)
	assert.Contains(t, f.queries[0], `namespace="sandbox"`)
	assert.Contains(t, f.queries[0], `container!="POD"`, "the pause container isn't the workload's")
}

func TestPrometheusURL_Errors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"an error status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, "status 502"},
		{"a failed query", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"status":"error","error":"query timed out"}`)
		}, "query timed out"},
		{"not Prometheus", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<html>`)
		}, "reading"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			t.Cleanup(server.Close)
			p, err := NewPrometheusURL(server.URL, server.Client())
			require.NoError(t, err)

			_, err = p.namespaceCPU(context.Background(), "sandbox", scanTime.Add(-time.Hour), scanTime, minStep)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Error(t, p.Check(context.Background()))
		})
	}
}

func TestNewPrometheusURL_RejectsARelativeURL(t *testing.T) {
	_, err := NewPrometheusURL("prometheus:9090", http.DefaultClient)
	assert.Error(t, err)
}

func service(namespace, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

func TestFindPrometheus(t *testing.T) {
	operated := service("monitoring", "prometheus-operated", map[string]string{"operated-prometheus": "true"},
		corev1.ServicePort{Name: "web", Port: 9090})
	community := service("prom", "prometheus-server",
		map[string]string{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"},
		corev1.ServicePort{Port: 80})
	unrelated := service("monitoring", "grafana", map[string]string{"app.kubernetes.io/name": "grafana"})

	tests := []struct {
		name       string
		services   []client.Object
		forbidden  bool
		wantSource string
		wantErr    error
	}{
		{name: "the Prometheus Operator's", services: []client.Object{community, operated, unrelated},
			wantSource: "monitoring/prometheus-operated"},
		{name: "the community chart's", services: []client.Object{community, unrelated},
			wantSource: "prom/prometheus-server"},
		{name: "none", services: []client.Object{unrelated}, wantErr: ErrNoPrometheus},
		{name: "in a monitoring namespace when Services can't be listed everywhere",
			services: []client.Object{operated}, forbidden: true, wantSource: "monitoring/prometheus-operated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithObjects(tt.services...)
			if tt.forbidden {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						var o client.ListOptions
						o.ApplyOptions(opts)
						if o.Namespace == "" {
							return apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
						}
						return c.List(ctx, list, opts...)
					},
				})
			}

			p, err := FindPrometheus(context.Background(), builder.Build(), nil, []string{"sandbox"})

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, p.Source)
		})
	}
}

func TestPrometheusProxy_Forbidden(t *testing.T) {
	restClient := &restfake.RESTClient{
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		Client: restfake.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))}, nil
		}),
	}
	p := NewPrometheusProxy(restClient, "monitoring", "prometheus-operated", "web")

	err := p.Check(context.Background())

	var forbidden *ProxyForbiddenError
	require.ErrorAs(t, err, &forbidden)
	assert.Equal(t, ProxyForbiddenError{Namespace: "monitoring", Service: "prometheus-operated", Port: "web"}, *forbidden)
}

func TestPrometheusProxy_GoesThroughTheAPIServer(t *testing.T) {
	var gotPath, gotQuery string
	restClient := &restfake.RESTClient{
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			gotPath, gotQuery = req.URL.Path, req.URL.Query().Get("query")
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[]}}`))}, nil
		}),
	}
	p := NewPrometheusProxy(restClient, "monitoring", "prometheus-operated", "web")

	require.NoError(t, p.Check(context.Background()))

	assert.Equal(t, "/api/v1/namespaces/monitoring/services/prometheus-operated:web/proxy/api/v1/query", gotPath)
	assert.Equal(t, "vector(1)", gotQuery)
}
