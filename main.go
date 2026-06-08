package main

// registry-stats collects container image statistics from Docker Hub and GHCR,
// stores daily snapshots as flat JSON files, and serves them via a tiny HTTP API
// for Grafana (Infinity datasource) to query directly.
//
// Docker Hub: pull count, tag count, per-tag metadata via unauthenticated API.
// GHCR: download count scraped from public package pages (no API key needed).
//
// Data is stored as /data/YYYY-MM-DD.json (one file per day, overwritten each poll).
// The HTTP API serves current + historical data for Grafana dashboards.
//
// main.go is a pure composition root: it wires config → *http.Client
// (with httpx.DockerGitHubRedirectPolicy) → store.FS → dockerhub.Client +
// ghcr.Client → health.Marker → webapi.Server, threads those concrete
// values through runCollect / runScheduled / pruneOnce, and handles the
// signal-driven lifecycle. All business logic lives in internal/*; this
// file contains no shims, globals, or type aliases.

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/httpx"
	"github.com/cplieger/registry-stats/internal/api"
	collectpkg "github.com/cplieger/registry-stats/internal/collect"
	configpkg "github.com/cplieger/registry-stats/internal/config"
	"github.com/cplieger/registry-stats/internal/dockerhub"
	"github.com/cplieger/registry-stats/internal/ghcr"
	"github.com/cplieger/registry-stats/internal/metrics"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/store"
	"github.com/cplieger/registry-stats/internal/webapi"
)

func main() {
	// CLI health probe for Docker healthcheck (distroless has no curl/wget).
	// Checks for a marker file instead of making an HTTP request — no port needed.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}

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

	// Construct the shared client directly rather than via httpx.NewClient:
	// the library's NewClient installs DefaultRedirectPolicy (same-host
	// only), but registry-stats must follow Docker Hub / GHCR redirects
	// across the docker.com / github.com / githubusercontent.com family,
	// so we wire httpx.DockerGitHubRedirectPolicy as CheckRedirect.
	httpClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: httpx.DockerGitHubRedirectPolicy,
	}
	var storeOpts []store.Option
	if !cfg.EnableJSONAPI {
		storeOpts = append(storeOpts, store.WithDisableWrite())
	}
	snapStore := store.NewFS(cfg.DataDir, storeOpts...)

	// Sweep any .snapshot-*.tmp files left behind by a previous crashed run.
	// store.FS.Save uses CreateTemp+rename for atomicity, so a SIGKILL
	// between CreateTemp and Rename leaks a temp file; ListDates skips
	// them but they accumulate forever otherwise.
	if err := snapStore.CleanupStaleTmp(ctx); err != nil {
		slog.Warn("cleanup stale tmp failed", "error", err)
	}

	dh := dockerhub.NewClient(httpClient, nil, 0, slog.Default())
	gh := ghcr.NewClient(httpClient, nil, ghcr.Options{}, slog.Default())

	// Pin {dh, gh} order: DockerHub-then-GHCR is the scrape sequence
	// Loki dashboards key on in their per-source panels.
	sources := []api.RegistrySource{dh, gh}

	srv := webapi.New(webapi.Deps{
		Store:         snapStore,
		Health:        marker,
		Logger:        slog.Default(),
		ListenAddr:    cfg.ListenAddr,
		EnableJSONAPI: cfg.EnableJSONAPI,
		EnableMetrics: cfg.EnableMetrics,
	})
	srvErr := webapi.Start(srv, slog.Default())

	// Mark healthy as soon as the HTTP API is listening. The first collect
	// runs in the background so slow registries (GHCR per-package delays,
	// Docker Hub pagination) can't push us past the Docker healthcheck
	// grace window and trigger a restart loop. Subsequent collects in the
	// scheduled loop still flip the marker based on their real outcome.
	marker.Set(true)
	slog.Info("http api ready, starting initial collection in background")

	var initDone sync.WaitGroup
	initDone.Go(func() {
		defer recoverAndMarkUnhealthy(marker, "initial collect")
		marker.Set(runCollect(ctx, &cfg, snapStore, sources))
		pruneOnce(ctx, snapStore, cfg.RetentionDays)
	})

	if cfg.PollInterval == 0 {
		slog.Info("one-shot mode, serving collected data", "addr", cfg.ListenAddr)
		select {
		case <-ctx.Done():
		case err := <-srvErr:
			if err != nil {
				slog.Error("http server died, shutting down", "error", err)
				stop()
			}
		}
	} else {
		slog.Info("scheduled mode", "interval", cfg.PollInterval, "jitter", "±10%")
		runScheduled(ctx, &cfg, snapStore, marker, sources, srvErr, stop)
	}

	// Wait (bounded) for any in-flight initial collect before shutting down
	// the HTTP server. The collect respects ctx cancellation so this
	// normally returns quickly; the 10s cap protects against a wedged
	// scraper holding shutdown past stop_grace_period.
	waitWithTimeout(&initDone, 10*time.Second)

	webapi.Shutdown(srv, marker, context.Cause(ctx), slog.Default())
	httpx.Close(httpClient)
}

