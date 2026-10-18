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
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// prometheusTimeout bounds a query made without a deadline of its own, such
// as the check that a Prometheus answers. A history query gets what's left
// of the time the scan gives history instead: a week of a large namespace
// can take a slow Thanos longer than this, and a fixed limit would never
// let it finish, however long --timeout is.
const prometheusTimeout = 30 * time.Second

// maxResponseBytes bounds what one query may return. A week of a namespace
// with a thousand containers at five-minute steps is about 60 MiB of JSON;
// anything much past that is a query that has gone wrong, not one to hold
// in memory.
const maxResponseBytes = 128 << 20

// maxErrorShown keeps an error page from filling the report.
const maxErrorShown = 300

// ErrNoPrometheus means no Prometheus was found in the cluster.
var ErrNoPrometheus = errors.New("no Prometheus found")

// Prometheus reads CPU history from a Prometheus HTTP API.
type Prometheus struct {
	// Source says which Prometheus it is, for the report.
	Source string
	// Selector narrows every query, as to one cluster of a store that holds
	// many.
	Selector Selector
	get      func(ctx context.Context, path string, params url.Values) ([]byte, error)
	// proxied is the Service it's reached through the API server at, if
	// it is.
	proxied *ProxyForbiddenError

	mu sync.Mutex
	// sources are the values each label that tells one Prometheus from
	// another had in what queries returned.
	sources map[string]map[string]bool
}

// sourceLabels tell one Prometheus's series from another's in a store that
// holds many, such as Thanos or Mimir: the external labels Prometheus
// setups conventionally add.
var sourceLabels = []string{"cluster", "prometheus", "prometheus_replica", "replica"}

// The schemes the service proxy reaches a Service with.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// ProxyForbiddenError means the user may not reach a Prometheus Service
// through the API server's service proxy. It names the Service and port,
// which are what a Role granting it has to name.
type ProxyForbiddenError struct {
	Namespace, Service, Port string
	// Scheme is "https" when the proxy reaches the Service over TLS.
	Scheme string
}

func (e *ProxyForbiddenError) Error() string {
	return fmt.Sprintf("can't query %s/%s through the API server, which needs get on services/proxy",
		e.Namespace, e.Service)
}

// ResourceName is the Service as the proxy path, and so RBAC, names it.
func (e *ProxyForbiddenError) ResourceName() string {
	if e.Scheme == schemeHTTPS {
		return "https:" + e.Service + ":" + e.Port
	}
	return e.Service + ":" + e.Port
}

// HTTPOptions say how to reach a Prometheus outside the cluster.
type HTTPOptions struct {
	// CAFile is a PEM bundle to trust besides the system's roots.
	CAFile string
	// InsecureSkipVerify trusts any certificate.
	InsecureSkipVerify bool
}

// NewHTTPClient returns a client for a Prometheus outside the cluster. It
// sets no timeout of its own: every query is bounded by its context, which
// for history is the time the scan gives it.
func NewHTTPClient(o HTTPOptions) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading the CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the CA file %s has no PEM certificates", o.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	tlsConfig.InsecureSkipVerify = o.InsecureSkipVerify //nolint:gosec // only when the user asks for it by flag
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}

// NewPrometheusURL reads from a Prometheus API at baseURL, such as Thanos,
// Mimir, or a managed Prometheus, sending header with every request. A path
// prefix and query parameters in the URL are kept, and credentials in it
// are sent as basic auth, unless header has an Authorization.
//
// Credentials and query parameters, which can hold a token, are left out
// of Source and of errors, since both end up in reports that are shared.
func NewPrometheusURL(baseURL string, httpClient *http.Client, header http.Header) (*Prometheus, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("the Prometheus URL isn't an absolute URL, such as https://prometheus.example.com")
	}
	user := base.User
	base.User = nil
	baseQuery := base.Query()
	get := func(ctx context.Context, path string, params url.Values) ([]byte, error) {
		u := base.JoinPath(path)
		query := maps.Clone(baseQuery)
		maps.Copy(query, params)
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		maps.Copy(req.Header, header)
		if user != nil && req.Header.Get("Authorization") == "" {
			password, _ := user.Password()
			req.SetBasicAuth(user.Username(), password)
		}
		resp, err := httpClient.Do(req)
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }() // read below; nothing to do if closing fails
		body, err := readLimited(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, errorExcerpt(body))
		}
		return body, nil
	}
	source := *base
	source.RawQuery, source.Fragment = "", ""
	return &Prometheus{Source: source.String(), get: get}, nil
}

