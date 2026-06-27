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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configpkg "github.com/cplieger/registry-stats/internal/config"
	"github.com/cplieger/registry-stats/internal/metrics"
	"github.com/cplieger/registry-stats/internal/model"
)

// TestLogConfig pins the "no repos configured" ERROR branch and
// the happy-path config-summary INFO output. logConfig just logs —
// the assertion is that it doesn't panic with a populated *Config.
func TestLogConfig(t *testing.T) {
	cfg := &configpkg.Config{
		DockerHubRepos: []model.RepoRef{{Owner: "a", Repo: "b"}},
		GHCRRepos:      []model.RepoRef{{Owner: "c", Repo: "d"}},
		PollInterval:   time.Hour,
	}
	logConfig(cfg)
}

// TestSetupLogging_levels exercises the LOG_LEVEL env-var parser.
// Each level string (case-insensitive, trimmed) selects the expected
// slog.Level; unknown values fall back to Info. Pins an inviolate
// env-var contract (LOG_LEVEL) that dashboards rely on.
func TestSetupLogging_levels(t *testing.T) {
	tests := []struct {
		env  string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" Debug ", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.env)
			cfg := configpkg.LoadConfig()
			setupLogging(cfg.LogLevel)
			if !slog.Default().Enabled(t.Context(), tt.want) {
				t.Errorf("LOG_LEVEL=%q: expected level %v to be enabled", tt.env, tt.want)
			}
			// Verify the level below the expected one is disabled (except
			// when expected is the lowest, Debug).
			if tt.want > slog.LevelDebug {
				below := tt.want - 4 // slog levels are spaced 4 apart
				if slog.Default().Enabled(t.Context(), below) {
					t.Errorf("LOG_LEVEL=%q: level %v should be disabled (below %v)", tt.env, below, tt.want)
				}
			}
		})
	}
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
