// Package main is the registry-stats composition root.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v5"
	collectpkg "github.com/cplieger/registry-stats/v2/internal/collect"
	"github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
	"github.com/cplieger/registry-stats/v2/internal/ghcr"
	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/webapi"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
	"github.com/cplieger/webhttp/v2"
)

// warnValueBytes bounds raw warning attributes while preserving enough input for diagnosis.
const warnValueBytes = 128

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			// The serving process reports configuration warnings; the frequent probe stays silent.
			interval, _ := config.PollInterval()
			health.RunProbe(health.DefaultPath, health.WithMaxAge(healthMaxAge(interval)))
		default:
			// slogx first: the stdlib default handler emits no level=ERROR field, which
			// is what the shipped log rules match on.
			slogx.Setup(slogx.Options{})
			slog.Error("unknown command", "supported", "health")
			os.Exit(2)
		}
	}

	if err := run(); err != nil {
		slog.Error("registry-stats exited with error", "error", err)
		os.Exit(1)
	}
}

// progressLease tolerates two worst-case gaps between registry requests.
const progressLease = 2 * (requestTimeout + max(
	httpx.RetryAfterCap,
	httpx.DefaultBaseDelay<<(httpx.DefaultMaxAttempts-2),
	ghcr.DefaultMinPacing+ghcr.DefaultPacingJitter,
))

func healthMaxAge(interval time.Duration) time.Duration {
	if interval == 0 {
		return 0
	}
	return interval + progressLease
}

// run wires dependencies and serves until a signal or server error.
func run() error {
	levelVar := slogx.Setup(slogx.Options{})
	marker := health.NewMarker(health.DefaultPath)
	cfg := loadConfig(levelVar)
	logConfig(&cfg)
	m := obs.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A marker inherited from a prior process must not report healthy before this one binds.
	marker.Cleanup()
	defer marker.Cleanup()

	var ready webhttp.Ready
	srv := webapi.New(webapi.Deps{
		Metrics: m,
		Ready:   &ready,
		Logger:  slog.Default(),
	})

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		if webhttp.CausedByCancellation(ctx, err) {
			return nil
		}
		return fmt.Errorf("http server bind: %w", err)
	}
	slog.Info("http server starting", "addr", ln.Addr().String())

	httpClient := registryClient(func() {
		refreshPresentMarker(marker)
	})

	dh := dockerhub.NewClient(httpClient, dockerhub.Options{Logger: slog.Default()})
	gh := ghcr.NewClient(httpClient, ghcr.Options{Logger: slog.Default()})
	active, sourceIDs := activeSources([]collectpkg.SourceRefs{
		{Source: dh, Refs: cfg.DockerHubRepos},
		{Source: gh, Refs: cfg.GHCRRepos},
	})
	m.MintCollectSources(sourceIDs)
	// The marker must exist before collection because request progress only refreshes it.
	marker.Set(true)

	pub := &publication{marker: marker, m: m, ready: &ready}
	collect := func(ctx context.Context) {
		runCollect(ctx, active, pub)
	}

	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		collectLoop(ctx, cfg.PollInterval, collect)
	}()

	preDrain := func(context.Context) {
		slog.Info("shutting down", "cause", context.Cause(ctx))
		pub.drain()
	}

	waitForCollect := func(ctx context.Context) {
		if !webhttp.AwaitDone(ctx, bgDone) {
			slog.Warn("collect loop did not finish before shutdown deadline")
		}
	}
	serveExit := func(shutdownCtx context.Context) {
		stop()
		waitForCollect(shutdownCtx)
	}

	return webhttp.Run(ctx, srv, ln, waitForCollect,
		webhttp.WithPreDrain(preDrain),
		webhttp.WithServeExit(serveExit))
}

func collectLoop(ctx context.Context, interval time.Duration, collect func(context.Context)) {
	if interval == 0 {
		slog.Info("one-shot mode, collecting once then serving")
		collect(ctx)
		return
	}
	slog.Info("scheduled mode", "interval", interval)
	scheduler.RunLoop(ctx, collect, scheduler.LoopOptions{
		Interval:    interval,
		FireOnStart: true,
	})
}

