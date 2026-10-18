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

package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// queryTimeout bounds one query. An activity query is an instant query over
// a handful of series, which a healthy Prometheus answers in milliseconds;
// every awake workload asks each minute, from a few reconcile workers that
// wakes queue behind, so a slow answer costs more than a missing one.
const queryTimeout = 2 * time.Second

// timeoutsToTrip is how many queries in a row must time out before the
// endpoint is taken to be failing. One slow query is that query's: a tenant's
// expensive PromQL mustn't hold off every workload's Prometheus source, and
// the other workloads' answers in between reset the count.
const timeoutsToTrip = 3

// backoff is how long after Prometheus fails it isn't asked again. It's
// about one activity check, so every workload's next check finds the
// outage at once rather than each waiting out a timeout to learn of it.
const backoff = time.Minute

// maxResponseBytes bounds a query's response. An activity query answers
// with a handful of series; a response this large is a query returning a
// series per pod across the cluster, and isn't read into memory.
const maxResponseBytes = 4 << 20

// ErrEndpointNotConfigured is returned when a Prometheus signal is evaluated
// but the operator was started without a Prometheus URL.
var ErrEndpointNotConfigured = errors.New("prometheus endpoint not configured, set --prometheus-url on the operator")

// ErrBackingOff is returned, wrapping the failure, while Prometheus isn't
// asked again after failing.
var ErrBackingOff = errors.New("prometheus failed recently, not asked again yet")

type prometheusResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Prometheus evaluates PromQL instant queries against one endpoint. A
// workload is active when any sample in a query's result is above zero:
// with a series per pod, one busy pod is enough.
//
// NaN and infinite samples are ignored, as no evidence either way: NaN is
// what a ratio of two idle counters gives (0/0). A result with no usable
// sample, whether empty or all NaN, is no data, and no data doesn't confirm
// activity, so the workload's other activity sources decide.
//
// When the endpoint fails, by not answering, answering with a server error,
// or timeoutsToTrip queries in a row not answering within queryTimeout, it
// isn't asked again for
// backoff: every query fails at once with ErrBackingOff. Then one query is
// let through to see whether it's back, while the rest go on failing until
// it answers. A query Prometheus rejects, such as one that doesn't parse,
// is that query's failure, and a caller's own deadline running out is the
// caller's: neither is the endpoint's.
type Prometheus struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration
	now      func() time.Time

	mu       sync.Mutex
	failedAt time.Time
	failure  error
	timeouts int
}

// NewPrometheus returns a client for the Prometheus HTTP API at endpoint,
// to be shared by every query against it. An empty endpoint fails every
// query with ErrEndpointNotConfigured.
func NewPrometheus(endpoint string) *Prometheus {
	return &Prometheus{endpoint: endpoint, client: &http.Client{}, timeout: queryTimeout, now: time.Now}
}

// Check evaluates query.
func (p *Prometheus) Check(ctx context.Context, query string) (Result, error) {
	if p.endpoint == "" {
		return Result{}, ErrEndpointNotConfigured
	}
	if err := p.admit(); err != nil {
		return Result{}, err
	}
	res, err := p.query(ctx, query)
	var failed endpointFailure
	var slow queryTimedOut
	switch {
	case errors.As(err, &failed):
		p.backOff(failed.err)
		return Result{}, failed.err
	case errors.As(err, &slow):
		p.timedOut(slow.err)
		return Result{}, slow.err
	case ctx.Err() != nil:
		// The caller gave up before the endpoint answered either way.
	default:
		p.answered()
	}
	return res, err
}

// endpointFailure marks a query's error as the endpoint's: it didn't
// answer, or answered that it can't.
type endpointFailure struct{ err error }

func (e endpointFailure) Error() string { return e.err.Error() }
func (e endpointFailure) Unwrap() error { return e.err }

// queryTimedOut marks a query's error as its own timeout running out.
type queryTimedOut struct{ err error }

func (e queryTimedOut) Error() string { return e.err.Error() }
func (e queryTimedOut) Unwrap() error { return e.err }

// unlessGivenUp is err as the endpoint's failure, or as the query timing
// out, unless the caller's own context ended first.
func unlessGivenUp(caller, call context.Context, err error) error {
	switch {
	case caller.Err() != nil:
		return err
	case errors.Is(call.Err(), context.DeadlineExceeded):
		return queryTimedOut{err}
	default:
		return endpointFailure{err}
	}
}

