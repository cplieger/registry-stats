package config

import (
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
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
		got, _ := ParseRepoRefs(tt.input)
		if len(got) != tt.want {
			t.Errorf("ParseRepoRefs(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestParseRepoRefsWildcard(t *testing.T) {
	refs, _ := ParseRepoRefs("owner/repo,owner2/*,owner3/pkg")
	if len(refs) != 3 {
		t.Fatalf("len = %d, want 3", len(refs))
	}
	if refs[1].Owner != "owner2" || refs[1].Repo != "*" {
		t.Errorf("refs[1] = %+v, want owner2/*", refs[1])
	}
}

func TestLoad(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "owner1/app1,owner2/app2")
	t.Setenv("GHCR_REPOS", "gh1/pkg1,gh2/pkg2,gh3/pkg3")
	t.Setenv("POLL_INTERVAL_HOURS", "12")
	t.Setenv("LISTEN_ADDR", ":8080")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, _ := Load()

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

func TestLoadDefaults(t *testing.T) {
	for _, key := range []string{"DOCKERHUB_REPOS", "GHCR_REPOS", "POLL_INTERVAL_HOURS", "LISTEN_ADDR", "LOG_LEVEL"} {
		t.Setenv(key, "")
	}
	cfg, _ := Load()

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

func TestLoadInvalidNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "notanumber")
	cfg, _ := Load()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback", cfg.PollInterval)
	}
}

func TestLoadNegativeNumbers(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "-5")
	cfg, _ := Load()

	if cfg.PollInterval != 1*time.Hour {
		t.Errorf("PollInterval = %v, want 1h fallback for negative", cfg.PollInterval)
	}
}

func TestLoadWildcard(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "cplieger/*")
	t.Setenv("GHCR_REPOS", "cplieger/*,cplieger/fclones")
	t.Setenv("POLL_INTERVAL_HOURS", "1")

	cfg, _ := Load()

	if len(cfg.DockerHubRepos) != 1 || cfg.DockerHubRepos[0].Repo != "*" {
		t.Errorf("DockerHubRepos = %+v, want [cplieger/*]", cfg.DockerHubRepos)
	}
	if len(cfg.GHCRRepos) != 2 {
		t.Errorf("GHCRRepos len = %d, want 2", len(cfg.GHCRRepos))
	}
}

// TestLoadZeroValues verifies that POLL_INTERVAL_HOURS=0 is a valid
// value (one-shot mode), not coerced to the positive fallback.
func TestLoadZeroValues(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "0")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg, _ := Load()

	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0 (one-shot mode)", cfg.PollInterval)
	}
}

func TestLoad_poll_interval_clamped_to_max(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "99999")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg, _ := Load()
	const maxPollHours = 24 * 365
	if cfg.PollInterval != time.Duration(maxPollHours)*time.Hour {
		t.Errorf("PollInterval = %v, want %v (clamped)", cfg.PollInterval, time.Duration(maxPollHours)*time.Hour)
	}
}

// Property-based tests exercising the full surface of ParseRepoRefs.
func TestParseRepoRefs_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.String().Draw(t, "input")
		refs, _ := ParseRepoRefs(input)
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
		refs, _ := ParseRepoRefs(input)
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
// Load (24 * 365). Referenced by the clamp-boundary tests below so
// they assert against the exact threshold.
const clampMaxPollHours = 24 * 365

// warningsContain reports whether any returned Warning's message contains
// substr. Config never logs (go-rulebook C1 relocation): warnings are values,
// so the tests read them directly instead of capturing the global logger.
func warningsContain(warns []Warning, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w.Msg, substr) {
			return true
		}
	}
	return false
}

// TestWarningsCarryStructuredAttrs pins the k/v shape main emits: the
// pre-relocation code logged these as slog attributes, so flattening them
// into prose would break any Loki filter or alert keyed on the attribute
// rather than the message text.
func TestWarningsCarryStructuredAttrs(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "not-a-ref")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", "notanumber")
	t.Setenv("LOG_LEVEL", "bogus")

	_, warns := Load()

	want := map[string][]any{
		"invalid POLL_INTERVAL_HOURS, using default of 1 hour": {"value", "notanumber"},
		"invalid LOG_LEVEL, using default":                     {"value", "bogus", "default", "info"},
		"skipping invalid repo ref":                            {"input", "not-a-ref", "expected", "owner/repo or owner/*"},
	}
	got := make(map[string][]any, len(warns))
	for _, w := range warns {
		got[w.Msg] = w.Attrs
	}
	for msg, attrs := range want {
		gotAttrs, ok := got[msg]
		if !ok {
			t.Errorf("no warning with message %q (got %v)", msg, got)
			continue
		}
		if !reflect.DeepEqual(gotAttrs, attrs) {
			t.Errorf("warning %q attrs = %v, want %v", msg, gotAttrs, attrs)
		}
	}
}

