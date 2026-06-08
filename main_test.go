package main

// main_test.go is intentionally small: the two tests below exercise the
// only behavior main.go owns that isn't already covered by a direct
// internal/* package test.
//
// Every other test this file used to host (handler/filter/writeJSON/
// accessLog/shutdown, storage Save/Load/Prune/Cache, httpx retry/
// drain/redirect, DockerHub + GHCR mock scraping, config parse +
// rapid/PBT coverage, collect orchestration) was migrated in
// cycles 1-2 to its new owning package alongside the corresponding
// main.go shim deletion. See cycle-2 chain reports under
// apps/registry-stats/.refactor/cycles/ for the full migration log.

import (
	"log/slog"
	"testing"
	"time"

	configpkg "github.com/cplieger/registry-stats/internal/config"
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
		RetentionDays:  30,
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
