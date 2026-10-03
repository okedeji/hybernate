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

package doorman

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func request(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n\r\n")
}

func TestIsPageLoad(t *testing.T) {
	const htmlAccept = "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	tests := []struct {
		name string
		head []byte
		want bool
	}{
		{name: "a browser opening a page", head: request("GET /orders HTTP/1.1", "Host: shop", htmlAccept, "Sec-Fetch-Mode: navigate"), want: true},
		{name: "a browser checking a page", head: request("HEAD / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"), want: true},
		{name: "an older browser", head: request("GET / HTTP/1.1", "Host: shop", htmlAccept), want: true},
		{name: "a script's fetch", head: request("GET /api/orders HTTP/1.1", "Host: shop", "Accept: */*", "Sec-Fetch-Mode: cors")},
		{name: "a script asking for HTML", head: request("GET /partial HTTP/1.1", "Host: shop", htmlAccept, "Sec-Fetch-Mode: cors")},
		{name: "curl", head: request("GET / HTTP/1.1", "Host: shop", "User-Agent: curl/8.7.1", "Accept: */*")},
		{name: "a form submission", head: request("POST /checkout HTTP/1.1", "Host: shop", htmlAccept, "Sec-Fetch-Mode: navigate")},
		{name: "a WebSocket", head: request("GET /ws HTTP/1.1", "Host: shop", "Upgrade: websocket", "Connection: Upgrade", "Sec-Fetch-Mode: websocket")},
		{name: "an older client's WebSocket", head: request("GET /ws HTTP/1.1", "Host: shop", htmlAccept, "Upgrade: websocket", "Connection: Upgrade")},
		{name: "headers still arriving", head: []byte("GET / HTTP/1.1\r\nHost: shop\r\nSec-Fetch-Mode: navi")},
		{name: "TLS", head: []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01}},
		{name: "HTTP/2", head: []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")},
		{name: "nothing sent", head: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := isPageLoad(tt.head)
			assert.Equal(t, tt.want, got)
		})
	}
}

func readPage(t *testing.T, raw []byte, method string) (*http.Response, string) {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), &http.Request{Method: method})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

func TestWritePage(t *testing.T) {
	req, ok := isPageLoad(request("GET / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"))
	require.True(t, ok)
	var out bytes.Buffer

	require.NoError(t, writePage(&out, req, pageData{
		Address: "admin.sandbox-42.example.dev", Service: "checkout", Workload: "checkout-api",
		Namespace: "sandbox-42", Elapsed: "14s", PausedAgo: "3 hours ago",
	}))

	resp, body := readPage(t, out.Bytes(), http.MethodGet)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a 503, so nothing mistakes it for the app")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "3", resp.Header.Get("Retry-After"))
	assert.True(t, resp.Close, "the connection closes after the page")
	assert.Contains(t, body, "<title>Waking up checkout</title>")
	assert.Contains(t, body, `<meta http-equiv="refresh" content="3">`)
	assert.Contains(t, body, "<header>admin.sandbox-42.example.dev</header>")
	assert.Contains(t, body, `<li class="now">`)
	assert.Contains(t, body, "Starting checkout-api")
	assert.Contains(t, body, ">14s<")
	assert.Contains(t, body, "<dd>3 hours ago</dd>")
	assert.Contains(t, body, "Paused by Hybernate")
}

// Once a pod is Ready, the page says so and reloads straight into the app.
func TestWritePage_Ready(t *testing.T) {
	req, _ := isPageLoad(request("GET / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"))
	var out bytes.Buffer

	require.NoError(t, writePage(&out, req, pageData{Service: "checkout", Workload: "checkout-api", Ready: true}))

	resp, body := readPage(t, out.Bytes(), http.MethodGet)
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))
	assert.Contains(t, body, `<meta http-equiv="refresh" content="1">`)
	assert.Contains(t, body, "<title>checkout is ready</title>")
	assert.Contains(t, body, `class="wrap ready"`)
	assert.NotContains(t, body, `class="now"`, "every step is done")
}

func TestPageAddress(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{host: "admin.sandbox-42.example.dev", want: "admin.sandbox-42.example.dev"},
		{host: "admin.sandbox-42.example.dev:8443", want: "admin.sandbox-42.example.dev"},
		{host: "[fd00::1]:80", want: "fd00::1"},
		{host: "", want: "sandbox-42/checkout"},
	}
	for _, tt := range tests {
		got := pageAddress(&http.Request{Host: tt.host}, "sandbox-42", "checkout")
		assert.Equal(t, tt.want, got, tt.host)
	}
}

func TestAgoLabel(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{12 * time.Minute, "12 minutes ago"},
		{time.Hour, "1 hour ago"},
		{3*time.Hour + 20*time.Minute, "3 hours ago"},
		{47 * time.Hour, "47 hours ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, agoLabel(tt.d), tt.d.String())
	}
}

func TestWritePage_HEADHasNoBody(t *testing.T) {
	req, ok := isPageLoad(request("HEAD / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"))
	require.True(t, ok)
	var out bytes.Buffer

	require.NoError(t, writePage(&out, req, pageData{Service: "checkout"}))

	assert.True(t, bytes.HasSuffix(out.Bytes(), []byte("\r\n\r\n")), "headers only")
	resp, _ := readPage(t, out.Bytes(), http.MethodHead)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestWritePage_EscapesNames(t *testing.T) {
	req, _ := isPageLoad(request("GET / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"))
	var out bytes.Buffer

	require.NoError(t, writePage(&out, req, pageData{Service: "<script>x</script>", Address: "<img src=x>"}))

	assert.NotContains(t, out.String(), "<script>x")
	assert.NotContains(t, out.String(), "<img src=x>", "the Host header is the caller's, so it's escaped too")
}

func TestSinceLabel(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "just now"},
		{1500 * time.Millisecond, "just now"},
		{14 * time.Second, "14s"},
		{75 * time.Second, "1m 15s"},
		{10*time.Minute + 3*time.Second, "10m 03s"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, sinceLabel(tt.d), tt.d.String())
	}
}