// NewPrometheusProxy reads from a Prometheus Service in the cluster through
// the API server's service proxy, so it needs no port-forward or URL, only
// get on services/proxy in its namespace. scheme is "https" for a Service
// that serves TLS.
func NewPrometheusProxy(restClient rest.Interface, namespace, service, scheme, port string) *Prometheus {
	proxied := &ProxyForbiddenError{Namespace: namespace, Service: service, Port: port, Scheme: scheme}
	get := func(ctx context.Context, path string, params url.Values) ([]byte, error) {
		req := restClient.Get().AbsPath("/api/v1/namespaces", namespace, "services", proxied.ResourceName(), "proxy", path)
		for k, vs := range params {
			for _, v := range vs {
				req = req.Param(k, v)
			}
		}
		stream, err := req.Stream(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stream.Close() }() // read below; nothing to do if closing fails
		return readLimited(stream)
	}
	return &Prometheus{Source: namespace + "/" + service, get: get, proxied: proxied}
}

func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("the response is over %d MiB; try a shorter --window", maxResponseBytes>>20)
	}
	return body, nil
}

// errorExcerpt is what an error response says: a Prometheus API's own error
// message, or the start of whatever else came back, on one line. What can't
// be printed, such as a terminal escape sequence's ESC, is dropped, as the
// excerpt is shown in the terminal.
func errorExcerpt(body []byte) string {
	var resp struct {
		Error string `json:"error"`
	}
	text := string(body)
	if json.Unmarshal(body, &resp) == nil && resp.Error != "" {
		text = resp.Error
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxErrorShown {
		text = string(runes[:maxErrorShown]) + "..."
	}
	if text == "" {
		return "no body"
	}
	return text
}

// prometheusSelectors find the Services Prometheus installs expose, best
// first: the Prometheus Operator's (and so kube-prometheus-stack's)
// governing Service, then the community chart's.
var prometheusSelectors = []client.MatchingLabels{
	{"operated-prometheus": "true"},
	{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"},
	{"app": "prometheus", "component": "server"},
}

// monitoringNamespaces are where Prometheus is usually installed, best
// first. A Prometheus found in one of them is preferred, and they're looked
// in when the user can't list Services across the cluster.
var monitoringNamespaces = []string{"monitoring", "prometheus", "observability", "kube-prometheus-stack",
	"cattle-monitoring-system", "openshift-monitoring"}

// prometheusNames are the Service names common installs give Prometheus,
// preferred over others in the same namespace.
var prometheusNames = []string{"prometheus-operated", "kube-prometheus-stack-prometheus", "prometheus-server",
	"prometheus", "prometheus-k8s"}

// maxCandidates bounds how many Prometheus Services are tried in turn.
const maxCandidates = 5

// FindPrometheus looks for a Prometheus Service and returns a reader for the
// best one that answers like a Prometheus API, through the service proxy.
// It looks across the cluster, or, when the user can't list Services
// everywhere, in the usual monitoring namespaces and the scanned ones. When
// none answers, the error is why the best one didn't, or, if any refused the
// user, that, which says what access to ask for.
func FindPrometheus(ctx context.Context, c client.Client, restClient rest.Interface, namespaces []string) (*Prometheus, error) {
	candidates, err := prometheusServices(ctx, c, namespaces)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoPrometheus
	}
	var first error
	var forbidden *ProxyForbiddenError
	for _, svc := range candidates[:min(len(candidates), maxCandidates)] {
		scheme, port := servicePort(&svc)
		p := NewPrometheusProxy(restClient, svc.Namespace, svc.Name, scheme, port)
		err := p.Check(ctx)
		if err == nil {
			return p, nil
		}
		first = cmp.Or(first, err)
		if forbidden == nil {
			errors.As(err, &forbidden)
		}
	}
	if forbidden != nil {
		return nil, forbidden
	}
	return nil, first
}

// prometheusServices are the Services that look like Prometheus, best first.
func prometheusServices(ctx context.Context, c client.Client, namespaces []string) ([]corev1.Service, error) {
	seen := map[string]bool{}
	var found []corev1.Service
	add := func(svc corev1.Service) {
		if key := svc.Namespace + "/" + svc.Name; !seen[key] && selectorRank(svc) < len(prometheusSelectors) {
			seen[key] = true
			found = append(found, svc)
		}
	}
	forbidden := false
	for _, selector := range prometheusSelectors {
		err := listAll(ctx, c, func(list *corev1.ServiceList) {
			for _, svc := range list.Items {
				add(svc)
			}
		}, selector)
		if apierrors.IsForbidden(err) {
			forbidden = true
			break
		}
		if err != nil {
			return nil, fmt.Errorf("can't look for Prometheus: listing services: %w", err)
		}
	}
	if forbidden {
		for _, ns := range uniqueSorted(append(slices.Clone(namespaces), monitoringNamespaces...)) {
			// A namespace the user can't read either is one Prometheus
			// can't be found in, which isn't worth stopping for.
			_ = listAll(ctx, c, func(list *corev1.ServiceList) {
				for _, svc := range list.Items {
					add(svc)
				}
			}, client.InNamespace(ns))
		}
	}
	slices.SortFunc(found, func(a, b corev1.Service) int {
		return cmp.Or(
			cmp.Compare(rankIn(monitoringNamespaces, a.Namespace), rankIn(monitoringNamespaces, b.Namespace)),
			cmp.Compare(rankIn(prometheusNames, a.Name), rankIn(prometheusNames, b.Name)),
			cmp.Compare(selectorRank(a), selectorRank(b)),
			strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name),
		)
	})
	return found, nil
}

