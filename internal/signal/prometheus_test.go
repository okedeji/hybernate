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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrometheus_Confirms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/query", r.URL.Path)
		assert.Equal(t, `rate(http_requests_total[5m])`, r.URL.Query().Get("query"))
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [{"metric": {}, "value": [1234567890, "42.5"]}]
			}
		}`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `rate(http_requests_total[5m])`)
	res, err := p.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.True(t, res.Confirm)
	assert.Contains(t, res.Reason, "42.5")
}

func TestPrometheus_DeniesZeroValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [{"metric": {}, "value": [1234567890, "0"]}]
			}
		}`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `rate(http_requests_total[5m])`)
	res, err := p.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Confirm)
	assert.Contains(t, res.Reason, "value is 0")
}

func TestPrometheus_DeniesEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": []
			}
		}`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `rate(http_requests_total[5m])`)
	res, err := p.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Confirm)
	assert.Contains(t, res.Reason, "empty result")
}

func TestPrometheus_QueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status": "error", "errorType": "bad_data", "error": "invalid query"}`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `bad{`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prometheus query failed")
}

func TestPrometheus_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned status 500")
}

func TestPrometheus_Unreachable(t *testing.T) {
	p := NewPrometheus("http://localhost:1", `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "querying prometheus")
}

func TestPrometheus_EndpointNotConfigured(t *testing.T) {
	p := NewPrometheus("", `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.ErrorIs(t, err, ErrEndpointNotConfigured)
}

func TestPrometheus_PreservesEndpointPathPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/prometheus/api/v1/query", r.URL.Path)
		_, _ = w.Write([]byte(`{"status": "success", "data": {"resultType": "vector", "result": []}}`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL+"/prometheus", `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
}

func TestPrometheus_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding prometheus response")
}

func serving(t *testing.T, body string) *Prometheus {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewPrometheus(srv.URL, `sum by (pod) (rate(http_requests_total[5m]))`)
}

func vectorOf(values ...string) string {
	series := make([]string, len(values))
	for i, v := range values {
		series[i] = `{"metric":{"pod":"p` + strconv.Itoa(i) + `"},"value":[1,"` + v + `"]}`
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
}

func TestPrometheus_Results(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantConfirm bool
		wantReason  string
	}{
		{name: "any busy series is activity", body: vectorOf("0", "12"), wantConfirm: true, wantReason: "12"},
		{name: "every series idle", body: vectorOf("0", "0"), wantReason: "value is 0"},
		{name: "NaN is no data, not activity", body: vectorOf("NaN"), wantReason: "no usable value"},
		{name: "infinities are ignored", body: vectorOf("+Inf", "-Inf"), wantReason: "no usable value"},
		{name: "NaN beside an idle series", body: vectorOf("NaN", "0"), wantReason: "value is 0"},
		{name: "NaN beside a busy series", body: vectorOf("NaN", "3"), wantConfirm: true},
		{name: "negative", body: vectorOf("-2"), wantReason: "value is -2"},
		{name: "busy scalar", body: `{"status":"success","data":{"resultType":"scalar","result":[1,"3"]}}`, wantConfirm: true},
		{name: "idle scalar", body: `{"status":"success","data":{"resultType":"scalar","result":[1,"0"]}}`},
		{name: "NaN scalar", body: `{"status":"success","data":{"resultType":"scalar","result":[1,"NaN"]}}`,
			wantReason: "no usable value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := serving(t, tt.body).Check(context.Background(), "staging", "api")
			require.NoError(t, err)
			assert.Equal(t, tt.wantConfirm, res.Confirm)
			assert.Contains(t, res.Reason, tt.wantReason)
		})
	}
}

func TestPrometheus_RejectsRangeVectors(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"5"]]}]}}`

	_, err := serving(t, body).Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"matrix"`)
}

func TestPrometheus_ReportsTheQueryErrorOnABadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at char 4"}`))
	}))
	defer srv.Close()

	_, err := NewPrometheus(srv.URL, `bad{`).Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse error at char 4")
}

func TestPrometheus_BoundsTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[`))
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResponseBytes))
		_, _ = w.Write([]byte(`]}}`))
	}))
	defer srv.Close()

	_, err := NewPrometheus(srv.URL, `up`).Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
}

func TestPrometheus_HonoursTheCallersDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewPrometheus(srv.URL, `up`).Check(ctx, "staging", "api")

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPrometheus_EncodesTheQuery(t *testing.T) {
	query := `sum(rate(http_requests_total{job="api",path=~"/a&b=c"}[5m])) > 0 # comment`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, query, r.URL.Query().Get("query"))
		assert.Len(t, r.URL.Query(), 1, "the query can't inject parameters")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	_, err := NewPrometheus(srv.URL, query).Check(context.Background(), "staging", "api")
	require.NoError(t, err)
}