// admit fails fast while the endpoint is backing off. Once backoff is up,
// the caller is let through and the backoff restarted, so the others wait
// on its answer rather than all asking at once.
func (p *Prometheus) admit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure == nil {
		return nil
	}
	now := p.now()
	if now.Before(p.failedAt.Add(backoff)) {
		return fmt.Errorf("%w: %w", ErrBackingOff, p.failure)
	}
	p.failedAt = now
	return nil
}

func (p *Prometheus) backOff(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failedAt, p.failure = p.now(), err
}

func (p *Prometheus) timedOut(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.timeouts++
	if p.timeouts >= timeoutsToTrip {
		p.failedAt, p.failure, p.timeouts = p.now(), err, 0
	}
}

func (p *Prometheus) answered() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failure, p.timeouts = nil, 0
}

func (p *Prometheus) query(ctx context.Context, query string) (Result, error) {
	u, err := url.Parse(p.endpoint)
	if err != nil {
		return Result{}, fmt.Errorf("parsing prometheus endpoint: %w", err)
	}
	// Join rather than replace the path so endpoints served under a prefix
	// (Thanos, Mimir, or Prometheus behind a reverse proxy) keep working.
	u = u.JoinPath("api", "v1", "query")
	u.RawQuery = url.Values{"query": {query}}.Encode()

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("building prometheus request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, unlessGivenUp(ctx, callCtx, fmt.Errorf("querying prometheus: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, unlessGivenUp(ctx, callCtx, fmt.Errorf("reading prometheus response: %w", err))
	}
	if len(data) > maxResponseBytes {
		return Result{}, fmt.Errorf("prometheus response for %q is larger than %d bytes", query, maxResponseBytes)
	}

	var body prometheusResponse
	decodeErr := json.Unmarshal(data, &body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("prometheus returned status %d", resp.StatusCode)
		if decodeErr == nil && body.Error != "" {
			err = fmt.Errorf("prometheus returned status %d: %s: %s", resp.StatusCode, body.ErrorType, body.Error)
		}
		if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
			return Result{}, endpointFailure{err}
		}
		return Result{}, err
	}
	if decodeErr != nil {
		return Result{}, fmt.Errorf("decoding prometheus response: %w", decodeErr)
	}
	if body.Status != "success" {
		return Result{}, fmt.Errorf("prometheus query failed: status %q: %s", body.Status, body.Error)
	}

	samples, err := samplesOf(body.Data.ResultType, body.Data.Result)
	if err != nil {
		return Result{}, fmt.Errorf("reading prometheus result for %q: %w", query, err)
	}
	return evaluate(query, samples), nil
}

func evaluate(query string, samples []float64) Result {
	highest, usable := math.Inf(-1), 0
	for _, v := range samples {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		usable++
		highest = math.Max(highest, v)
	}
	switch {
	case len(samples) == 0:
		return Result{NoSeries: true, Reason: fmt.Sprintf("promql returned empty result for %q", query)}
	case usable == 0:
		return Result{Reason: fmt.Sprintf("promql returned no usable value (NaN or Inf) for %q", query)}
	case highest <= 0:
		return Result{Reason: fmt.Sprintf("promql value is %g for %q", highest, query)}
	default:
		return Result{Confirm: true, Reason: fmt.Sprintf("promql value is %g for %q", highest, query)}
	}
}

// samplesOf reads the values of an instant query's result: one per series
// of a vector, or the single value of a scalar.
func samplesOf(resultType string, result json.RawMessage) ([]float64, error) {
	switch resultType {
	case "vector":
		var series []struct {
			Value []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(result, &series); err != nil {
			return nil, fmt.Errorf("decoding vector: %w", err)
		}
		samples := make([]float64, 0, len(series))
		for _, s := range series {
			v, err := sampleValue(s.Value)
			if err != nil {
				return nil, err
			}
			samples = append(samples, v)
		}
		return samples, nil
	case "scalar":
		var pair []json.RawMessage
		if err := json.Unmarshal(result, &pair); err != nil {
			return nil, fmt.Errorf("decoding scalar: %w", err)
		}
		v, err := sampleValue(pair)
		if err != nil {
			return nil, err
		}
		return []float64{v}, nil
	default:
		return nil, fmt.Errorf("result type %q is not an instant vector or scalar", resultType)
	}
}

// sampleValue pulls the float64 from a Prometheus [timestamp, "value"] pair.
// Prometheus writes NaN and infinities as "NaN", "+Inf" and "-Inf", which
// ParseFloat reads.
func sampleValue(pair []json.RawMessage) (float64, error) {
	if len(pair) < 2 {
		return 0, fmt.Errorf("expected [timestamp, value] pair, got %d elements", len(pair))
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, fmt.Errorf("unmarshalling value: %w", err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing value %q: %w", s, err)
	}
	return v, nil
}
