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
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	p, err := NewPrometheusURL(server.URL+"/thanos", server.Client(), nil)
	require.NoError(t, err)
	return p
}

func TestPrometheusURL_ReadsANamespacesCPU(t *testing.T) {
	f := &fakePrometheus{byNamespace: map[string][]series{
		"dev": {{pod: "api-7d9f8c6b5-abcde", container: "app", cores: func(time.Time) float64 { return 0.25 }}},
	}}
	p := newFakePrometheus(t, f)
	end := scanTime.Truncate(minStep)

	history, err := p.namespaceCPU(context.Background(), "dev", end.Add(-time.Hour), end, minStep)

	require.NoError(t, err, "the path prefix in the URL is kept")
	require.Len(t, history, 1)
	assert.Equal(t, "api-7d9f8c6b5-abcde", history[0].pod)
	assert.Len(t, history[0].samples, 13)
	assert.InDelta(t, 0.25, history[0].samples[end.Unix()], 0.0001)
	require.Len(t, f.queries, 1)
	assert.Contains(t, f.queries[0], `namespace="dev"`)
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
			p, err := NewPrometheusURL(server.URL, server.Client(), nil)
			require.NoError(t, err)

			_, err = p.namespaceCPU(context.Background(), "dev", scanTime.Add(-time.Hour), scanTime, minStep)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Error(t, p.Check(context.Background()))
		})
	}
}

func TestNewPrometheusURL_RejectsARelativeURL(t *testing.T) {
	_, err := NewPrometheusURL("prometheus:9090", http.DefaultClient, nil)
	assert.Error(t, err)
}