func rankIn(list []string, s string) int {
	if i := slices.Index(list, s); i >= 0 {
		return i
	}
	return len(list)
}

// selectorRank is the first of prometheusSelectors that matches svc, or
// their number when none does.
func selectorRank(svc corev1.Service) int {
	for i, selector := range prometheusSelectors {
		matches := true
		for k, v := range selector {
			matches = matches && svc.Labels[k] == v
		}
		if matches {
			return i
		}
	}
	return len(prometheusSelectors)
}

// servicePort picks the port Prometheus serves its API on, and whether it
// serves it over TLS: the one named for it, or the only one.
func servicePort(svc *corev1.Service) (scheme, port string) {
	for _, p := range svc.Spec.Ports {
		if p.Name == "web" || p.Name == schemeHTTP || p.Name == "http-web" {
			return portScheme(p), p.Name
		}
	}
	for _, p := range svc.Spec.Ports {
		if portScheme(p) == schemeHTTPS {
			return schemeHTTPS, portName(p)
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return portScheme(svc.Spec.Ports[0]), portName(svc.Spec.Ports[0])
	}
	return schemeHTTP, "9090"
}

func portScheme(p corev1.ServicePort) string {
	if strings.Contains(p.Name, schemeHTTPS) || p.Port == 443 || p.Port == 8443 ||
		(p.AppProtocol != nil && strings.EqualFold(*p.AppProtocol, schemeHTTPS)) {
		return schemeHTTPS
	}
	return schemeHTTP
}

func portName(p corev1.ServicePort) string {
	if p.Name != "" {
		return p.Name
	}
	return strconv.Itoa(int(p.Port))
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
	if p.proxied != nil && proxyDenied(err) {
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

// proxyDenied says the API server refused the user the service proxy, as
// opposed to passing on a 403 from behind it, such as a kube-rbac-proxy in
// front of Prometheus: the API server's own refusal is a Status it writes,
// where a backend's body reaches the client as an unexpected response.
func proxyDenied(err error) bool {
	if !apierrors.IsForbidden(err) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	if d := status.Status().Details; d != nil {
		for _, cause := range d.Causes {
			if cause.Type == metav1.CauseTypeUnexpectedServerResponse {
				return false
			}
		}
	}
	return true
}

// isTimeout says a query ran out of time: its deadline passed, the HTTP
// client gave up, or the API server's proxy timed out waiting on it.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
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
			Values []sample          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// sample is one of a range query's [time, "value"] pairs. It's decoded into
// fields rather than [2]any, which would box both halves of every sample of
// a response, millions for a week of a large namespace, all at once.
type sample struct {
	at    int64
	value float64
	// ok is false for a value that isn't a finite number, which is skipped.
	ok bool
}

func (s *sample) UnmarshalJSON(b []byte) error {
	*s = sample{}
	inner, isArray := bytes.CutPrefix(bytes.TrimSpace(b), []byte("["))
	inner, closed := bytes.CutSuffix(inner, []byte("]"))
	at, value, paired := bytes.Cut(inner, []byte(","))
	if !isArray || !closed || !paired {
		return nil
	}
	t, err := strconv.ParseFloat(string(bytes.TrimSpace(at)), 64)
	if err != nil {
		return nil
	}
	raw, err := strconv.Unquote(string(bytes.TrimSpace(value)))
	if err != nil {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	*s = sample{at: int64(t), value: v, ok: true}
	return nil
}

// scrapeLabels tell apart scrapes of the same container's CPU by the same
// Prometheus, as when cAdvisor's counter is scraped from both the kubelet's
// /metrics/cadvisor and /metrics/resource, or by two jobs. They're copies,
// so only one of them counts.
var scrapeLabels = []string{"job", "metrics_path", "service", "endpoint", "instance"}

// namespaceCPU returns the CPU each container in the namespace used over
// the window, from cAdvisor's counters, which every common Prometheus setup
// scrapes. Steps are keyed by Unix time. Copies of a container's CPU that
// one Prometheus scraped twice count once, the highest at each step. The
// series of different Prometheus are kept apart by the labels that tell
// them apart, so a store mixing several is noticed, and then summed per
// container.
func (p *Prometheus) namespaceCPU(ctx context.Context, namespace string, start, end time.Time, step time.Duration) (
	[]containerCPU, error) {
	matchers := append([]string{"namespace=" + strconv.Quote(namespace), `container!=""`, `container!="POD"`},
		p.Selector...)
	query := fmt.Sprintf(`sum by (pod, container, %s) (rate(container_cpu_usage_seconds_total{%s}[%s]))`,
		strings.Join(slices.Concat(sourceLabels, scrapeLabels), ", "), strings.Join(matchers, ", "),
		promDuration(step))
	params := url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step.Seconds()), 10)},
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, prometheusTimeout)
		defer cancel()
	}
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
	type fromSource struct{ pod, container, source string }
	scraped := map[fromSource]map[int64]float64{}
	var order []fromSource
	for _, series := range resp.Data.Result {
		p.noteSource(series.Metric)
		k := fromSource{series.Metric["pod"], series.Metric["container"], sourceOf(series.Metric)}
		samples := scraped[k]
		if samples == nil {
			samples = make(map[int64]float64, len(series.Values))
			scraped[k] = samples
			order = append(order, k)
		}
		for _, v := range series.Values {
			if have, seen := samples[v.at]; v.ok && (!seen || v.value > have) {
				samples[v.at] = v.value
			}
		}
	}
	type key struct{ pod, container string }
	index := map[key]int{}
	out := make([]containerCPU, 0, len(order))
	for _, k := range order {
		i, ok := index[key{k.pod, k.container}]
		if !ok {
			index[key{k.pod, k.container}] = len(out)
			out = append(out, containerCPU{pod: k.pod, container: k.container, samples: scraped[k]})
			continue
		}
		for at, cores := range scraped[k] {
			out[i].samples[at] += cores
		}
	}
	return out, nil
}

