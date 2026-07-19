package main

// main_test.go is intentionally small: the tests below exercise the
// only behavior main.go owns that isn't already covered by a direct
// internal/* package test.
//
// Every other test this file used to host (handler/filter/writeJSON/
// accessLog/shutdown, httpx retry/drain/redirect, DockerHub + GHCR
// mock scraping, config parse + rapid/PBT coverage, collect
// orchestration) was migrated in cycles 1-2 to its new owning package
// alongside the corresponding main.go shim deletion. The legacy
// on-disk storage tests (Save/Load/Prune/Cache) were not migrated to a
// package -- they were deleted with the store when v2 became stateless.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/api"
	configpkg "github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/metrics"
	"github.com/cplieger/registry-stats/v2/internal/model"
)

// TestLogConfig smoke-tests logConfig on a populated *Config: it emits the
// per-repo and config-summary INFO lines and must not panic. The
// "no repos configured" ERROR branch is covered by TestLogConfig_noReposLogsError.
func TestLogConfig(t *testing.T) {
	cfg := &configpkg.Config{
		DockerHubRepos: []model.RepoRef{{Owner: "a", Repo: "b"}},
		GHCRRepos:      []model.RepoRef{{Owner: "c", Repo: "d"}},
		PollInterval:   time.Hour,
	}
	logConfig(cfg)
}

// mainFakeSource is a canned api.RegistrySource for driving runCollect in
// isolation from any HTTP path.
type mainFakeSource struct {
	src     model.RegistrySource
	entries []model.RegistryEntry
	healthy bool
}

func (f *mainFakeSource) Name() string                 { return f.src.String() }
func (f *mainFakeSource) Source() model.RegistrySource { return f.src }

func (f *mainFakeSource) Collect(_ context.Context, _ []model.RepoRef) ([]model.RegistryEntry, int, bool) {
	return f.entries, len(f.entries), f.healthy
}

// TestRunCollect_partialSuccessStaysHealthy pins the health-marker contract
// runCollect owns and that diverges from collect.Run's verdict: when one
// registry produces data and the other fails, Run reports the cycle degraded
// (healthy=false) but runCollect must still return true so the marker stays
// healthy ("partial failures stay healthy as long as one repo succeeds").
// Mutates process-global metrics, so no t.Parallel.
func TestRunCollect_partialSuccessStaysHealthy(t *testing.T) {
	dh := &mainFakeSource{
		src:     model.SourceDockerHub,
		entries: []model.RegistryEntry{{Owner: "o", Repo: "app", Pulls: 1}},
		healthy: true,
	}
	gh := &mainFakeSource{src: model.SourceGHCR, healthy: false} // no entries, unhealthy
	cfg := &configpkg.Config{
		DockerHubRepos: []model.RepoRef{{Owner: "o", Repo: "app"}},
		GHCRRepos:      []model.RepoRef{{Owner: "o", Repo: "pkg"}},
	}
	if got := runCollect(t.Context(), cfg, []api.RegistrySource{dh, gh}); !got {
		t.Error("runCollect() = false, want true (DockerHub produced data; partial success stays healthy)")
	}
}

// TestRunCollect_allEmptyIsUnhealthy pins the other side of the contract: when
// no registry produces an entry, runCollect returns false so the marker flips
// unhealthy. Mutates process-global metrics, so no t.Parallel.
func TestRunCollect_allEmptyIsUnhealthy(t *testing.T) {
	dh := &mainFakeSource{src: model.SourceDockerHub, healthy: false}
	cfg := &configpkg.Config{DockerHubRepos: []model.RepoRef{{Owner: "o", Repo: "app"}}}
	if got := runCollect(t.Context(), cfg, []api.RegistrySource{dh}); got {
		t.Error("runCollect() = true, want false (no repo collected)")
	}
}

