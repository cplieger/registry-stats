package config

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/urlsafe"
	"pgregory.net/rapid"
)

func TestParseRepoRefs(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"owner/repo", 1},
		{"a/b,c/d,e/f", 3},
		{" a/b , c/d ", 2},
		{"noslash", 0},
		{"a/b,,c/d", 2},
		{"a/b,bad%owner/repo,c/d", 2},
		{"a/b,owner/bad?repo,c/d", 2},
		{"/norepo", 0},
		{"noowner/", 0},
	}
	for _, tt := range tests {
		got := ParseRepoRefs(tt.input)
		if len(got) != tt.want {
			t.Errorf("ParseRepoRefs(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestParseRepoRefsWildcard(t *testing.T) {
	refs := ParseRepoRefs("owner/repo,owner2/*,owner3/pkg")
	if len(refs) != 3 {
		t.Fatalf("len = %d, want 3", len(refs))
	}
	if refs[1].Owner != "owner2" || refs[1].Repo != "*" {
		t.Errorf("refs[1] = %+v, want owner2/*", refs[1])
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "owner1/app1,owner2/app2")
	t.Setenv("GHCR_REPOS", "gh1/pkg1,gh2/pkg2,gh3/pkg3")
	t.Setenv("POLL_INTERVAL_HOURS", "12")
	t.Setenv("LISTEN_ADDR", ":8080")
	t.Setenv("LOG_LEVEL", "debug")

	cfg := LoadConfig()

	if len(cfg.DockerHubRepos) != 2 {
		t.Errorf("DockerHubRepos len = %d, want 2", len(cfg.DockerHubRepos))
	}
	if cfg.DockerHubRepos[0].Owner != "owner1" || cfg.DockerHubRepos[0].Repo != "app1" {
		t.Errorf("DockerHubRepos[0] = %+v, want owner1/app1", cfg.DockerHubRepos[0])
	}
	if len(cfg.GHCRRepos) != 3 {
		t.Errorf("GHCRRepos len = %d, want 3", len(cfg.GHCRRepos))
	}
	if cfg.PollInterval != 12*time.Hour {
		t.Errorf("PollInterval = %v, want 12h", cfg.PollInterval)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{"DOCKERHUB_REPOS", "GHCR_REPOS", "POLL_INTERVAL_HOURS", "LISTEN_ADDR", "LOG_LEVEL"} {
		t.Setenv(key, "")
	}
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h", cfg.PollInterval)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
}

func TestLoadConfigInvalidNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "notanumber")
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback", cfg.PollInterval)
	}
}

func TestLoadConfigNegativeNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "-5")
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback for negative", cfg.PollInterval)
	}
}

func TestLoadConfigWildcard(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "cplieger/*")
	t.Setenv("GHCR_REPOS", "cplieger/*,cplieger/fclones")
	t.Setenv("POLL_INTERVAL_HOURS", "1")

	cfg := LoadConfig()

	if len(cfg.DockerHubRepos) != 1 || cfg.DockerHubRepos[0].Repo != "*" {
		t.Errorf("DockerHubRepos = %+v, want [cplieger/*]", cfg.DockerHubRepos)
	}
	if len(cfg.GHCRRepos) != 2 {
		t.Errorf("GHCRRepos len = %d, want 2", len(cfg.GHCRRepos))
	}
}

// Kills CONDITIONALS_BOUNDARY at the clamp site — verifies that 0 is a
// valid value for POLL_INTERVAL_HOURS (not treated as negative).
func TestLoadConfigZeroValues(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "0")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg := LoadConfig()

	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0 (one-shot mode)", cfg.PollInterval)
	}
}

func TestLoadConfig_poll_interval_clamped_to_max(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "99999")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg := LoadConfig()
	const maxPollHours = 24 * 365
	if cfg.PollInterval != time.Duration(maxPollHours)*time.Hour {
		t.Errorf("PollInterval = %v, want %v (clamped)", cfg.PollInterval, time.Duration(maxPollHours)*time.Hour)
	}
}