// runCollect executes a single collection cycle against the shared
// composition-root dependencies and returns the boolean healthcheck
// signal: true iff collect.Run persisted a non-empty snapshot.
//
// Return semantics (unchanged from the pre-refactor main.go collect):
//
//	true  — snapshot persisted (fully healthy or partial-success with
//	        at least one registry's data).
//	false — nothing saved: empty-snapshot guard fired, all collections
//	        failed, or Save itself errored.
func runCollect(
	ctx context.Context,
	cfg *configpkg.Config,
	fs api.Store,
	sources []api.RegistrySource,
) bool {
	start := time.Now()
	snap, _, err := collectpkg.Run(ctx, collectpkg.Options{
		Sources: sources,
		Store:   fs,
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
	// Record per-source collect metrics.
	metrics.CollectDuration.Observe(time.Since(start).Seconds())
	for _, src := range sources {
		metrics.CollectsTotal.Inc(src.Name())
	}
	if err != nil {
		for _, src := range sources {
			metrics.CollectErrors.Inc(src.Name())
		}
		return false
	}
	if snap == nil {
		return false
	}
	if cfg.EnableMetrics {
		updateImageMetrics(snap, cfg)
	}
	return len(snap.DockerHub) > 0 || len(snap.GHCR) > 0
}

// updateImageMetrics converts a snapshot into ImageMetric slice and
// pushes it to the metrics package for /metrics rendering.
func updateImageMetrics(snap *model.Snapshot, cfg *configpkg.Config) {
	imgs := make([]metrics.ImageMetric, 0, len(snap.DockerHub)+len(snap.GHCR))
	for _, dh := range snap.DockerHub {
		owner := ownerForRepo(cfg.DockerHubRepos, dh.Repo)
		imgs = append(imgs, metrics.ImageMetric{
			Registry: "dockerhub", Owner: owner, Repo: dh.Repo,
			Pulls: dh.PullCount, Tags: len(dh.Tags),
		})
	}
	for _, gh := range snap.GHCR {
		owner := ownerForRepo(cfg.GHCRRepos, gh.Package)
		imgs = append(imgs, metrics.ImageMetric{
			Registry: "ghcr", Owner: owner, Repo: gh.Package,
			Pulls: gh.DownloadCount,
		})
	}
	metrics.SetImageMetrics(imgs)
}

// ownerForRepo finds the owner from the configured refs for a given repo name.
func ownerForRepo(refs []model.RepoRef, repo string) string {
	for _, r := range refs {
		if r.Repo == repo || r.Repo == "*" {
			return r.Owner
		}
	}
	return ""
}

// runScheduled runs collection + retention prune on each tick of a
// PollInterval-sized timer with ±10% jitter, until ctx is cancelled.
// fs, marker, dh, and gh are the composition-root values wired in
// main(); runScheduled does not construct or cache any of them itself.
// srvErr propagates fatal HTTP server errors; stop cancels the parent
// context so the process exits cleanly.
func runScheduled(
	ctx context.Context,
	cfg *configpkg.Config,
	fs api.Store,
	marker api.HealthSignal,
	sources []api.RegistrySource,
	srvErr <-chan error,
	stop context.CancelFunc,
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
		case err := <-srvErr:
			timer.Stop()
			if err != nil {
				slog.Error("http server died, shutting down", "error", err)
				stop()
			}
			return
		case <-timer.C:
			func() {
				defer recoverAndMarkUnhealthy(marker, "scheduled collect")
				marker.Set(runCollect(ctx, cfg, fs, sources))
				pruneOnce(ctx, fs, cfg.RetentionDays)
			}()
		}
	}
}

// pruneOnce wraps store.FS.Prune with the legacy log shape:
// "pruned old snapshots" on success, "failed to list dates for
// pruning" on error. Extracted so both the initial-collect goroutine
// and runScheduled's per-tick block call through a single code path.
func pruneOnce(ctx context.Context, fs api.Store, retentionDays int) {
	n, err := fs.Prune(ctx, retentionDays)
	if err != nil {
		slog.Error("failed to list dates for pruning", "error", err)
		return
	}
	if n > 0 {
		slog.Info("pruned old snapshots", "count", n, "retention_days", retentionDays)
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

// waitWithTimeout waits for wg up to d, then returns. Used to bound
// graceful shutdown so a wedged collect goroutine can't hold the
// HTTP server past stop_grace_period.
func waitWithTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		slog.Warn("initial collect goroutine did not finish within 10s of shutdown")
	}
}

// setupLogging configures slog based on the provided level. Called at the
// top of main before any logging happens. Matches the convention in the other
// Go apps in this repo (plex-exporter, plex-language-sync, subflux).
func setupLogging(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: level})))
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
		"poll_interval", cfg.PollInterval,
		"retention_days", cfg.RetentionDays)
	if len(cfg.DockerHubRepos) == 0 && len(cfg.GHCRRepos) == 0 {
		slog.Error("no repos configured; healthcheck will fail after first collect",
			"hint", "set DOCKERHUB_REPOS and/or GHCR_REPOS")
	}
}