// TestRunCollect_publishesImageMetrics pins the end-to-end metric contract
// through the composition root: with metrics enabled, one runCollect cycle
// flows each source's flat records into the {registry,owner,repo} gauge
// labels on the real /metrics output (the same boundary the collector
// scrapes, and the label set grafana-dashboard.json queries). Mutates
// process-global metrics, so no t.Parallel.
func TestRunCollect_publishesImageMetrics(t *testing.T) {
	dh := &mainFakeSource{
		src:     model.SourceDockerHub,
		entries: []model.RegistryEntry{{Owner: "cplieger", Repo: "subflux", Pulls: 1234, TagCount: 2}},
		healthy: true,
	}
	gh := &mainFakeSource{
		src:     model.SourceGHCR,
		entries: []model.RegistryEntry{{Owner: "cplieger", Repo: "vibekit", Pulls: 56}},
		healthy: true,
	}
	cfg := &configpkg.Config{
		DockerHubRepos: []model.RepoRef{{Owner: "cplieger", Repo: "subflux"}},
		GHCRRepos:      []model.RepoRef{{Owner: "cplieger", Repo: "vibekit"}},
		EnableMetrics:  true,
	}
	if got := runCollect(t.Context(), cfg, []api.RegistrySource{dh, gh}); !got {
		t.Fatal("runCollect() = false, want true")
	}

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	metrics.Handler()(w, r)
	body := w.Body.String()

	want := []string{
		`registrystats_image_pulls_total{owner="cplieger",registry="dockerhub",repo="subflux"} 1234`,
		`registrystats_image_tags{owner="cplieger",registry="dockerhub",repo="subflux"} 2`,
		`registrystats_image_pulls_total{owner="cplieger",registry="ghcr",repo="vibekit"} 56`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("metrics output missing %q\n got:\n%s", line, body)
		}
	}
}

// mainFakeMarker is a minimal api.HealthSignal for asserting the health flag
// recoverAndMarkUnhealthy sets.
type mainFakeMarker struct{ healthy bool }

func (m *mainFakeMarker) Set(h bool)    { m.healthy = h }
func (m *mainFakeMarker) Healthy() bool { return m.healthy }

// TestRecoverAndMarkUnhealthy_onPanicMarksUnhealthyAndLogs pins the
// collect-goroutine panic safety net: a recovered panic must flip the marker
// unhealthy AND emit the "<phase> panicked" ERROR line Loki alerts key on (per
// the function docstring). Swaps slog.Default to capture, so no t.Parallel.
func TestRecoverAndMarkUnhealthy_onPanicMarksUnhealthyAndLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	m := &mainFakeMarker{healthy: true}
	func() {
		defer recoverAndMarkUnhealthy(m, "scheduled collect")
		panic("boom")
	}()

	if m.Healthy() {
		t.Error("recoverAndMarkUnhealthy did not flip marker to unhealthy after a recovered panic")
	}
	if !strings.Contains(buf.String(), "scheduled collect panicked") {
		t.Errorf("missing Loki-alert log line %q; logs:\n%s", "scheduled collect panicked", buf.String())
	}
}

// TestRecoverAndMarkUnhealthy_noPanicLeavesMarker pins the no-op path: with no
// panic in the deferred scope the marker is left untouched.
func TestRecoverAndMarkUnhealthy_noPanicLeavesMarker(t *testing.T) {
	m := &mainFakeMarker{healthy: true}
	func() {
		defer recoverAndMarkUnhealthy(m, "initial collect")
	}()
	if !m.Healthy() {
		t.Error("recoverAndMarkUnhealthy flipped marker with no panic")
	}
}

// TestLogConfig_noReposLogsError drives the no-repos ERROR branch that
// TestLogConfig's comment claims to cover but does not (it passes a populated
// *Config). With zero repos configured, logConfig must emit the operator-facing
// "no repos configured" ERROR that warns the healthcheck will fail after the
// first collect. Swaps slog.Default to capture, so no t.Parallel.
func TestLogConfig_noReposLogsError(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	logConfig(&configpkg.Config{})

	if !strings.Contains(buf.String(), "no repos configured") {
		t.Errorf("logConfig with no repos did not emit the expected ERROR; logs:\n%s", buf.String())
	}
}
