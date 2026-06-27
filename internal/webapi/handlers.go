// Package webapi implements the HTTP API server for registry-stats.
package webapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/cplieger/registry-stats/internal/api"
)

// handlers holds the per-request dependencies the HTTP endpoints need.
// Constructed once at startup by New and shared across requests.
type handlers struct {
	healthS api.HealthSignal
	logger  *slog.Logger
}

// newHandlers returns a handlers bound to the given dependencies.
// A nil logger falls back to slog.Default.
func newHandlers(health api.HealthSignal, logger *slog.Logger) *handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &handlers{healthS: health, logger: logger}
}

// health handles GET /api/health. Returns the canonical JSON envelope
// shared across the homelab's custom Go apps: 200 with {"status":"ok"}
// when the health signal reports ready, 503 with
// {"status":"unready","reason":"..."} otherwise.
func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.healthS == nil || !h.healthS.Healthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{
			"status": "unready",
			"reason": "health signal reports unhealthy (no successful collect yet)",
		}, h.logger)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"}, h.logger)
}

// writeJSON marshals v as JSON and writes it to w. Logs on error.
func writeJSON(w http.ResponseWriter, v any, logger *slog.Logger) {
	if err := json.NewEncoder(w).Encode(v); err != nil && logger != nil {
		logger.Error("failed to write JSON response", "error", err)
	}
}
