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
	"time"
)

const prometheusTimeout = 5 * time.Second

// maxResponseBytes bounds a query's response. An activity query answers
// with a handful of series; a response this large is a query returning a
// series per pod across the cluster, and isn't read into memory.
const maxResponseBytes = 4 << 20

// ErrEndpointNotConfigured is returned when a Prometheus signal is evaluated
// but the operator was started without a Prometheus URL.
var ErrEndpointNotConfigured = errors.New("prometheus endpoint not configured, set --prometheus-url on the operator")

type prometheusResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Prometheus evaluates a PromQL instant query. The workload is active when
// any sample in the result is above zero: with a series per pod, one busy
// pod is enough.
//
// NaN and infinite samples are ignored, as no evidence either way: NaN is
// what a ratio of two idle counters gives (0/0). A result with no usable
// sample, whether empty or all NaN, is no data, and no data doesn't confirm
// activity, so the workload's other activity sources decide.
type Prometheus struct {
	Endpoint string
	Query    string
	client   *http.Client
}

func NewPrometheus(endpoint, query string) *Prometheus {
	return &Prometheus{
		Endpoint: endpoint,
		Query:    query,
		client: &http.Client{
			Timeout: prometheusTimeout,
		},
	}
}

func (p *Prometheus) Check(ctx context.Context) (Result, error) {
	if p.Endpoint == "" {
		return Result{}, ErrEndpointNotConfigured
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil {
		return Result{}, fmt.Errorf("parsing prometheus endpoint: %w", err)
	}
	// Join rather than replace the path so endpoints served under a prefix
	// (Thanos, Mimir, or Prometheus behind a reverse proxy) keep working.
	u = u.JoinPath("api", "v1", "query")
	u.RawQuery = url.Values{"query": {p.Query}}.Encode()

	ctx, cancel := context.WithTimeout(ctx, prometheusTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("building prometheus request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("querying prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading prometheus response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return Result{}, fmt.Errorf("prometheus response for %q is larger than %d bytes", p.Query, maxResponseBytes)
	}

	var body prometheusResponse
	decodeErr := json.Unmarshal(data, &body)
	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && body.Error != "" {
			return Result{}, fmt.Errorf("prometheus returned status %d: %s: %s", resp.StatusCode, body.ErrorType, body.Error)
		}
		return Result{}, fmt.Errorf("prometheus returned status %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return Result{}, fmt.Errorf("decoding prometheus response: %w", decodeErr)
	}
	if body.Status != "success" {
		return Result{}, fmt.Errorf("prometheus query failed: status %q: %s", body.Status, body.Error)
	}

	samples, err := samplesOf(body.Data.ResultType, body.Data.Result)
	if err != nil {
		return Result{}, fmt.Errorf("reading prometheus result for %q: %w", p.Query, err)
	}
	return p.evaluate(samples), nil
}

func (p *Prometheus) evaluate(samples []float64) Result {
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
		return Result{Reason: fmt.Sprintf("promql returned empty result for %q", p.Query)}
	case usable == 0:
		return Result{Reason: fmt.Sprintf("promql returned no usable value (NaN or Inf) for %q", p.Query)}
	case highest <= 0:
		return Result{Reason: fmt.Sprintf("promql value is %g for %q", highest, p.Query)}
	default:
		return Result{Confirm: true, Reason: fmt.Sprintf("promql value is %g for %q", highest, p.Query)}
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
