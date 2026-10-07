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
			req, ok := parseRequest(tt.head)
			assert.Equal(t, tt.want, ok && isPageLoad(req))
		})
	}
}

func TestIsHealthCheck(t *testing.T) {
	tests := []struct {
		method string
		agent  string
		want   bool
	}{
		{method: "GET", agent: "kube-probe/1.31", want: true},
		{method: "GET", agent: "Prometheus/2.53.0", want: true},
		{method: "HEAD", agent: "ELB-HealthChecker/2.0", want: true},
		{method: "GET", agent: "GoogleHC/1.0", want: true},
		{method: "GET", agent: "Envoy/HC", want: true},
		{method: "GET", agent: "OpenTelemetry Collector Contrib/0.104.0 (linux/amd64)", want: true},
		{method: "GET", agent: "otelcol-contrib/0.110.0", want: true},
		{method: "GET", agent: "Datadog Agent/7.55.0", want: true},
		{method: "GET", agent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5)"},
		{method: "GET", agent: "curl/8.7.1"},
		{method: "GET", agent: ""},
		// Remote write and Alertmanager notifications send real data with
		// the same agents; dropping them would lose samples and alerts.
		{method: "POST", agent: "Prometheus/2.53.0"},
		{method: "POST", agent: "Alertmanager/0.27.0"},
		{method: "PUT", agent: "GrafanaAgent/0.40.0"},
	}
	for _, tt := range tests {
		req, ok := parseRequest(request(tt.method+" /healthz HTTP/1.1", "Host: shop", "User-Agent: "+tt.agent))
		require.True(t, ok)
		assert.Equal(t, tt.want, isHealthCheck(req, defaultHealthCheckAgents), "%s %s", tt.method, tt.agent)
	}
}

// Agents added with Options.HealthCheckAgents are recognised alongside the
// built-in ones, and a blank one is dropped rather than matching everything.
func TestHealthCheckAgents_AddToTheBuiltIn(t *testing.T) {
	agents := healthCheckAgents([]string{"MyCorpMonitor/", "  uptime-kuma ", "", "   "})
	probe := func(agent string) bool {
		req, ok := parseRequest(request("GET /healthz HTTP/1.1", "Host: shop", "User-Agent: "+agent))
		require.True(t, ok)
		return isHealthCheck(req, agents)
	}

	assert.True(t, probe("MyCorpMonitor/3.1"))
	assert.True(t, probe("uptime-kuma/1.23"))
	assert.True(t, probe("kube-probe/1.31"), "the built-in agents still count")
	assert.False(t, probe("Mozilla/5.0 (X11; Linux x86_64)"))
	assert.Len(t, agents, len(defaultHealthCheckAgents)+2)
}

// Scanners probe every address for hidden files, hoping for leaked secrets;
// none of them is anything an app serves its users.
func TestIsHiddenFileRequest(t *testing.T) {
	tests := map[string]bool{
		"/.env":                             true,
		"/.env.production":                  true,
		"/.git/config":                      true,
		"/.aws/credentials":                 true,
		"/.ssh/id_rsa":                      true,
		"/api/.env":                         true,
		"/static/.DS_Store":                 true,
		"/%2eenv":                           true,
		"/.well-known/acme-challenge/token": false,
		"/.well-known/openid-configuration": false,
		"/api/.well-known/x":                true,
		"/":                                 false,
		"/index.html":                       false,
		"/api/v1/users":                     false,
		"/files/report.v2.pdf":              false,
		"/a/../b":                           false,
		"/./index.html":                     false,
	}
	for path, want := range tests {
		req, ok := parseRequest(request("GET "+path+" HTTP/1.1", "Host: shop"))
		require.True(t, ok, path)
		assert.Equal(t, want, isHiddenFileRequest(req), path)
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

func pageLoad(t *testing.T, method string) *http.Request {
	t.Helper()
	req, ok := parseRequest(request(method+" / HTTP/1.1", "Host: shop", "Sec-Fetch-Mode: navigate"))
	require.True(t, ok)
	require.True(t, isPageLoad(req))
	return req
}

func TestWritePage(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, writePage(&out, pageLoad(t, http.MethodGet), pageData{
		Address: "admin.preview-42.example.dev", Elapsed: "14s",
	}))

	resp, body := readPage(t, out.Bytes(), http.MethodGet)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a 503, so nothing mistakes it for the app")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "3", resp.Header.Get("Retry-After"))
	assert.True(t, resp.Close, "the connection closes after the page")
	assert.Contains(t, body, "<title>Waking up admin.preview-42.example.dev</title>")
	assert.Contains(t, body, `<meta http-equiv="refresh" content="3">`)
	assert.Contains(t, body, "<header>admin.preview-42.example.dev</header>")
	assert.Contains(t, body, `<li class="now">`)
	assert.Contains(t, body, ">14s<")
	assert.Contains(t, body, "Paused by Hybernate")
}

// The page is shown to whoever reaches the Service, so it says nothing about
// what runs inside the cluster.
func TestWritePage_NamesNothingInTheCluster(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, writePage(&out, pageLoad(t, http.MethodGet), pageData{}))

	_, body := readPage(t, out.Bytes(), http.MethodGet)
	assert.Contains(t, body, "Waking up <span>this app</span>", "a request with no host gets a generic name")
	for _, internal := range []string{"Namespace", "Service", "Paused</dt>"} {
		assert.NotContains(t, body, internal)
	}
}

func TestPageAddress(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{host: "admin.preview-42.example.dev", want: "admin.preview-42.example.dev"},
		{host: "admin.preview-42.example.dev:8443", want: "admin.preview-42.example.dev"},
		{host: "[fd00::1]:80", want: "fd00::1"},
		{host: "", want: ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, pageAddress(&http.Request{Host: tt.host}), tt.host)
	}
}

func TestWritePage_HEADHasNoBody(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, writePage(&out, pageLoad(t, http.MethodHead), pageData{Address: "shop"}))

	assert.True(t, bytes.HasSuffix(out.Bytes(), []byte("\r\n\r\n")), "headers only")
	resp, _ := readPage(t, out.Bytes(), http.MethodHead)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestWritePage_EscapesTheHost(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, writePage(&out, pageLoad(t, http.MethodGet), pageData{Address: "<img src=x>"}))

	assert.NotContains(t, out.String(), "<img src=x>", "the Host header is the caller's, so it's escaped")
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
