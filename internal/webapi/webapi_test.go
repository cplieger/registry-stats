package webapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/registry-stats/v2/internal/metrics"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
	"github.com/cplieger/webhttp"
)

// TestNew_readinessEndpoint pins the wiring of GET /api/health onto webhttp's
// readiness gate through New's full middleware chain: 200 {"status":"ok"} when
// the injected Ready view reports ready, and 503 {"status":"unready"} when it
// does not — including a nil view, which New defaults to a not-ready gate
// rather than panicking. This is the HTTP serving-readiness gate, distinct from
// the container file-marker liveness probe.
func TestNew_readinessEndpoint(t *testing.T) {
	readyTrue := &webhttp.Ready{}
	readyTrue.Set(true)

	tests := []struct {
		name       string
		ready      webhttp.ReadinessChecker
		wantStatus int
		wantField  string
	}{
		{"ready view returns 200 ok", readyTrue, http.StatusOK, "ok"},
		{"unready view returns 503 unready", &webhttp.Ready{}, http.StatusServiceUnavailable, "unready"},
		{"nil view returns 503 unready", nil, http.StatusServiceUnavailable, "unready"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(Deps{Ready: tt.ready, Logger: testsupport.QuietLogger()})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)

			srv.Handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body is not JSON: %v (body=%q)", err, rec.Body.String())
			}
			if body["status"] != tt.wantField {
				t.Errorf("status field = %q, want %q (body=%q)", body["status"], tt.wantField, rec.Body.String())
			}
		})
	}
}

// TestNew_appliesSecurityHeaders confirms the webhttp.SecurityHeaders baseline
// is wired into New's middleware chain: every response carries nosniff, the
// DENY frame guard, and the referrer policy, and neither CSP nor HSTS is set
// (this is a non-browser metrics/health endpoint).
func TestNew_appliesSecurityHeaders(t *testing.T) {
	readyTrue := &webhttp.Ready{}
	readyTrue.Set(true)
	srv := New(Deps{Ready: readyTrue, Logger: testsupport.QuietLogger()})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	h := rec.Header()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := h.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	if got := h.Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q, want empty (no CSP on a metrics endpoint)", got)
	}
	if got := h.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want empty (HSTS off)", got)
	}
}

// TestAccessLogLevel_byStatus pins the app's level POLICY (fed to
// webhttp.WithLogLevel) and its wiring: 2xx/3xx at DEBUG (scrape-quiet),
// 4xx at WARN, 5xx at ERROR, observed end-to-end through the composed
// webhttp.Logging middleware.
func TestAccessLogLevel_byStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantLevel string
	}{
		{"2xx logs at debug", http.StatusOK, "level=DEBUG"},
		{"3xx logs at debug", http.StatusMovedPermanently, "level=DEBUG"},
		{"4xx logs at warn", http.StatusNotFound, "level=WARN"},
		{"5xx logs at error", http.StatusInternalServerError, "level=ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			webhttp.Logging(
				webhttp.WithLogger(logger),
				webhttp.WithLogLevel(accessLogLevel),
			)(next).ServeHTTP(rec, req)

			logs := buf.String()
			if !strings.Contains(logs, "msg=http") {
				t.Fatalf("status %d: no access-log line emitted, got %q", tt.status, logs)
			}
			if !strings.Contains(logs, tt.wantLevel) {
				t.Errorf("status %d: access-log level = %q, want %q", tt.status, logs, tt.wantLevel)
			}
		})
	}
}

func TestNew_metricsRoutingHonorsEnableMetrics(t *testing.T) {
	tests := []struct {
		name          string
		enableMetrics bool
		wantStatus    int
	}{
		{"enabled serves metrics", true, http.StatusOK},
		{"disabled hides metrics", false, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &webhttp.Ready{}
			r.Set(true)
			srv := New(Deps{
				Ready:         r,
				Logger:        testsupport.QuietLogger(),
				EnableMetrics: tt.enableMetrics,
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			srv.Handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("GET /metrics with EnableMetrics=%v: status = %d, want %d", tt.enableMetrics, rec.Code, tt.wantStatus)
			}
		})
	}
}