// loadConfig parses configuration and emits its diagnostics before the parsed
// LOG_LEVEL applies, so an error-level setting cannot hide the warning that explains it.
func loadConfig(levelVar *slog.LevelVar) config.Config {
	cfg, warns := config.Load()
	logWarnings(warns)
	levelVar.Set(cfg.LogLevel)
	return cfg
}

func logWarnings(warns []config.Warning) {
	for _, w := range warns {
		attrs := slices.Clone(w.Attrs)
		for i, attr := range attrs {
			if attr.Value.Kind() == slog.KindString {
				text, _ := runesafe.SanitizeSingleLineCapped(attr.Value.String(), warnValueBytes, "...")
				attrs[i] = slog.String(attr.Key, text)
			}
		}
		slog.LogAttrs(context.Background(), slog.LevelWarn, w.Msg, attrs...)
	}
}

// publication serializes cycle publication against shutdown clearing readiness.
// It owns the per-cycle phase of the three operator signals -- the image gauges,
// the liveness marker and the readiness gate -- and is not their only writer:
// run() owns boot liveness, cleared pre-bind and re-armed once the listener is up,
// so the container is healthy before the first cycle finishes, then refreshed before
// every registry request -- that refresh is the mtime the probe's lease reads.
type publication struct {
	marker healthSignal
	m      *obs.Metrics
	ready  *webhttp.Ready
	mu     sync.Mutex
}

func (p *publication) publish(ctx context.Context, images []obs.ImageMetric) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// SetImage deletes labels absent from a pass, so a cancelled cycle publishes nothing.
	if ctx.Err() != nil {
		return
	}
	p.m.SetImage(images)
	ok := len(images) > 0
	p.marker.Set(ok)
	if ok {
		p.ready.Set(true)
	}
}

func (p *publication) drain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready.Set(false)
}

// runCollect executes one cycle and publishes its outcome.
func runCollect(ctx context.Context, sources []collectpkg.SourceRefs, pub *publication) {
	images := collectpkg.Run(ctx, collectpkg.Options{
		Metrics: pub.m,
		Sources: sources,
		Logger:  slog.Default(),
	})
	pub.publish(ctx, images)
}

// requestTimeout bounds each attempt made by the shared outbound
// client, which both registry readers use.
const requestTimeout = 30 * time.Second

func refreshPresentMarker(marker *health.Marker) {
	if marker.CheckHealthy() {
		marker.Set(true)
	}
}

type progressTransport struct {
	next     http.RoundTripper
	progress func()
}

func (t progressTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.progress()
	return t.next.RoundTrip(r)
}

// registryClient is the one outbound client both registry readers share.
// progress runs before every registry request. CheckRedirect is the SSRF
// allowlist: without it net/http follows up to ten redirects to any host,
// on two readers that fetch URLs an upstream page controls.
func registryClient(progress func()) *http.Client {
	return &http.Client{
		Transport:     progressTransport{next: http.DefaultTransport, progress: progress},
		Timeout:       requestTimeout,
		CheckRedirect: httpx.DockerGitHubRedirectPolicy,
	}
}

// activeSources drops the candidates with no configured refs and returns the
// survivors with their metric IDs. One pass keeps invocation and pre-minting on
// the same selected set.
func activeSources(candidates []collectpkg.SourceRefs) (active []collectpkg.SourceRefs, ids []registry.ID) {
	active = make([]collectpkg.SourceRefs, 0, len(candidates))
	ids = make([]registry.ID, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.Refs) == 0 {
			continue
		}
		active = append(active, candidate)
		ids = append(ids, candidate.Source.Source())
	}
	return active, ids
}

// healthSignal is the write-only liveness contract used by the collect loop.
type healthSignal interface {
	Set(healthy bool)
}

func logConfig(cfg *config.Config) {
	slog.Info("configuration loaded",
		"docker_hub_refs", len(cfg.DockerHubRepos),
		"ghcr_refs", len(cfg.GHCRRepos),
		"poll_interval", cfg.PollInterval)
	if len(cfg.DockerHubRepos) == 0 && len(cfg.GHCRRepos) == 0 {
		slog.Error("no repos configured; healthcheck will fail after first collect",
			"hint", "set DOCKERHUB_REPOS and/or GHCR_REPOS")
	}
}
