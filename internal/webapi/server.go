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

	// Logging is outermost, so the recovery below decides the logged status.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(d.Logger),
			webhttp.ProbeLogLevel("/api/health", "/metrics"),
			webhttp.WithRecordRouteMetric(d.Metrics.RecordHTTP),
		),
		// Recoverer buys legibility, not survival: net/http's (*conn).serve
		// recovers every handler panic but suppresses ErrAbortHandler's entire panic
		// log line (go1.27.0, src/net/http/server.go), so without it the process
		// still lives - the connection drops and the access line above reports the
		// status recorder's default 200 instead of a truthful 500.
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
