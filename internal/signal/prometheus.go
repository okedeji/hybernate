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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const prometheusTimeout = 5 * time.Second

type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Prometheus implements Checker by evaluating a PromQL instant query.
// A non-empty, non-zero result confirms the action; empty or zero denies it.
// The PromQL query encodes the intent — the caller writes a query that
// returns non-empty when the proposed action should proceed.
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

func (p *Prometheus) Check(ctx context.Context, _, _ string) (Result, error) {
	u, err := url.Parse(p.Endpoint)
	if err != nil {
		return Result{}, fmt.Errorf("parsing prometheus endpoint: %w", err)
	}
	u.Path = "/api/v1/query"
	u.RawQuery = url.Values{"query": {p.Query}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("building prometheus request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("querying prometheus: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("prometheus returned status %d", resp.StatusCode)
	}

	var body prometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Result{}, fmt.Errorf("decoding prometheus response: %w", err)
	}

	if body.Status != "success" {
		return Result{}, fmt.Errorf("prometheus query failed: status %q", body.Status)
	}

	if len(body.Data.Result) == 0 {
		return Result{Confirm: false, Reason: fmt.Sprintf("promql returned empty result for %q", p.Query)}, nil
	}

	val, err := extractScalarValue(body.Data.Result[0].Value)
	if err != nil {
		return Result{}, fmt.Errorf("extracting prometheus value: %w", err)
	}

	if val == 0 {
		return Result{Confirm: false, Reason: fmt.Sprintf("promql value is 0 for %q", p.Query)}, nil
	}
	return Result{Confirm: true, Reason: fmt.Sprintf("promql value is %g for %q", val, p.Query)}, nil
}

// extractScalarValue pulls the float64 from a Prometheus [timestamp, "value"] pair.
func extractScalarValue(pair []json.RawMessage) (float64, error) {
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
