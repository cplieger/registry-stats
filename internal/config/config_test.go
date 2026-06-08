package config

import (
	"log/slog"
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
	t.Setenv("RETENTION_DAYS", "30")
	t.Setenv("LISTEN_ADDR", ":8080")
	t.Setenv("DATA_DIR", "/custom/data")
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
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", cfg.RetentionDays)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
	if cfg.DataDir != "/custom/data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/custom/data")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{"DOCKERHUB_REPOS", "GHCR_REPOS", "POLL_INTERVAL_HOURS", "RETENTION_DAYS", "LISTEN_ADDR", "DATA_DIR", "LOG_LEVEL"} {
		t.Setenv(key, "")
	}
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h", cfg.PollInterval)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90", cfg.RetentionDays)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, DefaultDataDir)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
}

func TestLoadConfigInvalidNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "notanumber")
	t.Setenv("RETENTION_DAYS", "also-bad")
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback", cfg.PollInterval)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90 fallback", cfg.RetentionDays)
	}
}

func TestLoadConfigNegativeNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "-5")
	t.Setenv("RETENTION_DAYS", "-10")
	cfg := LoadConfig()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback for negative", cfg.PollInterval)
	}
	if cfg.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90 fallback for negative", cfg.RetentionDays)
	}
}

func TestLoadConfigWildcard(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "cplieger/*")
	t.Setenv("GHCR_REPOS", "cplieger/*,cplieger/fclones")
	t.Setenv("POLL_INTERVAL_HOURS", "1")
	t.Setenv("RETENTION_DAYS", "90")

	cfg := LoadConfig()

	if len(cfg.DockerHubRepos) != 1 || cfg.DockerHubRepos[0].Repo != "*" {
		t.Errorf("DockerHubRepos = %+v, want [cplieger/*]", cfg.DockerHubRepos)
	}
	if len(cfg.GHCRRepos) != 2 {
		t.Errorf("GHCRRepos len = %d, want 2", len(cfg.GHCRRepos))
	}
}

// Kills CONDITIONALS_BOUNDARY at the clamp sites — verifies that 0 is a
// valid value for RETENTION_DAYS and POLL_INTERVAL_HOURS (not treated as
// negative).
func TestLoadConfigZeroValues(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "0")
	t.Setenv("RETENTION_DAYS", "0")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg := LoadConfig()

	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0 (one-shot mode)", cfg.PollInterval)
	}
	if cfg.RetentionDays != 0 {
		t.Errorf("RetentionDays = %d, want 0 (keep forever)", cfg.RetentionDays)
	}
}

func TestLoadConfig_retention_clamped_to_max(t *testing.T) {
	t.Setenv("RETENTION_DAYS", "99999")
	t.Setenv("POLL_INTERVAL_HOURS", "1")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg := LoadConfig()
	const maxRetentionDays = 365 * 10
	if cfg.RetentionDays != maxRetentionDays {
		t.Errorf("RetentionDays = %d, want %d (clamped)", cfg.RetentionDays, maxRetentionDays)
	}
}

func TestLoadConfig_poll_interval_clamped_to_max(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "99999")
	t.Setenv("RETENTION_DAYS", "90")
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
