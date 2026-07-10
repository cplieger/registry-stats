package main

// main_test.go is intentionally small: the two tests below exercise the
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

// TestSplitOwnerRepo pins the owner/name split that feeds the
// registrystats_image_*{owner,repo} labels (grafana-dashboard.json reads
// them). splitOwnerRepo cuts on the FIRST slash and treats a slashless
// identifier as a bare repo with empty owner.
func TestSplitOwnerRepo(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		wantOwner string
		wantRepo  string
	}{
		{"owner and repo", "cplieger/subflux", "cplieger", "subflux"},
		{"no slash yields empty owner", "alpine", "", "alpine"},
		{"empty string", "", "", ""},
		{"multiple slashes split on first", "a/b/c", "a", "b/c"},
		{"trailing slash empty repo", "owner/", "owner", ""},
		{"leading slash empty owner", "/repo", "", "repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo := splitOwnerRepo(tt.id)
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("splitOwnerRepo(%q) = (%q, %q), want (%q, %q)",
					tt.id, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

// TestUpdateImageMetrics_splitsOwnerRepoLabels pins the observable metric
// contract: updateImageMetrics splits each "owner/name" snapshot identifier
// into separate owner/repo labels and counts DockerHub tags. Asserted through
// the real /metrics output (metrics.Handler), the same boundary Alloy scrapes.
// SetImageMetrics mutates process-global gauges (Reset+Set), so this test must
// not call t.Parallel().
func TestUpdateImageMetrics_splitsOwnerRepoLabels(t *testing.T) {
	snap := &model.Snapshot{
		DockerHub: []model.RepoStats{
			{Repo: "cplieger/subflux", PullCount: 1234, Tags: []model.TagInfo{{Name: "latest"}, {Name: "v1"}}},
		},
		GHCR: []model.GhcrStats{
			{Package: "cplieger/vibekit", DownloadCount: 56},
		},
	}
	updateImageMetrics(snap)

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
		entries: []model.RegistryEntry{{Name: "o/app", PullCount: 1}},
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
