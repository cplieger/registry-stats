package webapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/metrics"
)

// Default HTTP server timeouts. Chosen for a LAN-only Grafana
// Infinity datasource: Grafana issues short bursts of requests, so
// per-request caps of a few seconds are comfortably above P99 and
// below any reverse-proxy timeout. Mirrors the pre-refactor values in
// main.go's startServer.
const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 10 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	defaultMaxHeaderBytes    = 1 << 20
)

// Deps is the injection surface for the webapi package. Concrete
// implementations live elsewhere: Store is *store.FS from
// internal/store, Health is *health.Marker. A nil Logger
// falls back to slog.Default.
type Deps struct {
	Store         api.Store
	Health        api.HealthSignal
	Logger        *slog.Logger
	ListenAddr    string
	EnableJSONAPI bool
	EnableMetrics bool
}

// New constructs the HTTP server, wiring the handlers to the Deps and
// applying access-log middleware. Does not start the server; call
// Start (or ListenAndServe) separately so the composition root can
// emit a log line at the right moment.
func New(d Deps) *http.Server {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	addr := d.ListenAddr
	if addr == "" {
		addr = ":9100"
	}

	h := newHandlers(d.Store, d.Health, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", h.health)
	if d.EnableJSONAPI {
		mux.HandleFunc("GET /api/snapshot", h.snapshot)
		mux.HandleFunc("GET /api/pulls", h.pulls)
		mux.HandleFunc("GET /api/pulls/daily", h.pullsDaily)
		mux.HandleFunc("GET /api/summary", h.summary)
	}
	if d.EnableMetrics {
		mux.HandleFunc("GET /metrics", metrics.Handler())
	}

	return &http.Server{
		Addr:              addr,
		Handler:           WithAccessLog(mux, logger),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
	}
}

// Start launches srv.ListenAndServe in a new goroutine and returns a
// channel that receives the first fatal error (anything other than
// ErrServerClosed). The channel is closed without a send on normal
// shutdown. Callers select on the returned channel to detect bind
// failures and propagate them to the composition root.
func Start(srv *http.Server, logger *slog.Logger) <-chan error {
	if logger == nil {
		logger = slog.Default()
	}
	errCh := make(chan error, 1)
	logger.Info("http server starting", "addr", srv.Addr)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
			errCh <- err
			return
		}
		close(errCh)
	}()
	return errCh
}

// Shutdown marks the service unhealthy (so the orchestrator stops
// routing traffic), then gracefully shuts down srv with a 5-second
// timeout. The timeout is derived from context.Background so it
// survives the outer shutdown ctx already having been cancelled by
// the signal that kicked off the shutdown.
//
// cause is logged alongside the shutdown so Loki can distinguish
// SIGTERM, SIGINT, or an internal trigger. Pass
// context.Cause(parentCtx) when the caller already has the parent
// context in scope.
func Shutdown(srv *http.Server, health api.HealthSignal, cause error, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if health != nil {
		health.Set(false)
	}
	logger.Info("shutting down", "cause", cause)
	sdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sdCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}
}

// StatusRecorder captures the HTTP status code for the access log.
// Defaults to 200 because handlers that never call WriteHeader
// implicitly return 200. Exported so the main package's thin shim
// (which exists to keep the pre-refactor TestStatusRecorder test
// green) can construct instances directly.
type StatusRecorder struct {
	http.ResponseWriter

	Status int
}

// WriteHeader records the status code and forwards to the wrapped
// ResponseWriter. Callers that never invoke WriteHeader leave
// Status at its zero initialization (main's shim defaults to 200,
// which matches net/http's implicit response code).
func (s *StatusRecorder) WriteHeader(code int) {
	s.Status = code
	s.ResponseWriter.WriteHeader(code)
}

// WithAccessLog emits one structured log line per HTTP request, at
// DEBUG for 2xx/3xx (quiet by default; turn on LOG_LEVEL=debug to see
// them), WARN for 4xx, and ERROR for 5xx. This makes dashboard
// failures traceable in Loki without flooding logs for normal
// Grafana polls.
func WithAccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		metrics.HTTPRequests.Inc(r.Method, r.URL.Path, strconv.Itoa(rw.Status))
		metrics.HTTPDuration.Observe(dur.Seconds())
		lvl := slog.LevelDebug
		switch {
		case rw.Status >= 500:
			lvl = slog.LevelError
		case rw.Status >= 400:
			lvl = slog.LevelWarn
		}
		logger.Log(r.Context(), lvl, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rw.Status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}