// sourceOf is which Prometheus a series came from, by its sourceLabels.
func sourceOf(metric map[string]string) string {
	values := make([]string, len(sourceLabels))
	for i, label := range sourceLabels {
		values[i] = metric[label]
	}
	return strings.Join(values, "\x00")
}

func (p *Prometheus) noteSource(metric map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sources == nil {
		p.sources = map[string]map[string]bool{}
	}
	for _, label := range sourceLabels {
		if v := metric[label]; v != "" {
			if p.sources[label] == nil {
				p.sources[label] = map[string]bool{}
			}
			p.sources[label][v] = true
		}
	}
}

// mixedSourcesNote warns when the history came from more than one
// Prometheus, as a store holding many clusters' returns unless a selector
// narrows it: the replay then sums every cluster's pods of the same name,
// or each replica's copy of one.
func (p *Prometheus) mixedSourcesNote() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var mixed []string
	for _, label := range sourceLabels {
		if values := p.sources[label]; len(values) > 1 {
			mixed = append(mixed, fmt.Sprintf("%s: %s", label, listSome(slices.Sorted(maps.Keys(values)))))
		}
	}
	if len(mixed) == 0 {
		return ""
	}
	example := fmt.Sprintf(`'%s="..."'`, sourceLabels[0])
	if values := p.sources[sourceLabels[0]]; len(values) == 0 {
		example = `'<label>="..."'`
	}
	return fmt.Sprintf("%s returned CPU from more than one Prometheus (%s), so the history replay adds them "+
		"together; pass --prometheus-selector %s to read only this cluster's", p.Source,
		strings.Join(mixed, "; "), example)
}

// promDuration writes d as a PromQL duration.
func promDuration(d time.Duration) string {
	return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
}
