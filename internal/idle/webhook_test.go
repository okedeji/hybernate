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

package idle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhook_Idle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "staging", r.URL.Query().Get("namespace"))
		assert.Equal(t, "api", r.URL.Query().Get("name"))
		w.Write([]byte(`{"idle": true}`))
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL)
	res, err := wh.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.False(t, res.Active)
	assert.Equal(t, "webhook reports idle", res.Reason)
}

func TestWebhook_Active(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"idle": false}`))
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL)
	res, err := wh.Check(context.Background(), "staging", "api")

	require.NoError(t, err)
	assert.True(t, res.Active)
	assert.Equal(t, "webhook reports active", res.Reason)
}

func TestWebhook_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL)
	_, err := wh.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned status 500")
}

func TestWebhook_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL)
	_, err := wh.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding webhook response")
}

func TestWebhook_Unreachable(t *testing.T) {
	wh := NewWebhook("http://localhost:1")
	_, err := wh.Check(context.Background(), "staging", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "calling webhook")
}
