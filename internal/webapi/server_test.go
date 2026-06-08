package webapi

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/registry-stats/internal/testsupport"
)

// TestMemStore_StoreContract verifies that the in-memory fake satisfies
// the same api.Store contract as store.FS, preventing silent drift.

func TestWriteJSON_setsHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]int{"a": 1}, testsupport.QuietLogger())

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if xfo := w.Header().Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", xfo)
	}
	if rp := w.Header().Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", rp)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestWriteJSON_nilSlice(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, []string(nil), testsupport.QuietLogger())
	if got := strings.TrimSpace(w.Body.String()); got != "null" {
		t.Errorf("body = %q, want null", got)
	}
}

// --- server lifecycle ---

func TestStatusRecorder_default200(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
	if rec.Status != http.StatusOK {
		t.Errorf("default Status = %d, want 200", rec.Status)
	}
	rec.WriteHeader(http.StatusTeapot)
	if rec.Status != http.StatusTeapot {
		t.Errorf("Status after WriteHeader = %d, want 418", rec.Status)
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("wrapped writer Status = %d, want 418", w.Code)
	}
}

func TestWithAccessLog_passesThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "nope")
	})
	wrapped := WithAccessLog(next, testsupport.QuietLogger())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	if !called {
		t.Error("inner handler not called")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if w.Body.String() != "nope" {
		t.Errorf("body = %q, want nope", w.Body.String())
	}
}

func TestNew_wiresRoutes(t *testing.T) {
	store := testsupport.NewMemStore()
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 42, 0))
	health := &fakeHealth{}
	health.Set(true)
	srv := New(Deps{
		Store:         store,
		Health:        health,
		Logger:        testsupport.QuietLogger(),
		ListenAddr:    ":0",
		EnableJSONAPI: true,
		EnableMetrics: true,
	})

	// Exercise /api/health through the wired handler.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", http.NoBody)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/api/health status = %d, want 200", w.Code)
	}

	// Exercise /api/snapshot.
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot", http.NoBody)
	w2 := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("/api/snapshot status = %d, want 200", w2.Code)
	}
}

func TestNew_defaultsListenAddr(t *testing.T) {
	srv := New(Deps{
		Store:  testsupport.NewMemStore(),
		Health: &fakeHealth{},
		Logger: testsupport.QuietLogger(),
	})
	if srv.Addr != ":9100" {
		t.Errorf("Addr = %q, want %q", srv.Addr, ":9100")
	}
}

func TestShutdown_setsUnhealthy(t *testing.T) {
	store := testsupport.NewMemStore()
	health := &fakeHealth{}
	health.Set(true)
	srv := New(Deps{Store: store, Health: health, Logger: testsupport.QuietLogger(), ListenAddr: ":0"})

	Shutdown(srv, health, nil, testsupport.QuietLogger())
	if health.Healthy() {
		t.Error("health should be false after Shutdown")
	}
}

// --- Handler tests migrated from main_test.go (Phase-3 step 2) ---
//
// These tests moved out of the main package when the shim handler
// bodies (handleHealth / handleSnapshot / handlePulls /
// handlePullsDaily / handleSummary) and their helpers
// (resolveSnapshot / filteredPulls / forEachSummaryEntry / writeJSON
// / dateToISO / withAccessLog / statusRecorder / dailyDelta) were
// deleted from main.go. Tests use the in-package memStore + fakeHealth
// harness already defined at the top of this file so no disk or
// httptest.Server involvement is needed for the HTTP-surface cases.
//
// Preserved inviolate contracts covered by these tests:
//   - HTTP status codes (200, 404, 500, 503)
//   - JSON response shapes (pulls / pulls/daily / summary row fields)
//   - Grafana query vocabulary ({dockerhub}, {ghcr}, $__all, {$__all},
//     multi-value braces)
//   - first_seen annotation + omitempty contract for /api/pulls/daily
//   - Zero-download GHCR exclusion at both filteredPulls and summary
//     paths
//   - Cross-registry merging (same name in both registries sums pulls)

// pullsDailyTwoDaySnap is a small helper that seeds two days of a
// single DockerHub repo. Used by the first_seen / smoothing tests.

func TestWriteJSON_emptySlice(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, []string{}, testsupport.QuietLogger())
	if got := w.Body.String(); got != "[]\n" {
		t.Errorf("empty slice JSON = %q, want []\n", got)
	}
}

func TestWriteJSON_unencodableValueDoesNotPanic(t *testing.T) {
	w := httptest.NewRecorder()
	// math.NaN is unencodable by encoding/json — must not panic;
	// headers must still be set by the time Encode fails.
	writeJSON(w, math.NaN(), testsupport.QuietLogger())

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
}

func TestWriteJSON_variousTypes(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"nil slice", []string(nil), "null\n"},
		{"empty map", map[string]int{}, "{}\n"},
		{"nested struct", struct {
			B string `json:"b"`
			A int    `json:"a"`
		}{"x", 1}, "{\"b\":\"x\",\"a\":1}\n"},
		{"integer", 42, "42\n"},
		{"boolean", true, "true\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeJSON(w, c.v, testsupport.QuietLogger())
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if got := w.Body.String(); got != c.want {
				t.Errorf("writeJSON(%v) = %q, want %q", c.v, got, c.want)
			}
		})
	}
}

func TestWriteJSON_contentTypeBeforeBody(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"key": "val"}, testsupport.QuietLogger())

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := w.Body.String(); body != "{\"key\":\"val\"}\n" {
		t.Errorf("body = %q, want %q", body, "{\"key\":\"val\"}\n")
	}
}

// --- WithAccessLog captures varied status codes ---

func TestWithAccessLog_capturesStatus(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantBody   string
		wantStatus int
	}{
		{
			name: "default 200 when handler never calls WriteHeader",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "ok")
			},
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
		{
			name: "explicit 200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "ok")
			},
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
		{
			name: "4xx status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bad", http.StatusBadRequest)
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   "bad\n",
		},
		{
			name: "5xx status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "boom\n",
		},
		{
			name: "3xx status passes through",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusMovedPermanently)
			},
			wantStatus: http.StatusMovedPermanently,
			wantBody:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wrapped := WithAccessLog(c.handler, testsupport.QuietLogger())
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/test", http.NoBody)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)

			if w.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, c.wantStatus)
			}
			if got := w.Body.String(); got != c.wantBody {
				t.Errorf("body = %q, want %q", got, c.wantBody)
			}
		})
	}
}

// --- dailyDelta smoothing ---