func TestGetEnv(t *testing.T) {
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TEST_GETENV_KEY", "myvalue")
		got := GetEnv("TEST_GETENV_KEY", "fallback")
		if got != "myvalue" {
			t.Errorf("GetEnv() = %q, want %q", got, "myvalue")
		}
	})

	t.Run("returns fallback when not set", func(t *testing.T) {
		got := GetEnv("TEST_GETENV_NONEXISTENT_KEY_12345", "fallback")
		if got != "fallback" {
			t.Errorf("GetEnv() = %q, want %q", got, "fallback")
		}
	})

	t.Run("returns fallback when empty", func(t *testing.T) {
		t.Setenv("TEST_GETENV_EMPTY", "")
		got := GetEnv("TEST_GETENV_EMPTY", "default")
		if got != "default" {
			t.Errorf("GetEnv() = %q, want %q", got, "default")
		}
	})
}

func TestGetEnvDistinguishesUnsetFromEmpty(t *testing.T) {
	// Unset: should return fallback
	got := GetEnv("TOTALLY_NONEXISTENT_VAR_XYZ_123", "default")
	if got != "default" {
		t.Errorf("GetEnv(unset) = %q, want %q", got, "default")
	}

	// Set to non-empty: should return the value
	t.Setenv("TEST_GETENV_SET", "hello")
	got = GetEnv("TEST_GETENV_SET", "default")
	if got != "hello" {
		t.Errorf("GetEnv(set) = %q, want %q", got, "hello")
	}
}

// Property-based tests exercising the full surface of ParseRepoRefs.
func TestParseRepoRefs_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.String().Draw(t, "input")
		refs := ParseRepoRefs(input)
		for _, ref := range refs {
			if ref.Owner == "" {
				t.Errorf("ParseRepoRefs(%q) produced empty owner", input)
			}
			if ref.Repo == "" {
				t.Errorf("ParseRepoRefs(%q) produced empty repo", input)
			}
		}
	})
}

func TestParseRepoRefs_output_always_safe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.String().Draw(t, "input")
		refs := ParseRepoRefs(input)
		for _, ref := range refs {
			if ref.Repo != "*" && !urlsafe.IsSafeURLSegment(ref.Repo) {
				t.Errorf("ParseRepoRefs(%q) produced unsafe repo %q", input, ref.Repo)
			}
			if !urlsafe.IsSafeURLSegment(ref.Owner) {
				t.Errorf("ParseRepoRefs(%q) produced unsafe owner %q", input, ref.Owner)
			}
		}
	})
}

// clampMaxPollHours mirrors the maxPollHours clamp threshold inside
// LoadConfig (24 * 365). Referenced by the clamp-boundary tests below so
// they assert against the exact threshold.
const clampMaxPollHours = 24 * 365

// captureClampLog redirects the global slog default to a buffer-backed
// text handler for the test's duration and returns the buffer. LoadConfig
// emits its clamp warning through the package-global slog, so capturing
// the default logger is the only way to observe whether the warning
// fired. The default is restored in cleanup.
func captureClampLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// TestLoadConfig_clamp_boundary_silent_at_exact_max verifies that a
// POLL_INTERVAL_HOURS value exactly equal to the clamp threshold is
// accepted as-is, with NO clamp warning. At the boundary the clamped and
// unclamped durations are identical, so the only observable difference
// between "clamp" and "don't clamp" is the warning: it must stay silent
// when the input equals the maximum.
func TestLoadConfig_clamp_boundary_silent_at_exact_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(clampMaxPollHours))
	buf := captureClampLog(t)

	cfg := LoadConfig()

	want := time.Duration(clampMaxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("LoadConfig(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v",
			clampMaxPollHours, cfg.PollInterval, want)
	}
	if strings.Contains(buf.String(), "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("LoadConfig at exact max %d emitted a clamp warning, want none at the boundary (log=%q)",
			clampMaxPollHours, buf.String())
	}
}

// TestLoadConfig_clamp_warns_one_above_max confirms the clamp branch (and
// the log-capture mechanism) actually fire one hour above the threshold,
// so the boundary test's "no warning" assertion is a genuine signal
// rather than a silently-broken capture.
func TestLoadConfig_clamp_warns_one_above_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(clampMaxPollHours+1))
	buf := captureClampLog(t)

	cfg := LoadConfig()

	want := time.Duration(clampMaxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("LoadConfig(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v (clamped)",
			clampMaxPollHours+1, cfg.PollInterval, want)
	}
	if !strings.Contains(buf.String(), "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("LoadConfig above max emitted no clamp warning, want one (log=%q)", buf.String())
	}
}
