// Package main is the entry point for registry-stats. It polls Docker Hub and GHCR on a
// schedule, records download counts, and exposes them as Prometheus metrics on port 9100.
//
// main.go is a pure composition root: it wires config → *http.Client
// (with httpx.DockerGitHubRedirectPolicy) → dockerhub.Client +
// ghcr.Client → health.Marker → webapi.Server, threads those concrete
// values through runCollect, and handles the signal-driven lifecycle.
// All business logic lives in internal/*; this file contains no shims,
// globals, or type aliases.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/registry-stats/v2/internal/api"
	collectpkg "github.com/cplieger/registry-stats/v2/internal/collect"
	configpkg "github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
	"github.com/cplieger/registry-stats/v2/internal/ghcr"
	"github.com/cplieger/registry-stats/v2/internal/metrics"
	"github.com/cplieger/registry-stats/v2/internal/model"
	"github.com/cplieger/registry-stats/v2/internal/webapi"
	"github.com/cplieger/webhttp"
)

func main() {
	// CLI health probe for Docker healthcheck (distroless has no curl/wget).
	// Checks for a marker file instead of making an HTTP request — no port needed.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}

	if err := run(); err != nil {
		slog.Error("registry-stats exited with error", "error", err)
		os.Exit(1)
	}
}

// run is the composition root proper: it wires config, the health signals,
// the registry clients, and the HTTP server, then drives the signal-driven
// lifecycle via webhttp.Run. It returns a non-nil error when the HTTP server
// fails to bind or serve — which main turns into a non-zero exit — and nil on
// a clean signal-driven shutdown. Keeping the body here (rather than in main)
// lets deferred cleanup run before the process exits on the error path.
func run() error {
	cfg := configpkg.LoadConfig()
	setupLogging(cfg.LogLevel)
	logConfig(&cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Remove stale health file from a previous run that may have crashed
	// before its defer ran. Without this, the health probe would report
	// healthy before the first collection completes.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	// ready is the HTTP serving-readiness gate behind GET /api/health,
	// deliberately distinct from the file-marker liveness probe above: it
	// latches true only after the first successful collect (so /api/health
	// reports ok once data exists) and is cleared on shutdown. The Docker
	// HEALTHCHECK keeps using the file marker via `registry-stats health`.
	var ready webhttp.Ready

	srv := webapi.New(webapi.Deps{
		Ready:         &ready,
		Logger:        slog.Default(),
		EnableMetrics: cfg.EnableMetrics,
	})

	// Bind the listener up front so a port-in-use error surfaces
	// synchronously here rather than late inside a serve goroutine.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("http server bind on %s: %w", cfg.ListenAddr, err)
	}
	slog.Info("http server starting", "addr", ln.Addr().String())

	// Construct the shared client directly rather than via httpx.NewClient:
	// the library's NewClient installs DefaultRedirectPolicy (same-host
	// only), but registry-stats must follow Docker Hub / GHCR redirects
	// across the docker.com / github.com / githubusercontent.com family,
	// so we wire httpx.DockerGitHubRedirectPolicy as CheckRedirect.
	httpClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: httpx.DockerGitHubRedirectPolicy,
	}
	defer httpx.Close(httpClient)

	dh := dockerhub.NewClient(httpClient, nil, 0, slog.Default())
	gh := ghcr.NewClient(httpClient, nil, ghcr.Options{}, slog.Default())

	// Pin {dh, gh} order: DockerHub-then-GHCR is the scrape sequence
	// Loki dashboards key on in their per-source panels.
	sources := []api.RegistrySource{dh, gh}

	// Mark liveness healthy as soon as the HTTP API is bound. The first
	// collect runs in the background so slow registries (GHCR per-package
	// delays, Docker Hub pagination) can't push us past the Docker
	// healthcheck grace window and trigger a restart loop. Subsequent
	// collects still flip the marker based on their real outcome.
	marker.Set(true)
	slog.Info("http api ready, starting initial collection in background")

	// bg tracks the background collect goroutines (initial + scheduled) so
	// the shutdown teardown can wait for them within the grace budget.
	var bg sync.WaitGroup
	bg.Go(func() {
		defer recoverAndMarkUnhealthy(marker, "initial collect")
		markCollect(ctx, &cfg, sources, marker, &ready)
	})

	if cfg.PollInterval == 0 {
		// One-shot mode: the background initial collect above is the ONLY
		// collect. If it returns an empty snapshot the marker stays false and
		// /api/health serves 503 until the container is restarted -- there is
		// no re-poll to recover on. The README Healthcheck section's "recovers
		// on the next successful poll" is scheduled-mode only; caveat it there
		// too (README.md:152). No scheduled loop is started; webhttp.Run
		// (below) keeps serving the single collected snapshot until a signal or
		// serve error arrives.
		slog.Info("one-shot mode, serving collected data", "addr", ln.Addr().String())
	} else {
		slog.Info("scheduled mode", "interval", cfg.PollInterval, "jitter", "±10%")
		bg.Go(func() { runScheduled(ctx, &cfg, marker, &ready, sources) })
	}

	// onShutdown runs once ctx is cancelled and in-flight requests have
	// drained: flip both signals unhealthy so nothing new is routed, then
	// bound the wait for the background collects. It shares webhttp.Run's
	// shutdown-grace deadline.
	onShutdown := func(shutdownCtx context.Context) {
		slog.Info("shutting down", "cause", context.Cause(ctx))
		ready.Set(false)
		marker.Set(false)
		waitWithTimeout(shutdownCtx, &bg)
	}

	// Run serves in the foreground until ctx is cancelled (SIGINT/SIGTERM)
	// or Serve fails. A serve error is returned as non-nil (which main turns
	// into a non-zero exit); a clean signal-driven shutdown returns nil.
	return webhttp.Run(ctx, srv, ln, onShutdown, webhttp.WithShutdownGrace(10*time.Second))
}