func service(namespace, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

// proxyAnswers is an API server's service proxy, answering each Service,
// by the name the proxy path gives it, with a status; 200 answers like a
// Prometheus API.
func proxyAnswers(answers map[string]int) (*restfake.RESTClient, *[]string) {
	var asked []string
	return &restfake.RESTClient{
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			parts := strings.Split(req.URL.Path, "/")
			name := parts[4] + "/" + parts[6]
			asked = append(asked, name)
			switch status := answers[name]; status {
			case http.StatusOK:
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[]}}`))}, nil
			case http.StatusForbidden:
				return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))}, nil
			default:
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader("no endpoints available"))}, nil
			}
		}),
	}, &asked
}

func TestFindPrometheus(t *testing.T) {
	operated := service("monitoring", "prometheus-operated", map[string]string{"operated-prometheus": "true"},
		corev1.ServicePort{Name: "web", Port: 9090})
	community := service("prom", "prometheus-server",
		map[string]string{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"},
		corev1.ServicePort{Port: 80})
	unrelated := service("monitoring", "grafana", map[string]string{"app.kubernetes.io/name": "grafana"})
	// Alphabetically first, but a team's own Prometheus, not the cluster's.
	teams := service("a-team", "prometheus-operated", map[string]string{"operated-prometheus": "true"},
		corev1.ServicePort{Name: "web", Port: 9090})
	tlsOnly := service("secure", "prometheus-server",
		map[string]string{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"},
		corev1.ServicePort{Name: "https", Port: 9091})
	all := map[string]int{"monitoring/prometheus-operated:web": 200, "prom/prometheus-server:80": 200,
		"a-team/prometheus-operated:web": 200, "secure/https:prometheus-server:https": 200}

	tests := []struct {
		name       string
		services   []client.Object
		answers    map[string]int
		forbidden  bool
		wantSource string
		wantErr    error
		wantAsk    []string
	}{
		{name: "the Prometheus Operator's", services: []client.Object{community, operated, unrelated}, answers: all,
			wantSource: "monitoring/prometheus-operated"},
		{name: "the community chart's", services: []client.Object{community, unrelated}, answers: all,
			wantSource: "prom/prometheus-server"},
		{name: "none", services: []client.Object{unrelated}, wantErr: ErrNoPrometheus},
		{name: "in a monitoring namespace when Services can't be listed everywhere",
			services: []client.Object{operated}, forbidden: true, answers: all, wantSource: "monitoring/prometheus-operated"},
		{name: "the monitoring namespace's before one sorted first", services: []client.Object{teams, operated},
			answers: all, wantSource: "monitoring/prometheus-operated"},
		{name: "the next when the best doesn't answer", services: []client.Object{teams, operated},
			answers:    map[string]int{"a-team/prometheus-operated:web": 200},
			wantSource: "a-team/prometheus-operated",
			wantAsk:    []string{"monitoring/prometheus-operated:web", "a-team/prometheus-operated:web"}},
		{name: "over TLS through the proxy's https scheme", services: []client.Object{tlsOnly}, answers: all,
			wantSource: "secure/prometheus-server", wantAsk: []string{"secure/https:prometheus-server:https"}},
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
			restClient, asked := proxyAnswers(tt.answers)

			p, err := FindPrometheus(context.Background(), builder.Build(), restClient, []string{"dev"})

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSource, p.Source)
			if tt.wantAsk != nil {
				assert.Equal(t, tt.wantAsk, *asked)
			}
		})
	}
}

// When every Prometheus refuses the user, the error names one, so the scan
// can say what access to ask for.
func TestFindPrometheus_AllForbidden(t *testing.T) {
	operated := service("monitoring", "prometheus-operated", map[string]string{"operated-prometheus": "true"},
		corev1.ServicePort{Name: "web", Port: 9090})
	restClient, _ := proxyAnswers(map[string]int{"monitoring/prometheus-operated:web": http.StatusForbidden})

	_, err := FindPrometheus(context.Background(), fake.NewClientBuilder().WithObjects(operated).Build(), restClient, nil)

	var forbidden *ProxyForbiddenError
	require.ErrorAs(t, err, &forbidden)
	assert.Equal(t, "prometheus-operated:web", forbidden.ResourceName())
}

func TestProxyForbiddenError_ResourceName(t *testing.T) {
	assert.Equal(t, "prometheus:web", (&ProxyForbiddenError{Service: "prometheus", Port: "web", Scheme: "http"}).ResourceName())
	assert.Equal(t, "https:prometheus:9091",
		(&ProxyForbiddenError{Service: "prometheus", Port: "9091", Scheme: "https"}).ResourceName())
}

func TestParseSelector(t *testing.T) {
	tests := []struct {
		in      string
		want    Selector
		wantErr string
	}{
		{in: "", want: nil},
		{in: `cluster="prod"`, want: Selector{`cluster="prod"`}},
		{in: ` { cluster = "prod" , replica=~"a|b" } `, want: Selector{`cluster="prod"`, `replica=~"a|b"`}},
		{in: "region!=`eu-west-1`, env!~\"dev.*\"", want: Selector{`region!="eu-west-1"`, `env!~"dev.*"`}},
		{in: `cluster="a\"b"`, want: Selector{`cluster="a\"b"`}},
		{in: `cluster="x"}) or vector(1) #`, wantErr: "expected a comma"},
		{in: `cluster="prod"} or up{`, wantErr: "expected a comma"},
		{in: `{cluster="prod"`, wantErr: "doesn't close"},
		{in: `cluster=prod`, wantErr: "double-quoted"},
		{in: `cluster='prod'`, wantErr: "double-quoted"},
		{in: `cluster="prod`, wantErr: "no closing quote"},
		{in: `1cluster="prod"`, wantErr: "label name"},
		{in: `cluster:"prod"`, wantErr: "expected =, !=, =~ or !~"},
		{in: `cluster=~"(prod"`, wantErr: "regular expression"},
		{in: `cluster="a" cluster2="b"`, wantErr: "expected a comma"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSelector(tt.in)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A store holding many clusters is narrowed to one by the selector, in
// every history query.
func TestPrometheus_SelectorNarrowsEveryQuery(t *testing.T) {
	f := &fakePrometheus{byNamespace: map[string][]series{
		"dev": {{pod: "api-7d9f8c6b5-abcde", container: "app", cores: func(time.Time) float64 { return 0.25 }}},
	}}
	p := newFakePrometheus(t, f)
	sel, err := ParseSelector(`cluster="prod", tenant=~"a|b"`)
	require.NoError(t, err)
	p.Selector = sel
	end := scanTime.Truncate(minStep)

	_, err = p.namespaceCPU(context.Background(), "dev", end.Add(-time.Hour), end, minStep)

	require.NoError(t, err)
	require.Len(t, f.queries, 1)
	assert.Contains(t, f.queries[0], `{namespace="dev", container!="", container!="POD", cluster="prod", tenant=~"a|b"}`)
}

// Series from two clusters of one store are noticed and summed per
// container, and the scan says so; one cluster's alone aren't.
func TestPrometheus_MixedSources(t *testing.T) {
	end := scanTime.Truncate(minStep)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := float64(end.Unix())
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix", "result": []any{
				map[string]any{"metric": map[string]string{"pod": "api-x", "container": "app", "cluster": "prod"},
					"values": [][2]any{{at, "0.25"}}},
				map[string]any{"metric": map[string]string{"pod": "api-x", "container": "app", "cluster": "staging"},
					"values": [][2]any{{at, "0.5"}, {at - 300, "NaN"}}},
			}}})
	}))
	t.Cleanup(server.Close)
	p, err := NewPrometheusURL(server.URL, server.Client(), nil)
	require.NoError(t, err)
	assert.Empty(t, p.mixedSourcesNote(), "nothing read yet")

	history, err := p.namespaceCPU(context.Background(), "dev", end.Add(-time.Hour), end, minStep)

	require.NoError(t, err)
	require.Len(t, history, 1, "one container, from two clusters")
	assert.InDelta(t, 0.75, history[0].samples[end.Unix()], 0.0001)
	assert.NotContains(t, history[0].samples, end.Unix()-300, "NaN isn't a sample")
	note := p.mixedSourcesNote()
	assert.Contains(t, note, "more than one Prometheus (cluster: prod, staging)")
	assert.Contains(t, note, `--prometheus-selector 'cluster="..."'`)
}

