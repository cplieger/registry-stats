package webapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/metrics"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// fakeHealth is an in-memory api.HealthSignal for handler tests.
type fakeHealth struct {
	healthy atomic.Bool
}

// Compile-time assertion: *fakeHealth satisfies api.HealthSignal.
var _ api.HealthSignal = (*fakeHealth)(nil)

func (f *fakeHealth) Set(ok bool) { f.healthy.Store(ok) }

func (f *fakeHealth) Healthy() bool { return f.healthy.Load() }

// TestHealthHandler exercises the observable behavior of GET /api/health
// across all three branches: a ready signal returns 200 {"status":"ok"};
// an unready signal and a nil signal both return 503 with
// {"status":"unready",...}. The response is always JSON.
func TestHealthHandler(t *testing.T) {
	healthy := &fakeHealth{}
	healthy.Set(true)
	unhealthy := &fakeHealth{} // zero value reports unhealthy

	tests := []struct {
		name        string
		signal      api.HealthSignal
		wantStatus  int
		wantStatusF string
	}{
		{"ready signal returns 200 ok", healthy, http.StatusOK, "ok"},
		{"unready signal returns 503 unready", unhealthy, http.StatusServiceUnavailable, "unready"},
		{"nil signal returns 503 unready", nil, http.StatusServiceUnavailable, "unready"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandlers(tt.signal, testsupport.QuietLogger())
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)

			h.health(rec, req)

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
			if body["status"] != tt.wantStatusF {
				t.Errorf("status field = %q, want %q (body=%q)", body["status"], tt.wantStatusF, rec.Body.String())
			}
		})
	}
}

func TestWithAccessLog_levelByStatus(t *testing.T) {
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
			WithAccessLog(next, logger).ServeHTTP(rec, req)

			logs := buf.String()
			if !strings.Contains(logs, "http request") {
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
			h := &fakeHealth{}
			h.Set(true)
			srv := New(Deps{
				Health:        h,
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

// TestWithAccessLog_boundsMetricCardinalityOnUnmatchedRoutes pins the
// cardinality bound on registrystats_http_requests_total. A matched route
// records its real {method,path}; an unmatched route (r.Pattern == "")
// collapses BOTH labels to "unmatched" so an arbitrary client-supplied
// method or path on a 404/405 cannot mint unbounded metric series in Mimir.
// The collapse is observable only via the metrics exposition: the access
// log deliberately keeps the raw path, so a log assertion cannot witness it.
func TestWithAccessLog_boundsMetricCardinalityOnUnmatchedRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := WithAccessLog(mux, testsupport.QuietLogger())

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

// TestNewHandlers_nilLoggerFallsBackToNonNil pins newHandlers' documented
// contract: a nil logger is replaced by a usable (non-nil) default. Without
// the fallback the shared error-logging helper would hold a nil logger, so
// the constructor must never leave the field nil.
func TestNewHandlers_nilLoggerFallsBackToNonNil(t *testing.T) {
	h := newHandlers(&fakeHealth{}, nil)
	if h.logger == nil {
		t.Error("newHandlers(_, nil).logger = nil, want a non-nil fallback logger")
	}
}

// TestWriteJSON_logsOnEncodeFailure verifies writeJSON's documented "logs on
// error" behavior: when the value cannot be JSON-encoded and a logger is
// present, the failure is logged. A channel is an unencodable type, so the
// encoder returns an error. This pins both halves of the error guard
// (err != nil AND logger != nil).
func TestWriteJSON_logsOnEncodeFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	writeJSON(httptest.NewRecorder(), make(chan int), logger)

	if !strings.Contains(buf.String(), "failed to write JSON response") {
		t.Errorf("writeJSON did not log on encode failure; logs: %q", buf.String())
	}
}

// TestWriteJSON_silentOnSuccessfulEncode verifies the converse: a value that
// encodes cleanly produces no error log, so the success path stays quiet
// rather than logging a spurious failure.
func TestWriteJSON_silentOnSuccessfulEncode(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	writeJSON(httptest.NewRecorder(), map[string]string{"status": "ok"}, logger)

	if strings.Contains(buf.String(), "failed to write JSON response") {
		t.Errorf("writeJSON logged an error on a successful encode; logs: %q", buf.String())
	}
}

// TestNew_usesSuppliedLoggerForAccessLog confirms New wires the caller's
// logger (not a fresh default) into the access-log middleware: a request
// routed through the returned server's handler emits its access-log line
// into the supplied logger's sink.
func TestNew_usesSuppliedLoggerForAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &fakeHealth{}
	h.Set(true)
	srv := New(Deps{Health: h, Logger: logger})

	srv.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if !strings.Contains(buf.String(), "http request") {
		t.Errorf("access log did not land in the supplied logger; logs: %q", buf.String())
	}
}

// TestNew_listenAddrDefaultAndOverride pins New's address selection: an empty
// ListenAddr falls back to the package default :9100, and a non-empty
// ListenAddr is used verbatim on the returned server.
func TestNew_listenAddrDefaultAndOverride(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		wantAddr   string
	}{
		{"empty falls back to default", "", ":9100"},
		{"explicit address is used verbatim", ":18080", ":18080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(Deps{Health: &fakeHealth{}, Logger: testsupport.QuietLogger(), ListenAddr: tt.listenAddr})
			if srv.Addr != tt.wantAddr {
				t.Errorf("New(ListenAddr=%q).Addr = %q, want %q", tt.listenAddr, srv.Addr, tt.wantAddr)
			}
		})
	}
}
