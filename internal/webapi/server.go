// Package webapi implements the HTTP API server for registry-stats.
package webapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/webhttp/v2"
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
		mux.HandleFunc("GET /metrics", obs.Handler())
	}

	// Middleware via webhttp.Chain (first listed is outermost): the shared
	// access logger (the Logging role) → Recoverer → SecurityHeaders. Logging
	// stays outermost so a recovered panic is logged as its 500, not the
	// StatusRecorder's default 200. SecurityHeaders applies only the baseline
	// (nosniff, X-Frame-Options: DENY, Referrer-Policy) — no CSP or HSTS,
	// since this is a non-browser metrics/health endpoint.
	//
	// The logger carries one app POLICY and one app SINK, and the line itself
	// (request-id minting/threading, deferred panic-safe emission, hook
	// isolation) is the library's. accessLogLevel is the policy: ~15s
	// Prometheus scrape lines stay at DEBUG while 4xx/5xx are raised.
	// obs.RecordHTTP is the sink: webhttp.WithRecordRouteMetric derives
	// the bounded (method, path) label pair for
	// registrystats_http_requests_total in the LIBRARY and hands it in, so
	// this app has no derivation of its own left to get wrong — the reason to
	// prefer it over WithRecordMetricRequest, which hands over the raw
	// request. See webhttp.RouteMetricLabels for each label's derivation.
	//
	// What that bounds, for this route table: method is r.Method when it is
	// one of the nine standard methods and the fixed "other" bucket
	// otherwise, so the arbitrary token a 405 hands through (ServeMux passes
	// ANY method to a path whose method-bearing pattern did not match) cannot
	// mint a series; path is the matched route TEMPLATE from r.Pattern, or the
	// fixed "unmatched" when nothing matched. Ten methods times one more than
	// the route table is the whole ceiling, and no property of the traffic
	// widens it. The template rather than r.URL.Path matters even on a match:
	// a path-canonicalising redirect (GET /api/./health → /api/health, HTTP
	// 307) keeps a non-empty r.Pattern while r.URL.Path still holds the raw
	// path a scanner can vary without bound (/api//health, /api/x/../health,
	// …).
	//
	// Two consequences worth knowing when reading the series. The method
	// comes from the REQUEST, not from the matched pattern, so a HEAD probe
	// against these GET-only routes records method="HEAD" even though
	// ServeMux routed it to the GET pattern — the metric and the access line
	// for one request_id agree. And only the PATH collapses on an unmatched
	// request: the former app-side derivation collapsed BOTH labels, so every
	// 404 and 405 landed on a single method="unmatched" series, whereas a 404
	// flood is now visible per method at no cardinality cost.
	//
	// The access LOG still records the raw r.URL.Path and the verbatim method
	// on purpose — a line reports what actually arrived, so a non-standard
	// method reads as itself there where the metric reads "other"; correlate
	// by request_id. Neither is UNBOUNDED: webhttp caps the recorded path at
	// 512 bytes (cut on a rune boundary, "...(truncated)" appended) and the
	// recorded method at 24 bytes ("(overlong)" past it). net/http carries a
	// request line up to MaxHeaderBytes (1 MiB here, webhttp's default), so
	// before those caps one unauthenticated request could size a Loki line at
	// will. That half is the library's floor and needs no wiring here.
	handler := webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(logger),
			webhttp.WithLogLevel(accessLogLevel),
			webhttp.WithRecordRouteMetric(obs.RecordHTTP),
		),
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

// accessLogLevel is the access-line LEVEL policy fed to webhttp.WithLogLevel:
// DEBUG for 2xx/3xx (quiet by default; set LOG_LEVEL=debug to see them, so
// ~15s Prometheus scrapes do not flood Loki), WARN for 4xx, and ERROR for
// 5xx. This keeps dashboard failures traceable without drowning them in
// normal-poll noise. Everything else about the line — attributes, request-id
// minting and threading, deferred panic-safe emission — is webhttp.Logging's
// mechanism.
func accessLogLevel(_ *http.Request, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	}
	return slog.LevelDebug
}
