package config

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gk_registry_stats_u2_maxPollHours mirrors the local maxPollHours const
// inside LoadConfig (24 * 365). Kept here so the boundary test references
// the exact clamp threshold.
const gk_registry_stats_u2_maxPollHours = 24 * 365

// gk_registry_stats_u2_captureDefaultLog swaps the global slog default
// for a buffer-backed text handler for the test's duration and returns
// the buffer. LoadConfig emits the clamp warning through the package
// global slog, so capturing the default logger is the only way to
// observe it. The default is restored in cleanup.
func gk_registry_stats_u2_captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// TestLoadConfig_clamp_boundary_silent_at_exact_max kills the
// CONDITIONALS_BOUNDARY mutant at config.go:47 (`pollIntervalHours >
// maxPollHours` -> `>=`). At the exact boundary the clamped value (max)
// equals the input, so PollInterval is identical for both operators; the
// ONLY observable difference is the clamp warning. With `>`, the input
// equals the max so the branch is NOT taken and no warning is emitted;
// with `>=` the branch IS taken and the warning fires.
func TestLoadConfig_clamp_boundary_silent_at_exact_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(gk_registry_stats_u2_maxPollHours))
	buf := gk_registry_stats_u2_captureDefaultLog(t)

	cfg := LoadConfig()

	want := time.Duration(gk_registry_stats_u2_maxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("LoadConfig(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v",
			gk_registry_stats_u2_maxPollHours, cfg.PollInterval, want)
	}
	if strings.Contains(buf.String(), "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("LoadConfig at exact max %d emitted a clamp warning, want none at boundary (log=%q)",
			gk_registry_stats_u2_maxPollHours, buf.String())
	}
}

// TestLoadConfig_clamp_warns_one_above_max confirms the clamp branch (and
// the log-capture mechanism) actually fire one hour above the boundary,
// so the boundary test's "no warning" assertion is a genuine signal
// rather than a silently-broken capture.
func TestLoadConfig_clamp_warns_one_above_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(gk_registry_stats_u2_maxPollHours+1))
	buf := gk_registry_stats_u2_captureDefaultLog(t)

	cfg := LoadConfig()

	want := time.Duration(gk_registry_stats_u2_maxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("LoadConfig(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v (clamped)",
			gk_registry_stats_u2_maxPollHours+1, cfg.PollInterval, want)
	}
	if !strings.Contains(buf.String(), "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("LoadConfig above max emitted no clamp warning, want one (log=%q)", buf.String())
	}
}