// runCollect executes a single collection cycle against the shared
// composition-root dependencies and returns the boolean healthcheck
// signal: true iff collect.Run produced a non-empty snapshot.
//
// Return semantics:
//
//	true  — snapshot collected successfully (fully healthy or partial-
//	        success with at least one registry's data).
//	false — nothing collected: empty-snapshot guard fired or all
//	        collections failed.
func runCollect(
	ctx context.Context,
	cfg *configpkg.Config,
	sources []api.RegistrySource,
) bool {
	start := time.Now()
	snap, _ := collectpkg.Run(ctx, collectpkg.Options{
		Sources: sources,
		Logger:  slog.Default(),
		Now:     time.Now,
		RefsFor: func(name string) []model.RepoRef {
			switch name {
			case model.SourceDockerHub.String():
				return cfg.DockerHubRepos
			case model.SourceGHCR.String():
				return cfg.GHCRRepos
			}
			return nil
		},
	})
	// Record cycle duration. Per-source collect counters are incremented in
	// collect.collectSource so they count only invoked sources (those with
	// configured refs), keeping collects_total and collect_errors_total over
	// the same denominator.
	metrics.CollectDuration.Observe(time.Since(start).Seconds())
	if cfg.EnableMetrics {
		updateImageMetrics(snap)
	}
	// Health marker = "at least one repo collected this cycle", per the documented contract that
	// partial failures stay healthy as long as one repo succeeds (README/CONTRIBUTING). This is
	// intentionally NOT collect.Run's healthy verdict (!degraded), which is stricter and would flip
	// the marker unhealthy on any partial failure. Run's verdict drives only its partial-failure WARN.
	return len(snap.DockerHub) > 0 || len(snap.GHCR) > 0
}

// updateImageMetrics converts a snapshot into ImageMetric slice and
// pushes it to the metrics package for /metrics rendering. Snapshot
// identifiers are "owner/name"; split each into the separate owner
// and repo labels the metric contract (and grafana-dashboard.json) expect.
func updateImageMetrics(snap *model.Snapshot) {
	imgs := make([]metrics.ImageMetric, 0, len(snap.DockerHub)+len(snap.GHCR))
	for _, dh := range snap.DockerHub {
		owner, repo := splitOwnerRepo(dh.Repo)
		imgs = append(imgs, metrics.ImageMetric{
			Registry: model.SourceDockerHub.String(), Owner: owner, Repo: repo,
			Pulls: dh.PullCount, Tags: len(dh.Tags),
		})
	}
	for _, gh := range snap.GHCR {
		owner, repo := splitOwnerRepo(gh.Package)
		imgs = append(imgs, metrics.ImageMetric{
			Registry: model.SourceGHCR.String(), Owner: owner, Repo: repo,
			Pulls: gh.DownloadCount,
		})
	}
	metrics.SetImageMetrics(imgs)
}

