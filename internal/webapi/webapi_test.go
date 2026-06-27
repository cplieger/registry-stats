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
