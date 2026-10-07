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
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

// Mark is the Hybernate snowflake as an SVG path, drawn on the waking-up page
// and the scan report.
const Mark = "M20.79,13.95L18.46,14.57L16.46,12.57L18.46,10.57L20.79,11.19C21.15,11.28 21.52,11.06 21.6,10.7C21.69," +
	"10.34 21.47,9.97 21.11,9.89L18.79,9.27L19.41,6.95C19.5,6.59 19.27,6.22 18.91,6.14C18.56,6.05 18.19,6.27 18.1," +
	"6.63L17.5,8.95L15.5,10.95L13,8.45V5.86L14.79,4.07C15.08,3.78 15.08,3.29 14.79,3C14.5,2.71 14,2.71 13.71,3L12," +
	"4.71L10.29,3C10,2.71 9.5,2.71 9.21,3C8.92,3.29 8.92,3.78 9.21,4.07L11,5.86V8.45L8.5,10.95L6.5,8.95L5.9,6.63C5.81," +
	"6.27 5.44,6.05 5.09,6.14C4.73,6.22 4.5,6.59 4.59,6.95L5.21,9.27L2.89,9.89C2.53,9.97 2.31,10.34 2.4,10.7C2.5," +
	"11.06 2.85,11.28 3.21,11.19L5.54,10.57L7.54,12.57L5.54,14.57L3.21,13.95C2.85,13.86 2.5,14.08 2.4,14.44C2.31,14.8 " +
	"2.53,15.17 2.89,15.25L5.21,15.87L4.59,18.19C4.5,18.55 4.73,18.92 5.09,19C5.44,19.09 5.81,18.87 5.9,18.5L6.5," +
	"16.19L8.5,14.19L11,16.69V19.28L9.21,21.07C8.92,21.36 8.92,21.86 9.21,22.14C9.5,22.43 10,22.43 10.29,22.14L12," +
	"20.43L13.71,22.14C14,22.43 14.5,22.43 14.79,22.14C15.08,21.86 15.08,21.36 14.79,21.07L13,19.28V16.69L15.5,14.19L17.5," +
	"16.19L18.1,18.5C18.19,18.87 18.56,19.09 18.91,19C19.27,18.92 19.5,18.55 19.41,18.19L18.79,15.87L21.11,15.25C21.47," +
	"15.17 21.69,14.8 21.6,14.44C21.52,14.08 21.15,13.86 20.79,13.95Z"

// pageRefresh is how often the page reloads while the workload starts. The
// reload that finds it Ready is passed straight through to it.
const pageRefresh = 3 * time.Second

// pageData is all the page shows. It's served to whoever reaches the
// Service, so it names nothing inside the cluster: no namespace, workload,
// or Service name, and not when the workload was paused.
type pageData struct {
	// Address is the host the browser asked for, so the page reads as part
	// of what the person opened rather than as another site.
	Address string
	Elapsed string
	Refresh int
	Mark    string
}

// pageAddress is the host the browser asked for, without its port.
func pageAddress(req *http.Request) string {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

// httpMethods are the request lines that mark a connection as HTTP/1.x, or
// as the HTTP/2 preface.
var httpMethods = []string{"GET ", "HEAD ", "POST ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "CONNECT ", "TRACE ", "PRI "}

func looksLikeHTTP(head []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(head, []byte(m)) {
			return true
		}
	}
	return false
}

// parseRequest parses head, the first bytes of a connection, as an HTTP
// request, if it is one whose headers have all arrived.
func parseRequest(head []byte) (*http.Request, bool) {
	if !looksLikeHTTP(head) {
		return nil, false
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		return nil, false
	}
	return req, true
}

// defaultHealthCheckAgents are the User-Agent prefixes of load balancer
// health checks, kubelet probes and metrics scrapers. They reach a paused
// workload's Service on a schedule, so a request from one never wakes it.
// Options.HealthCheckAgents adds to them.
var defaultHealthCheckAgents = []string{
	"kube-probe/",
	"Prometheus/",
	"vm_promscrape",
	"GrafanaAgent/",
	"Alloy/",
	"OpenTelemetry Collector",
	"otelcol",
	"Datadog Agent/",
	"ELB-HealthChecker/",
	"GoogleHC/",
	"Envoy/HC",
}

// isHealthCheck reports whether req comes from a health checker or scraper,
// one whose User-Agent starts with one of agents. Only a GET or HEAD can be
// one: the same agents also send real traffic, such as Prometheus remote
// write and Alertmanager notifications, which are POSTs and must reach the
// workload.
func isHealthCheck(req *http.Request, agents []string) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	agent := req.UserAgent()
	for _, prefix := range agents {
		if strings.HasPrefix(agent, prefix) {
			return true
		}
	}
	return false
}

// writeUnavailable answers a health check: the workload is paused, so it
// isn't healthy, and it stays paused.
// isHiddenFileRequest reports whether req asks for a hidden file, one with
// a path segment that starts with a dot, such as /.env or /.git/config.
// Scanners crawl the internet for these, hoping for leaked secrets; no app
// serves them to its users, so such a request never wakes a workload.
// /.well-known/ is the exception, since real clients use it, as ACME
// certificate checks do.
func isHiddenFileRequest(req *http.Request) bool {
	for i, segment := range strings.Split(req.URL.Path, "/") {
		if !strings.HasPrefix(segment, ".") || segment == "." || segment == ".." {
			continue
		}
		if i == 1 && segment == ".well-known" {
			continue
		}
		return true
	}
	return false
}

func writeNotFound(w io.Writer) error {
	_, err := io.WriteString(w, "HTTP/1.1 404 Not Found\r\n"+
		"Content-Length: 0\r\n"+
		"Cache-Control: no-store\r\n"+
		"Connection: close\r\n\r\n")
	return err
}

func writeUnavailable(w io.Writer) error {
	_, err := io.WriteString(w, "HTTP/1.1 503 Service Unavailable\r\n"+
		"Content-Length: 0\r\n"+
		"Cache-Control: no-store\r\n"+
		"Connection: close\r\n\r\n")
	return err
}

// isPageLoad reports whether req is a browser loading a page: a GET or HEAD
// for a document, not a fetch from a script, an API client, or a protocol
// upgrade. Those still get held, since a page is no answer to them.
func isPageLoad(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if req.Header.Get("Upgrade") != "" {
		return false
	}
	// Browsers since 2020 send Sec-Fetch-Mode on every request, and only a
	// top-level or frame navigation is "navigate". Older ones are judged by
	// asking for HTML.
	if mode := req.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(req.Header.Get("Accept"), "text/html")
}

// writePage answers a page load with the waking-up page. It's a 503 so that
// nothing caches it or mistakes it for the app, and the connection closes
// after it.
func writePage(w io.Writer, req *http.Request, data pageData) error {
	data.Mark = Mark
	data.Refresh = int(pageRefresh.Seconds())
	var body bytes.Buffer
	if err := pageTemplate.Execute(&body, data); err != nil {
		return fmt.Errorf("rendering page: %w", err)
	}
	header := fmt.Sprintf("HTTP/1.1 503 Service Unavailable\r\n"+
		"Content-Type: text/html; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Cache-Control: no-store\r\n"+
		"Retry-After: %d\r\n"+
		"X-Robots-Tag: noindex\r\n"+
		"Connection: close\r\n\r\n", body.Len(), data.Refresh)
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("writing page header: %w", err)
	}
	if req.Method == http.MethodHead {
		return nil
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		return fmt.Errorf("writing page: %w", err)
	}
	return nil
}

// sinceLabel says how long ago the wake started, for the page.
func sinceLabel(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}