// TestNew_boundsHTTPMetricCardinality pins the label vocabulary of
// registrystats_http_requests_total through New's REAL route table and
// middleware chain — which is where the bound now lives. The labels are
// derived by webhttp's WithRecordRouteMetric and handed to
// metrics.RecordHTTP; this app has no derivation of its own, so the property
// under test is the WIRING: that New reaches for the hook whose labels are
// bounded by construction rather than the request-aware one.
//
// Two halves. The bound: a client-chosen method token collapses to the fixed
// "other" bucket and an unmatched path to the fixed "unmatched" marker, so a
// scanner cannot mint series in Mimir. The non-collapse: a matched route
// keeps its real method and route template — including HEAD, which ServeMux
// routes to the GET pattern and which the metric must still record as HEAD
// so the metric and the access line for one request_id agree — and an
// unmatched request keeps its real METHOD, only the path collapsing. That
// last part is a deliberate change from the app's former hand-rolled
// derivation, which collapsed both labels onto method="unmatched"; the
// retired value is asserted absent below.
//
// Observable only via the metrics exposition: the access log deliberately
// keeps the raw path and the verbatim method, so a log assertion cannot
// witness the collapse.
func TestNew_boundsHTTPMetricCardinality(t *testing.T) {
	const hostilePunct = "M!#$%&'*+-.^_`|~" // every byte a valid RFC 9110 tchar

	ready := &webhttp.Ready{}
	ready.Set(true)
	srv := New(Deps{Ready: ready, Logger: testsupport.QuietLogger(), EnableMetrics: true})

	for _, req := range []struct{ method, target string }{
		{http.MethodGet, "/api/health"},   // matched
		{http.MethodHead, "/api/health"},  // matched via the GET pattern
		{http.MethodGet, "/metrics"},      // matched, second route
		{http.MethodPost, "/api/health"},  // 405: path matches, method does not
		{http.MethodGet, "/wp-login.php"}, // 404
		{"WEIRDPROBE", "/wp-login.php"},   // 404 with an arbitrary method token
		{hostilePunct, "/api/health"},     // 405 with a punctuation-only token
	} {
		srv.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(req.method, req.target, nil))
	}

	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	present := map[string]string{
		`{method="GET",path="/api/health",status="200"}`:  "matched route lost its real method+template (over-collapse)",
		`{method="HEAD",path="/api/health",status="200"}`: "HEAD was not preserved: ServeMux routes it to the GET pattern, but the metric must still record HEAD so it agrees with the access line for the same request_id",
		`{method="GET",path="/metrics",status="200"}`:     "second matched route lost its real method+template",
		`{method="POST",path="unmatched",status="405"}`:   "a 405 must keep its real method and collapse only the path",
		`{method="GET",path="unmatched",status="404"}`:    "a 404 must keep its real method and collapse only the path",
		`{method="other",path="unmatched",status="404"}`:  "a non-standard method must bucket to \"other\", not vanish",
	}
	for series, why := range present {
		t.Run("present "+series, func(t *testing.T) {
			if !strings.Contains(body, series) {
				t.Errorf("%s: %s missing from exposition:\n%s", why, series, body)
			}
		})
	}

	absent := map[string]string{
		`method="WEIRDPROBE"`:  "a client-supplied method token reached a metric label (cardinality bound broken)",
		hostilePunct:           "a punctuation-only method token reached a metric label (cardinality bound broken)",
		`path="/wp-login.php"`: "a raw unmatched path reached a metric label (cardinality bound broken)",
		`method="unmatched"`:   "the retired method collapse is back: the method label is bounded by a closed nine-method set plus \"other\", so an unmatched request keeps its real method and only the path collapses",
	}
	for probe, why := range absent {
		t.Run("absent "+probe, func(t *testing.T) {
			if strings.Contains(body, probe) {
				t.Errorf("%s: found %q in exposition:\n%s", why, probe, body)
			}
		})
	}
}

// TestNew_usesSuppliedLoggerForAccessLog confirms New wires the caller's
// logger (not a fresh default) into the access-log middleware: a request
// routed through the returned server's handler emits its access-log line
// into the supplied logger's sink.
func TestNew_usesSuppliedLoggerForAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &webhttp.Ready{}
	r.Set(true)
	srv := New(Deps{Ready: r, Logger: logger})

	srv.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if !strings.Contains(buf.String(), "msg=http") {
		t.Errorf("access log did not land in the supplied logger; logs: %q", buf.String())
	}
}