// TestLoad_clamp_boundary_silent_at_exact_max verifies that a
// POLL_INTERVAL_HOURS value exactly equal to the clamp threshold is
// accepted as-is, with NO clamp warning. At the boundary the clamped and
// unclamped durations are identical, so the only observable difference
// between "clamp" and "don't clamp" is the warning: it must stay silent
// when the input equals the maximum.
func TestLoad_clamp_boundary_silent_at_exact_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(clampMaxPollHours))

	cfg, warns := Load()

	want := time.Duration(clampMaxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("Load(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v",
			clampMaxPollHours, cfg.PollInterval, want)
	}
	if warningsContain(warns, "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("Load at exact max %d returned a clamp warning, want none at the boundary (warns=%v)",
			clampMaxPollHours, warns)
	}
}

// TestLoad_clamp_warns_one_above_max confirms the clamp branch (and
// the log-capture mechanism) actually fire one hour above the threshold,
// so the boundary test's "no warning" assertion is a genuine signal
// rather than a silently-broken capture.
func TestLoad_clamp_warns_one_above_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(clampMaxPollHours+1))

	cfg, warns := Load()

	want := time.Duration(clampMaxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("Load(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v (clamped)",
			clampMaxPollHours+1, cfg.PollInterval, want)
	}
	if !warningsContain(warns, "POLL_INTERVAL_HOURS clamped") {
		t.Errorf("Load above max returned no clamp warning, want one (warns=%v)", warns)
	}
}

func TestLoad_EnableMetrics(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"yes", true},
		{"", true},
		{"false", false},
		{"FALSE", false},
		{" false ", false},
		{"0", false},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("ENABLE_METRICS", tt.env)
			cfg, _ := Load()
			if cfg.EnableMetrics != tt.want {
				t.Errorf("Load(ENABLE_METRICS=%q).EnableMetrics = %v, want %v", tt.env, cfg.EnableMetrics, tt.want)
			}
		})
	}
}

func TestLoad_LogLevel(t *testing.T) {
	tests := []struct {
		env  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{" error ", slog.LevelError},
		{"", slog.LevelInfo},
		{"bogus", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.env)
			cfg, _ := Load()
			if cfg.LogLevel != tt.want {
				t.Errorf("Load(LOG_LEVEL=%q).LogLevel = %v, want %v", tt.env, cfg.LogLevel, tt.want)
			}
		})
	}
}

// TestLoad_invalidLogLevel_warns verifies an unrecognized LOG_LEVEL is
// surfaced with a warning (not silently swallowed) while still falling back to
// Info — matching the app's own warn-and-default handling of a malformed
// POLL_INTERVAL_HOURS. Reuses captureClampLog to observe the warning.
func TestLoad_invalidLogLevel_warns(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("LOG_LEVEL", "bogus")

	cfg, warns := Load()

	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("Load(LOG_LEVEL=bogus).LogLevel = %v, want Info (fallback)", cfg.LogLevel)
	}
	if !warningsContain(warns, "invalid LOG_LEVEL") {
		t.Errorf("invalid LOG_LEVEL returned no warning, want one (warns=%v)", warns)
	}
}

// TestLoad_validLogLevel_silent confirms the warn fires only on invalid
// input: a valid LOG_LEVEL parses without emitting the invalid-level warning.
func TestLoad_validLogLevel_silent(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, warns := Load()

	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("Load(LOG_LEVEL=warn).LogLevel = %v, want Warn", cfg.LogLevel)
	}
	if warningsContain(warns, "invalid LOG_LEVEL") {
		t.Errorf("valid LOG_LEVEL returned an invalid-level warning, want none (warns=%v)", warns)
	}
}
