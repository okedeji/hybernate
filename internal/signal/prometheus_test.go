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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrometheus_Confirms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/query", r.URL.Path)
		assert.Equal(t, `rate(http_requests_total[5m])`, r.URL.Query().Get("query"))
		w.Write([]byte(`{
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
		w.Write([]byte(`{
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
		w.Write([]byte(`{
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
		w.Write([]byte(`{"status": "error", "errorType": "bad_data", "error": "invalid query"}`))
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

func TestPrometheus_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	p := NewPrometheus(srv.URL, `up`)
	_, err := p.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding prometheus response")
}