func TestPrometheusURL_SendsHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(server.Close)
	p, err := NewPrometheusURL(server.URL+"?token=hidden", server.Client(),
		http.Header{"X-Scope-Orgid": {"team-a"}, "Authorization": {"Bearer t0ken"}})
	require.NoError(t, err)

	require.NoError(t, p.Check(context.Background()))

	assert.Equal(t, "team-a", got.Get("X-Scope-OrgID"))
	assert.Equal(t, "Bearer t0ken", got.Get("Authorization"))
	assert.NotContains(t, p.Source, "hidden", "a query string can hold a token")
}

// An error page says why, cut short; a response too large to hold is
// refused rather than read.
func TestPrometheusURL_ErrorBodiesAndLimits(t *testing.T) {
	big := strings.Repeat("x", maxErrorShown*3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "vector(1)":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "no org id\n"+big)
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[`))
			chunk := []byte(`{"metric":{"pod":"p","container":"c"},"values":[]},`)
			for written := 0; written <= maxResponseBytes; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	p, err := NewPrometheusURL(server.URL, server.Client(), nil)
	require.NoError(t, err)

	err = p.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401: no org id xxx")
	assert.Less(t, len(err.Error()), maxErrorShown+100)

	_, err = p.namespaceCPU(context.Background(), "dev", scanTime.Add(-time.Hour), scanTime, minStep)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "over 128 MiB")
}

// selfSigned is a TLS Prometheus with its own CA, and that CA as a PEM file.
func selfSigned(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	cert := server.Certificate()
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	return server, caFile
}

func TestNewHTTPClient(t *testing.T) {
	server, caFile := selfSigned(t)
	check := func(o HTTPOptions) error {
		c, err := NewHTTPClient(o)
		require.NoError(t, err)
		assert.Equal(t, prometheusTimeout, c.Timeout, "a Prometheus that stops answering is given up on")
		p, err := NewPrometheusURL(server.URL, c, nil)
		require.NoError(t, err)
		return p.Check(context.Background())
	}

	assert.Error(t, check(HTTPOptions{}), "a self-signed certificate isn't trusted")
	assert.NoError(t, check(HTTPOptions{CAFile: caFile}))
	assert.NoError(t, check(HTTPOptions{InsecureSkipVerify: true}))

	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	_, err := NewHTTPClient(HTTPOptions{CAFile: notPEM})
	assert.ErrorContains(t, err, "no PEM certificates")
	_, err = NewHTTPClient(HTTPOptions{CAFile: filepath.Join(t.TempDir(), "missing.pem")})
	assert.Error(t, err)
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
	p := NewPrometheusProxy(restClient, "monitoring", "prometheus-operated", "http", "web")

	err := p.Check(context.Background())

	var forbidden *ProxyForbiddenError
	require.ErrorAs(t, err, &forbidden)
	assert.Equal(t, ProxyForbiddenError{Namespace: "monitoring", Service: "prometheus-operated", Port: "web", Scheme: "http"},
		*forbidden)
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
	p := NewPrometheusProxy(restClient, "monitoring", "prometheus-operated", "http", "web")

	require.NoError(t, p.Check(context.Background()))

	assert.Equal(t, "/api/v1/namespaces/monitoring/services/prometheus-operated:web/proxy/api/v1/query", gotPath)
	assert.Equal(t, "vector(1)", gotQuery)
}