// splitOwnerRepo splits an "owner/name" snapshot identifier into its
// owner and repo-name parts on the first slash -- the inverse of how the
// registry clients build it (ref.Owner + "/" + ref.Repo). An identifier
// without a slash yields an empty owner and the whole string as repo.
func splitOwnerRepo(id string) (owner, repo string) {
	owner, repo, ok := strings.Cut(id, "/")
	if !ok {
		return "", id
	}
	return owner, repo
}

// markCollect runs one collection cycle and reflects the outcome onto the
// two health signals. The liveness marker tracks each cycle's result (a
// cycle collecting at least one repo keeps it healthy; an all-fail cycle
// clears it). The readiness flag only ever latches true — set once the
// first cycle produces data, so GET /api/health advertises serving-
// readiness once data exists — and is cleared solely on shutdown, never by
// a later transient collect failure.
func markCollect(
	ctx context.Context,
	cfg *configpkg.Config,
	sources []api.RegistrySource,
	marker api.HealthSignal,
	ready *webhttp.Ready,
) {
	ok := runCollect(ctx, cfg, sources)
	marker.Set(ok)
	if ok {
		ready.Set(true)
	}
}

// runScheduled runs collection on each tick of a PollInterval-sized
// timer with ±10% jitter, until ctx is cancelled. It runs in its own
// goroutine; a dead HTTP server is handled by webhttp.Run returning in
// the foreground, so the loop only needs to watch ctx.
func runScheduled(
	ctx context.Context,
	cfg *configpkg.Config,
	marker api.HealthSignal,
	ready *webhttp.Ready,
	sources []api.RegistrySource,
) {
	for {
		// Add ±10% jitter to avoid predictable access patterns. Guard
		// against a sub-nanosecond /5 rounding to 0 so rand.IntN never
		// sees a non-positive argument.
		jitterMax := max(1, int(cfg.PollInterval/5))
		jitter := time.Duration(rand.IntN(jitterMax)) //nolint:gosec // G404: scheduling jitter
		delay := cfg.PollInterval - cfg.PollInterval/10 + jitter
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			func() {
				defer recoverAndMarkUnhealthy(marker, "scheduled collect")
				markCollect(ctx, cfg, sources, marker, ready)
			}()
		}
	}
}

// recoverAndMarkUnhealthy is a deferred panic recoverer for the
// collection goroutines. On panic it logs at ERROR (preserving the
// legacy "<phase> panicked" key+panic value used by Loki alerts) and
// flips the health marker so the orchestrator observes the failure.
// phase is the short name of the calling context ("initial collect"
// or "scheduled collect").
func recoverAndMarkUnhealthy(marker api.HealthSignal, phase string) {
	if r := recover(); r != nil {
		slog.Error(phase+" panicked", "panic", r)
		marker.Set(false)
	}
}

// waitWithTimeout blocks until wg's goroutines finish or ctx is done,
// whichever comes first. Used in the shutdown teardown to bound how long
// a wedged collect goroutine can hold the server past the grace deadline.
func waitWithTimeout(ctx context.Context, wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("collect goroutines did not finish before shutdown deadline")
	}
}

// setupLogging configures slog based on the provided level. Called at the
// top of main before any logging happens. Matches the convention in the other
// Go apps in this repo (plex-exporter, plex-language-sync, subflux).
func setupLogging(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: level, ReplaceAttr: utcTimeAttr})))
}

// utcTimeAttr is a slog ReplaceAttr that renders the record's built-in time
// key in UTC, so log-line timestamps are zone-stable regardless of the
// container's TZ (the fleet logs-in-UTC standard). It rewrites only the
// top-level time attribute; a user attribute that happens to share the "time"
// key inside a group is left untouched.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

// logConfig logs the active configuration at startup (no secrets to redact).
func logConfig(cfg *configpkg.Config) {
	for _, r := range cfg.DockerHubRepos {
		slog.Info("docker hub repo", "ref", r.Owner+"/"+r.Repo)
	}
	for _, r := range cfg.GHCRRepos {
		slog.Info("ghcr package", "ref", r.Owner+"/"+r.Repo)
	}
	slog.Info("configuration loaded",
		"docker_hub_refs", len(cfg.DockerHubRepos),
		"ghcr_refs", len(cfg.GHCRRepos),
		"poll_interval", cfg.PollInterval)
	if len(cfg.DockerHubRepos) == 0 && len(cfg.GHCRRepos) == 0 {
		slog.Error("no repos configured; healthcheck will fail after first collect",
			"hint", "set DOCKERHUB_REPOS and/or GHCR_REPOS")
	}
}
