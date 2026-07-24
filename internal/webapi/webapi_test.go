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

// TestRecordHTTPMetric_boundsCardinalityOnUnmatchedRoutes pins the
// cardinality bound on registrystats_http_requests_total through the
// composed webhttp.Logging middleware and its request-aware metric hook. A
// matched route records its real {method,path}; an unmatched route
// (r.Pattern == "") collapses BOTH labels to "unmatched" so an arbitrary
// client-supplied method or path on a 404/405 cannot mint unbounded metric
// series in Mimir. The collapse is observable only via the metrics
// exposition: the access log deliberately keeps the raw path, so a log
// assertion cannot witness it.
func TestRecordHTTPMetric_boundsCardinalityOnUnmatchedRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := webhttp.Logging(
		webhttp.WithLogger(testsupport.QuietLogger()),
		webhttp.WithRecordMetricRequest(recordHTTPMetric),
	)(mux)

	// Matched route keeps its real method+path labels.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))
	// Unmatched path with an arbitrary client method token: both labels collapse.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("WEIRDPROBE", "/wp-login.php", nil))

	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `method="unmatched",path="unmatched"`) {
		t.Errorf("unmatched route did not collapse to method=unmatched,path=unmatched; metrics:\n%s", body)
	}
	if strings.Contains(body, `method="WEIRDPROBE"`) {
		t.Errorf("raw client method token leaked into a metric label (cardinality bound broken); metrics:\n%s", body)
	}
	if strings.Contains(body, `path="/wp-login.php"`) {
		t.Errorf("raw unmatched path leaked into a metric label (cardinality bound broken); metrics:\n%s", body)
	}
	if !strings.Contains(body, `path="/api/health"`) {
		t.Errorf("matched route lost its real path label (over-collapse); metrics:\n%s", body)
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
