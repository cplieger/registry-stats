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
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v5"
	collectpkg "github.com/cplieger/registry-stats/v2/internal/collect"
	configpkg "github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
	"github.com/cplieger/registry-stats/v2/internal/ghcr"
	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/webapi"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
	"github.com/cplieger/webhttp/v2"
)

func main() {
	// CLI health probe for Docker healthcheck (distroless has no curl/wget).
	// Checks for a marker file instead of making an HTTP request — no port
	// needed. Polling mode arms a freshness deadline: the collect loop
	// refreshes the marker each cycle, so a marker older than 3 intervals
	// means a wedged loop and a restart fixes it. One-shot mode
	// (POLL_INTERVAL_HOURS=0) disables the deadline (WithMaxAge(0) is a
	// no-op): after the single collect the marker is deliberately static.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// Warnings discarded: the probe runs many times an hour against the
		// same environment, and the serving process already reports a
		// malformed POLL_INTERVAL_HOURS once at startup. RunProbe exits, so
		// nothing downstream needs them.
		interval, _ := configpkg.PollInterval()
		health.RunProbe(health.DefaultPath, health.WithMaxAge(3*interval))
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
	// Install the UTC text handler up front at the DEFAULT level, load config,
	// then emit its warnings BEFORE applying the parsed level. Order is
	// load-bearing: config itself never logs (go-rulebook C1), so main owns
	// the emission — and a valid LOG_LEVEL=error applied first would swallow
	// every config warning, which is exactly the diagnostic an operator who
	// mistyped a value needs. Emitting first reproduces the old behavior,
	// where the warnings were logged inside Load before any level was set.
	levelVar := slogx.Setup(slogx.Options{})
	cfg, warns := configpkg.Load()
	for _, w := range warns {
		slog.Warn(w.Msg, w.Attrs...)
	}
	levelVar.Set(cfg.LogLevel)
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
	defer httpClient.CloseIdleConnections()

	dh := dockerhub.NewClient(httpClient, dockerhub.Options{Logger: slog.Default()})
	gh := ghcr.NewClient(httpClient, ghcr.Options{Logger: slog.Default()})

	// Pin {dh, gh} order: DockerHub-then-GHCR is the scrape sequence
	// Loki dashboards key on in their per-source panels.
	sources := []collectpkg.Source{dh, gh}

	// Pre-mint the per-source collect counters at zero before anything serves,
	// so a windowed alert on collect_errors_total has an earlier sample and can
	// see a source's FIRST failure (see obs.MintCollectSources).
	obs.MintCollectSources(configuredSources(&cfg, sources))

	// Mark liveness healthy as soon as the HTTP API is bound. The collect loop
	// runs in the background so slow registries (GHCR per-package delays, Docker
	// Hub pagination) can't push the first cycle past the Docker healthcheck
	// grace window and trigger a restart loop. Every cycle then flips the marker
	// to its own real outcome, the first one included.
	marker.Set(true)

	// collect is one cycle plus its own panic net. The recover has to live
	// INSIDE the job: scheduler.RunLoop deliberately does not recover, so a
	// recover wrapped around the loop instead would let one panicking cycle
	// take the whole schedule down with it.
	collect := func(ctx context.Context) {
		defer recoverAndMarkUnhealthy(marker)
		markCollect(ctx, &cfg, sources, marker, &ready)
	}

	// bg tracks the single background collect goroutine so the shutdown
	// teardown can wait for it within the grace budget. Both modes run exactly
	// one goroutine: RunLoop is sequential and fires the startup cycle as its
	// own first iteration, so no cycle can ever overlap another.
	var bg sync.WaitGroup
	if cfg.PollInterval == 0 {
		// One-shot mode: this is the ONLY collect. If it collects nothing the
		// marker stays false and /api/health serves 503 until the container is
		// restarted — there is no re-poll to recover on (the README Healthcheck
		// section documents this caveat). webhttp.Run (below) keeps serving the
		// single collected data set until a signal or serve error arrives.
		slog.Info("one-shot mode, collecting once then serving", "addr", ln.Addr().String())
		bg.Go(func() { collect(ctx) })
	} else {
		slog.Info("scheduled mode", "interval", cfg.PollInterval)
		bg.Go(func() {
			// No Jitter: jitter exists to keep MANY instances off a shared
			// upstream's doorstep at the same moment, and this exporter runs one
			// instance per deployment whose phase is already set by whenever its
			// container booted. FireOnStart makes the startup cycle iteration 1.
			scheduler.RunLoop(ctx, collect, scheduler.LoopOptions{
				Interval:    cfg.PollInterval,
				FireOnStart: true,
			})
		})
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
// signal: true iff collect.Run produced a non-empty image set.
//
// Return semantics:
//
//	true  — data collected successfully (fully healthy or partial-
//	        success with at least one registry's data).
//	false — nothing collected: empty-cycle guard fired or all
//	        collections failed.
func runCollect(
	ctx context.Context,
	cfg *configpkg.Config,
	sources []collectpkg.Source,
) bool {
	start := time.Now()
	images, _ := collectpkg.Run(ctx, collectpkg.Options{
		Sources: sources,
		Logger:  slog.Default(),
		Now:     time.Now,
		RefsFor: func(name string) []registry.RepoRef { return refsFor(cfg, name) },
	})
	// Record cycle duration. Per-source collect counters are incremented in
	// collect.collectSource so they count only invoked sources (those with
	// configured refs), keeping collects_total and collect_errors_total over
	// the same denominator.
	obs.CollectDuration.Observe(time.Since(start).Seconds())
	if cfg.EnableMetrics {
		obs.SetImage(images)
	}
	// Health marker = "at least one repo collected this cycle", per the documented contract that
	// partial failures stay healthy as long as one repo succeeds (README/CONTRIBUTING). This is
	// intentionally NOT collect.Run's healthy verdict (!degraded), which is stricter and would flip
	// the marker unhealthy on any partial failure. Run's verdict drives only its partial-failure WARN.
	return len(images) > 0
}

// refsFor resolves the refs configured for a source's on-wire name
// (registry.ID.String()). It is the one answer to "is this source configured":
// collect.Run skips a source with no refs, so a source absent here never
// touches the per-source collect counters.
func refsFor(cfg *configpkg.Config, name string) []registry.RepoRef {
	switch name {
	case registry.DockerHub.String():
		return cfg.DockerHubRepos
	case registry.GHCR.String():
		return cfg.GHCRRepos
	}
	return nil
}

// configuredSources returns the on-wire names of the sources with at least one
// configured ref, which is exactly the set collect.Run invokes and so exactly
// the set whose per-source counters can move. It drives the cold-start pre-mint.
func configuredSources(cfg *configpkg.Config, sources []collectpkg.Source) []string {
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		name := src.Source().String()
		if len(refsFor(cfg, name)) == 0 {
			continue
		}
		names = append(names, name)
	}
	return names
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
	sources []collectpkg.Source,
	marker healthSignal,
	ready *webhttp.Ready,
) {
	ok := runCollect(ctx, cfg, sources)
	marker.Set(ok)
	if ok {
		ready.Set(true)
	}
}

// healthSignal is the write-only file-marker liveness seam this composition
// root drives — declared here at its consumer per the interface-placement
// rule (the old internal/api hub held it). *health.Marker satisfies it. The
// collect loop flips it once per cycle (healthy when a cycle collected at
// least one repo); the `registry-stats health` subcommand reads the marker
// file for the Docker HEALTHCHECK. HTTP serving-readiness is a separate
// concern — GET /api/health is backed by a webhttp.Ready gate, not this
// marker — so this interface is deliberately write-only.
type healthSignal interface {
	Set(healthy bool)
}

// recoverAndMarkUnhealthy is the deferred panic recoverer for the collect
// goroutine. On panic it logs at ERROR (preserving the "collect panicked"
// key+panic value) and flips the health marker so the orchestrator observes
// the failure. It is deferred inside the job rather than around the schedule,
// because scheduler.RunLoop does not recover and a panic that escaped the job
// would end the loop.
func recoverAndMarkUnhealthy(marker healthSignal) {
	if r := recover(); r != nil {
		slog.Error("collect panicked", "panic", r)
		marker.Set(false)
	}
}

// waitWithTimeout blocks until wg's goroutines finish or ctx is done,
// whichever comes first. Used in the shutdown teardown to bound how long
// a wedged collect goroutine can hold the server past the grace deadline.
//
// webhttp.AwaitDone owns the wait, including the recheck the bare two-case
// select got wrong: webhttp.Run derives the teardown context from the SAME
// deadline srv.Shutdown just spent, so a drain that used the whole grace hands
// this function an ALREADY-EXPIRED context, and a select with both cases ready
// picks pseudo-randomly — reporting collect goroutines that DID finish as
// wedged, roughly half the time. AwaitDone re-checks completion after ctx
// fires, so completion wins. The policy stays here: whether to warn, and in
// whose words.
func waitWithTimeout(ctx context.Context, wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	if !webhttp.AwaitDone(ctx, done) {
		slog.Warn("collect goroutines did not finish before shutdown deadline")
	}
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
