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
			health.RunProbe(health.DefaultPath, health.WithMaxAge(3*interval))
		default:
			// slogx first: the stdlib default handler emits no level=ERROR field, which
			// is what the shipped log rules match on.
			slogx.Setup(slogx.Options{})
			slog.Error("unknown command", "command", os.Args[1], "supported", "health")
			os.Exit(2)
		}
	}

	if err := run(); err != nil {
		slog.Error("registry-stats exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires dependencies and serves until a signal or server error.
func run() error {
	levelVar := slogx.Setup(slogx.Options{})
	cfg, warns := config.Load()
	// Emit before applying LOG_LEVEL so error-level configuration cannot hide its own warning.
	logWarnings(warns)
	levelVar.Set(cfg.LogLevel)
	logConfig(&cfg)
	m := obs.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	marker := health.NewMarker(health.DefaultPath)
	// A marker inherited from a prior process must not report healthy before this one binds.
	marker.Set(false)
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

	// Registry hosts redirect across the Docker and GitHub host families.
	httpClient := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: httpx.DockerGitHubRedirectPolicy,
	}
	defer httpClient.CloseIdleConnections()

	dh := dockerhub.NewClient(httpClient, dockerhub.Options{Logger: slog.Default()})
	gh := ghcr.NewClient(httpClient, ghcr.Options{Logger: slog.Default()})
	sources := []collectpkg.Source{dh, gh}

	m.MintCollectSources(configuredSources(&cfg, sources))
	// Healthy before the first cycle: a first collect over two live
	// registries can outlast the image's 15s HEALTHCHECK start-period, and
	// runCollect's per-cycle Set(ok) cannot cover the window before a
	// cycle has finished.
	marker.Set(true)

	pub := &publication{marker: marker, m: m, ready: &ready}
	collect := func(ctx context.Context) {
		runCollect(ctx, &cfg, sources, pub)
	}

	var bg sync.WaitGroup
	if cfg.PollInterval == 0 {
		slog.Info("one-shot mode, collecting once then serving")
		bg.Go(func() { collect(ctx) })
	} else {
		slog.Info("scheduled mode", "interval", cfg.PollInterval)
		bg.Go(func() {
			scheduler.RunLoop(ctx, collect, scheduler.LoopOptions{
				Interval:    cfg.PollInterval,
				FireOnStart: true,
			})
		})
	}

	preDrain := func(context.Context) {
		slog.Info("shutting down", "cause", context.Cause(ctx))
		pub.drain()
	}

	// Start this waiter before teardown so AwaitDone can observe a completed cycle.
	bgDone := make(chan struct{})
	go func() {
		bg.Wait()
		close(bgDone)
	}()

	waitForCollect := func(ctx context.Context) {
		if !webhttp.AwaitDone(ctx, bgDone) {
			slog.Warn("collect goroutines did not finish before shutdown deadline")
		}
	}
	serveExit := func(shutdownCtx context.Context) {
		stop()
		waitForCollect(shutdownCtx)
	}

	return webhttp.Run(ctx, srv, ln, waitForCollect,
		webhttp.WithShutdownGrace(10*time.Second),
		webhttp.WithPreDrain(preDrain),
		webhttp.WithServeExit(serveExit))
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
func runCollect(ctx context.Context, cfg *config.Config, sources []collectpkg.Source, pub *publication) {
	start := time.Now()
	images := collectpkg.Run(ctx, collectpkg.Options{
		Metrics: pub.m,
		Sources: sources,
		Logger:  slog.Default(),
		RefsFor: func(source registry.ID) []registry.RepoRef { return refsFor(cfg, source) },
	})
	pub.m.ObserveCollectDuration(time.Since(start))
	pub.publish(ctx, images)
}

func refsFor(cfg *config.Config, source registry.ID) []registry.RepoRef {
	switch source {
	case registry.DockerHub:
		return cfg.DockerHubRepos
	case registry.GHCR:
		return cfg.GHCRRepos
	}
	return nil
}

func configuredSources(cfg *config.Config, sources []collectpkg.Source) []string {
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		source := src.Source()
		if len(refsFor(cfg, source)) == 0 {
			continue
		}
		names = append(names, source.String())
	}
	return names
}

// healthSignal is the write-only liveness contract used by the collect loop.
type healthSignal interface {
	Set(healthy bool)
}

func logConfig(cfg *config.Config) {
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
