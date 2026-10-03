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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// prometheusTimeout bounds each query. A week of one namespace's CPU at
// five-minute steps is a large but routine range query; one that takes
// longer than this is better reported than waited on.
const prometheusTimeout = 30 * time.Second

// ErrNoPrometheus means no Prometheus was found in the cluster.
var ErrNoPrometheus = errors.New("no Prometheus found")

// Prometheus reads CPU history from a Prometheus HTTP API.
type Prometheus struct {
	// Source says which Prometheus it is, for the report.
	Source string
	get    func(ctx context.Context, path string, params url.Values) ([]byte, error)
	// proxied is the Service it's reached through the API server at, if
	// it is.
	proxied *ProxyForbiddenError
}

// ProxyForbiddenError means the user may not reach a Prometheus Service
// through the API server's service proxy. It names the Service and port,
// which are what a Role granting it has to name.
type ProxyForbiddenError struct {
	Namespace, Service, Port string
}

func (e *ProxyForbiddenError) Error() string {
	return fmt.Sprintf("can't query %s/%s through the API server, which needs get on services/proxy",
		e.Namespace, e.Service)
}

// NewPrometheusURL reads from a Prometheus API at baseURL, such as Thanos,
// Mimir, or a managed Prometheus. A path prefix in the URL is kept.
func NewPrometheusURL(baseURL string, httpClient *http.Client) (*Prometheus, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("prometheus URL %q isn't an absolute URL", baseURL)
	}
	get := func(ctx context.Context, path string, params url.Values) ([]byte, error) {
		u := base.JoinPath(path)
		u.RawQuery = params.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }() // read fully below; nothing to do if closing fails
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return body, fmt.Errorf("status %d", resp.StatusCode)
		}
		return body, nil
	}
	return &Prometheus{Source: base.Redacted(), get: get}, nil
}

// NewPrometheusProxy reads from a Prometheus Service in the cluster through
// the API server's service proxy, so it needs no port-forward or URL, only
// get on services/proxy in its namespace.
func NewPrometheusProxy(restClient rest.Interface, namespace, service, port string) *Prometheus {
	get := func(ctx context.Context, path string, params url.Values) ([]byte, error) {
		req := restClient.Get().AbsPath("/api/v1/namespaces", namespace, "services", service+":"+port, "proxy", path)
		for k, vs := range params {
			for _, v := range vs {
				req = req.Param(k, v)
			}
		}
		return req.DoRaw(ctx)
	}
	return &Prometheus{Source: namespace + "/" + service, get: get,
		proxied: &ProxyForbiddenError{Namespace: namespace, Service: service, Port: port}}
}

// prometheusSelectors find the Services Prometheus installs expose, in the
// order they're tried: the Prometheus Operator's (and so
// kube-prometheus-stack's) governing Service, then the community chart's.
var prometheusSelectors = []client.MatchingLabels{
	{"operated-prometheus": "true"},
	{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"},
	{"app": "prometheus", "component": "server"},
}

// commonMonitoringNamespaces are looked in when the user can't list
// Services across the cluster.
var commonMonitoringNamespaces = []string{"monitoring", "prometheus", "observability"}

// FindPrometheus looks for a Prometheus Service and returns a reader for it
// through the service proxy. It looks across the cluster, or, when the user
// can't list Services everywhere, in the scanned namespaces and the usual
// monitoring ones.
func FindPrometheus(ctx context.Context, c client.Client, restClient rest.Interface, namespaces []string) (*Prometheus, error) {
	for _, selector := range prometheusSelectors {
		svc, err := findService(ctx, c, selector, namespaces)
		if err != nil {
			return nil, err
		}
		if svc != nil {
			return NewPrometheusProxy(restClient, svc.Namespace, svc.Name, servicePort(svc)), nil
		}
	}
	return nil, ErrNoPrometheus
}

func findService(ctx context.Context, c client.Client, selector client.MatchingLabels, namespaces []string) (
	*corev1.Service, error) {
	var list corev1.ServiceList
	err := c.List(ctx, &list, selector)
	if apierrors.IsForbidden(err) {
		list.Items = nil
		for _, ns := range uniqueSorted(append(slices.Clone(namespaces), commonMonitoringNamespaces...)) {
			var inNamespace corev1.ServiceList
			if err := c.List(ctx, &inNamespace, selector, client.InNamespace(ns)); err == nil {
				list.Items = append(list.Items, inNamespace.Items...)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	slices.SortFunc(list.Items, func(a, b corev1.Service) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return &list.Items[0], nil
}

// servicePort picks the port Prometheus serves its API on: the one named
// for it, or the only one.
func servicePort(svc *corev1.Service) string {
	for _, p := range svc.Spec.Ports {
		if p.Name == "web" || p.Name == "http" || p.Name == "http-web" {
			return p.Name
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return strconv.Itoa(int(svc.Spec.Ports[0].Port))
	}
	return "9090"
}

func uniqueSorted(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}

// Check runs a trivial query, so a Prometheus that can't be reached is
// reported once rather than for every namespace.
func (p *Prometheus) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, prometheusTimeout)
	defer cancel()
	body, err := p.get(ctx, "api/v1/query", url.Values{"query": {"vector(1)"}})
	if apierrors.IsForbidden(err) && p.proxied != nil {
		forbidden := *p.proxied
		return &forbidden
	}
	if err != nil {
		return fmt.Errorf("can't query %s: %w", p.Source, err)
	}
	var resp rangeResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Status != "success" {
		return fmt.Errorf("%s didn't answer like a Prometheus API", p.Source)
	}
	return nil
}

// containerCPU is the CPU one container of one pod used at each step, in
// cores. A step with no sample means the pod wasn't running then.
type containerCPU struct {
	pod, container string
	samples        map[int64]float64
}

type rangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// namespaceCPU returns the CPU each container in the namespace used over
// the window, from cAdvisor's counters, which every common Prometheus setup
// scrapes. Steps are keyed by Unix time.
func (p *Prometheus) namespaceCPU(ctx context.Context, namespace string, start, end time.Time, step time.Duration) (
	[]containerCPU, error) {
	query := fmt.Sprintf(
		`sum by (pod, container) (rate(container_cpu_usage_seconds_total{namespace=%q, container!="", container!="POD"}[%s]))`,
		namespace, promDuration(step))
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	}
	ctx, cancel := context.WithTimeout(ctx, prometheusTimeout)
	defer cancel()
	body, err := p.get(ctx, "api/v1/query_range", params)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", p.Source, err)
	}
	var resp rangeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("reading %s's response: %w", p.Source, err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("%s: query failed: %s", p.Source, resp.Error)
	}
	out := make([]containerCPU, 0, len(resp.Data.Result))
	for _, series := range resp.Data.Result {
		c := containerCPU{pod: series.Metric["pod"], container: series.Metric["container"],
			samples: make(map[int64]float64, len(series.Values))}
		for _, v := range series.Values {
			at, ok := v[0].(float64)
			raw, isString := v[1].(string)
			if !ok || !isString {
				continue
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			c.samples[int64(at)] = value
		}
		out = append(out, c)
	}
	return out, nil
}

// promDuration writes d as a PromQL duration.
func promDuration(d time.Duration) string {
	return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
}
