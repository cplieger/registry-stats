// Package testsupport provides test helpers shared across registry-stats test packages.
package testsupport

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"
)

// RedirectTransport rewrites all outbound requests to point at a local
// test server. This lets tests exercise functions with hardcoded base
// URLs without modifying production code.
type RedirectTransport struct {
	Target *httptest.Server
}

// RoundTrip rewrites the request URL to point at the configured test server, then delegates to http.DefaultTransport.
func (rt *RedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	u := *req.URL
	target, err := req.URL.Parse(rt.Target.URL)
	if err != nil {
		return nil, fmt.Errorf("RedirectTransport: parse target URL %q: %w", rt.Target.URL, err)
	}
	u.Scheme = target.Scheme
	u.Host = target.Host
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}

// MockClient returns an *http.Client that redirects all requests to srv.
func MockClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: &RedirectTransport{Target: srv},
		Timeout:   5 * time.Second,
	}
}

// QuietLogger returns a logger that discards output, suitable for
// tests that don't assert on log lines.
func QuietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
