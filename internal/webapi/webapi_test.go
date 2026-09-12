package webapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/webhttp/v2"
)

// TestNew_readinessEndpoint pins the wiring of GET /api/health onto webhttp's
// readiness gate through New's full middleware chain: 200 {"status":"ok"} when
// the injected Ready view reports ready, and 503 {"status":"unready"} when it
// does not. This is the HTTP serving-readiness gate, distinct from the
// container file-marker liveness probe.
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(Deps{Metrics: obs.New(), Ready: tt.ready, Logger: slog.New(slog.DiscardHandler)})
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

type panicOnceReadiness struct{ calls int }

func (r *panicOnceReadiness) Ready() bool {
	r.calls++
	if r.calls == 1 {
		panic("readiness boom")
	}
	return true
}

func TestNew_recoversHandlerPanicWithTruthfulAccessStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(Deps{Metrics: obs.New(), Ready: &panicOnceReadiness{}, Logger: logger})

	first := httptest.NewRecorder()
	srv.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if first.Code != http.StatusInternalServerError {
		t.Errorf("GET /api/health after handler panic status = %d, want %d", first.Code, http.StatusInternalServerError)
	}
	logs := buf.String()
	if count := strings.Count(logs, "msg=http"); count != 1 {
		t.Errorf("GET /api/health panic access record count = %d, want 1; logs: %q", count, logs)
	}
	if !strings.Contains(logs, "status=500") {
		t.Errorf("GET /api/health panic access status is not 500; logs: %q", logs)
	}

	second := httptest.NewRecorder()
	srv.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if second.Code != http.StatusOK {
		t.Errorf("GET /api/health after recovered panic status = %d, want %d", second.Code, http.StatusOK)
	}
}

// TestNew_boundsRequestReadAndWrite pins the guarantee the library does not
// make: webhttp leaves ReadTimeout and WriteTimeout unset, so dropping either
// option turns a bounded request into an unbounded one. ReadHeaderTimeout and
// IdleTimeout are deliberately not asserted: webhttp supplies non-zero
// defaults not part of this package's contract.
func TestNew_boundsRequestReadAndWrite(t *testing.T) {
	srv := New(Deps{Metrics: obs.New(), Ready: &webhttp.Ready{}, Logger: slog.New(slog.DiscardHandler)})

	if srv.ReadTimeout <= 0 {
		t.Errorf("New().ReadTimeout = %s, want a positive deadline (webhttp leaves it unset)", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("New().WriteTimeout = %s, want a positive deadline (webhttp leaves it unset)", srv.WriteTimeout)
	}
}

// TestNew_appliesSecurityHeaders confirms the webhttp.SecurityHeaders baseline
// is wired into New's middleware chain: nosniff, the DENY frame guard, and
// the referrer policy on every response, with neither CSP nor HSTS set (a
// non-browser metrics/health endpoint).
func TestNew_appliesSecurityHeaders(t *testing.T) {
	readyTrue := &webhttp.Ready{}
	readyTrue.Set(true)
	srv := New(Deps{Metrics: obs.New(), Ready: readyTrue, Logger: slog.New(slog.DiscardHandler)})

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

func TestNew_unreadyHealthLogMatchesAlertExclusion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(Deps{Metrics: obs.New(), Ready: &webhttp.Ready{}, Logger: logger})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/health status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	logs := buf.String()
	if count := strings.Count(logs, "msg=http"); count != 1 {
		t.Errorf("GET /api/health access record count = %d, want 1; logs: %q", count, logs)
	}
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("GET /api/health access level is not ERROR; logs: %q", logs)
	}
	if !strings.Contains(logs, "path=/api/health status=503") {
		t.Errorf("GET /api/health access record does not match the shipped exclusion; logs: %q", logs)
	}
}

func TestNew_successfulMetricsScrapeIsDebugOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv := New(Deps{Metrics: obs.New(), Ready: &webhttp.Ready{}, Logger: logger})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET /metrics status = %d, want %d", rec.Code, http.StatusOK)
	}
	if logs := buf.String(); logs != "" {
		t.Errorf("successful GET /metrics emitted an Info access record, want Debug-only; logs: %q", logs)
	}
}
