// Package webapi implements the HTTP API server for registry-stats.
package webapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/webhttp/v2"
)

// webhttp leaves ReadTimeout/WriteTimeout unset for streaming handlers and defaults
// ReadHeaderTimeout/IdleTimeout to 10s/120s; nothing served here streams, so all four are set.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Deps supplies the HTTP server dependencies. Metrics, Ready and Logger are required.
type Deps struct {
	Metrics *obs.Metrics
	Ready   webhttp.ReadinessChecker
	Logger  *slog.Logger
}

// New constructs the server without binding or starting it.
func New(d Deps) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /api/health", webhttp.ReadinessHandler(d.Ready))
	mux.HandleFunc("GET /metrics", d.Metrics.Handler())

	// Logging wraps Recoverer, which buys legibility rather than survival:
	// net/http recovers a handler panic either way, but only Recoverer turns
	// an ordinary one into a 500 the access line can report. ErrAbortHandler
	// is re-panicked, so it keeps net/http's silent abort and the recorder's
	// default status.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(d.Logger),
			webhttp.ProbeLogLevel("/api/health", "/metrics"),
			webhttp.WithRecordRouteMetric(d.Metrics.RecordHTTP),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(d.Logger)),
		webhttp.SecurityHeaders(),
	)

	return webhttp.NewServer(
		handler,
		webhttp.WithReadTimeout(defaultReadTimeout),
		webhttp.WithWriteTimeout(defaultWriteTimeout),
		webhttp.WithReadHeaderTimeout(defaultReadHeaderTimeout),
		webhttp.WithIdleTimeout(defaultIdleTimeout),
		webhttp.WithErrorLog(slog.NewLogLogger(d.Logger.Handler(), slog.LevelError)),
	)
}
