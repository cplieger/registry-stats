package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cplieger/registry-stats/internal/api"
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
