// Package webapi implements the HTTP API server for registry-stats.
package webapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/registry-stats/internal/metrics"
	"github.com/cplieger/webhttp"
)

// Default HTTP server timeouts. Chosen for a LAN-only setup:
// per-request caps of a few seconds are comfortably above P99 and
// below any reverse-proxy timeout. registry-stats is not a streaming
// app, so all four are passed explicitly to webhttp.NewServer, whose
// streaming-safe defaults otherwise leave ReadTimeout/WriteTimeout
// unset. MaxHeaderBytes is left at webhttp's 1 MiB default, which
// matches the previous hand-rolled value.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

// Deps is the injection surface for the webapi package. Ready is the
// serving-readiness view backing GET /api/health — a *webhttp.Ready in
// production, latched true after the first successful collect and cleared
// on shutdown; a nil Ready renders /api/health as 503 unready. A nil Logger
// falls back to slog.Default.
type Deps struct {
	Ready         webhttp.ReadinessChecker
	Logger        *slog.Logger
	EnableMetrics bool
}

// New constructs the HTTP server, wiring the routes and the standard webhttp
// middleware chain over webhttp.NewServer with the app's explicit (non-
// streaming) timeout posture. It neither binds nor starts the server: the
// composition root binds a listener up front (so a port-in-use error surfaces
// synchronously) and drives the lifecycle with webhttp.Run.
func New(d Deps) *http.Server {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ready := d.Ready
	if ready == nil {
		// webhttp.ReadinessHandler calls Ready() unconditionally, so a nil
		// checker would panic on the first request. A zero-value Ready reports
		// not-ready, preserving the 503 a missing readiness view should return.
		ready = &webhttp.Ready{}
	}

	mux := http.NewServeMux()
	// GET /api/health is the HTTP serving-readiness gate, backed by
	// webhttp.ReadinessHandler + the injected Ready view (200 {"status":"ok"}
	// once the first collect has produced data, 503 {"status":"unready"}
	// before then or during shutdown). It is deliberately DISTINCT from the
	// container file-marker liveness probe the `registry-stats health`
	// subcommand checks: same app, different question ("ready to serve?" vs
	// "process alive?"), different mechanism (HTTP vs marker file).
	mux.Handle("GET /api/health", webhttp.ReadinessHandler(ready))
	if d.EnableMetrics {
		mux.HandleFunc("GET /metrics", metrics.Handler())
	}

	// Middleware via webhttp.Chain (first listed is outermost): the app access
	// log (the Logging role) → Recoverer → SecurityHeaders. Logging stays
	// outermost so a recovered panic is logged as its 500, not the
	// StatusRecorder's default 200. SecurityHeaders applies only the baseline
	// (nosniff, X-Frame-Options: DENY, Referrer-Policy) — no CSP or HSTS,
	// since this is a non-browser metrics/health endpoint.
	handler := webhttp.Chain(mux,
		WithAccessLog(logger),
		webhttp.Recoverer(webhttp.WithRecoverLogger(logger)),
		webhttp.SecurityHeaders(),
	)

	return webhttp.NewServer(
		handler,
		webhttp.WithReadTimeout(defaultReadTimeout),
		webhttp.WithWriteTimeout(defaultWriteTimeout),
		webhttp.WithReadHeaderTimeout(defaultReadHeaderTimeout),
		webhttp.WithIdleTimeout(defaultIdleTimeout),
	)
}

// WithAccessLog returns Chain-composable middleware (a webhttp.Middleware) that
// emits one structured log line per HTTP request, at DEBUG for 2xx/3xx (quiet
// by default; set LOG_LEVEL=debug to see them, so ~15s Prometheus scrapes do
// not flood Loki), WARN for 4xx, and ERROR for 5xx. This keeps dashboard
// failures traceable without drowning them in normal-poll noise.
//
// It also records the bounded-cardinality registrystats_http_requests_total
// metric through a webhttp.StatusRecorder. The metric labels key on the
// matched route TEMPLATE (r.Pattern), collapsing any unmatched/probe request
// to method="unmatched",path="unmatched" so a scanner cannot mint unbounded
// series. That r.Pattern-keyed guard is why the access logger stays app-side
// rather than moving onto webhttp.Logging + WithRecordMetric: webhttp's hook
// receives r.URL.Path (and no *http.Request), so it cannot reproduce the
// template-keyed collapse.
func WithAccessLog(logger *slog.Logger) webhttp.Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := webhttp.NewStatusRecorder(w)
			next.ServeHTTP(rw, r)
			dur := time.Since(start)
			// Bound both labels to the matched route. r.Pattern is empty for a
			// 404/405, so an arbitrary client-supplied method/path token
			// collapses to "unmatched". When a route matched, use the pattern's
			// path TEMPLATE rather than r.URL.Path: a path-cleaning redirect
			// (e.g. GET /api/./health -> /api/health, HTTP 301) keeps a non-empty
			// r.Pattern but leaves r.URL.Path holding the raw, uncleaned path,
			// which a scanner can vary without bound (/api//health, /api/./health,
			// /api/x/../health, ...) to mint unbounded series.
			metricPath := "unmatched"
			metricMethod := "unmatched"
			if r.Pattern != "" {
				metricMethod = r.Method
				metricPath = r.Pattern
				if _, p, ok := strings.Cut(r.Pattern, " "); ok {
					metricPath = p
				}
			}
			metrics.RecordHTTP(metricMethod, metricPath, rw.Status(), dur)
			lvl := slog.LevelDebug
			switch {
			case rw.Status() >= 500:
				lvl = slog.LevelError
			case rw.Status() >= 400:
				lvl = slog.LevelWarn
			}
			logger.Log(r.Context(), lvl, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", rw.Status(),
				"duration_ms", dur.Milliseconds())
		})
	}
}
