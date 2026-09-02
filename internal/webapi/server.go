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

// Deps supplies the HTTP server dependencies. Metrics and Logger are required. A nil Ready reports 503.
type Deps struct {
	Metrics *obs.Metrics
	Ready   webhttp.ReadinessChecker
	Logger  *slog.Logger
}

// New constructs the server without binding or starting it.
func New(d Deps) *http.Server {
	ready := d.Ready
	if ready == nil {
		// ReadinessHandler calls Ready unconditionally.
		ready = &webhttp.Ready{}
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/health", webhttp.ReadinessHandler(ready))
	mux.HandleFunc("GET /metrics", d.Metrics.Handler())

	// Logging is outermost so recovered panics are recorded as 500 responses.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(d.Logger),
			webhttp.WithLogLevel(accessLogLevel),
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

// accessLogLevel keeps routine scrape traffic at Debug while retaining failed requests.
func accessLogLevel(_ *http.Request, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	}
	return slog.LevelDebug
}
