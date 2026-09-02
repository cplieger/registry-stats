package config

import (
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/registry"
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
		{"owner/*,owner/*", 1},
		{"owner/a,owner/a", 1},
		{"owner/*,owner/a", 2},
	}
	for _, tt := range tests {
		got, _ := parseRepoRefs(tt.input, registry.DockerHub)
		if len(got) != tt.want {
			t.Errorf("parseRepoRefs(%q, DockerHub) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestParseRepoRefs_GHCRNestedNames(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		reg       registry.ID
		want      []registry.RepoRef
		wantWarns int
	}{
		{
			name:  "uppercase_escape",
			input: "owner/helm-charts%2Fgrafana-operator",
			reg:   registry.GHCR,
			want:  []registry.RepoRef{{Owner: "owner", Repo: "helm-charts/grafana-operator"}},
		},
		{
			name:  "lowercase_escape",
			input: "owner/helm-charts%2fgrafana-operator",
			reg:   registry.GHCR,
			want:  []registry.RepoRef{{Owner: "owner", Repo: "helm-charts/grafana-operator"}},
		},
		{name: "docker_hub_encoded_slash", input: "owner/helm-charts%2Fgrafana-operator", reg: registry.DockerHub, wantWarns: 1},
		{name: "raw_nested_slash", input: "owner/helm-charts/grafana-operator", reg: registry.GHCR, wantWarns: 1},
		{name: "dot_segments", input: "owner/..%2f..%2fetc%2fpasswd", reg: registry.GHCR, wantWarns: 1},
		{name: "encoded_dot_segments", input: "owner/%2e%2e%2f%2e%2e", reg: registry.GHCR, wantWarns: 1},
		{name: "single_dot", input: "owner/%2e", reg: registry.GHCR, wantWarns: 1},
		{name: "empty_element", input: "owner/a//b", reg: registry.GHCR, wantWarns: 1},
		{name: "trailing_slash", input: "owner/a%2F", reg: registry.GHCR, wantWarns: 1},
		{name: "nul", input: "owner/bad%00name", reg: registry.GHCR, wantWarns: 1},
		{name: "invalid_escape", input: "owner/%zz", reg: registry.GHCR, wantWarns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warns := parseRepoRefs(tt.input, tt.reg)
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseRepoRefs(%q, %v) refs = %+v, want %+v", tt.input, tt.reg, got, tt.want)
			}
			if len(warns) != tt.wantWarns {
				t.Errorf("parseRepoRefs(%q, %v) returned %d warnings, want %d: %+v", tt.input, tt.reg, len(warns), tt.wantWarns, warns)
			}
		})
	}
}

func TestParseRepoRefs_RefusalReasons(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "owner", input: "owner$/repo", want: "owner not a safe URL segment"},
		{name: "raw slash", input: "owner/app/versions", want: "raw slash; percent-encode nested names"},
		{name: "escape", input: "owner/%zz", want: "invalid percent-escape"},
		{name: "length", input: "owner/" + strings.Repeat("a", 256), want: "repository name over 255 bytes"},
		{name: "charset", input: "owner/Repo$", want: "path element not a safe URL segment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, warns := parseRepoRefs(tt.input, registry.GHCR)
			if len(warns) != 1 {
				t.Fatalf("parseRepoRefs(%q, GHCR) warnings = %+v, want one", tt.input, warns)
			}
			want := slog.String("reason", tt.want)
			if len(warns[0].Attrs) != 2 || !warns[0].Attrs[1].Equal(want) {
				t.Errorf("parseRepoRefs(%q, GHCR) reason = %+v, want %v", tt.input, warns[0].Attrs, want)
			}
		})
	}
}

func TestParseRepoRefs_CanonicalizesOwner(t *testing.T) {
	tests := []struct {
		input string
		want  []registry.RepoRef
	}{
		{"Owner/*", []registry.RepoRef{{Owner: "owner", Repo: "*"}}},
		{"OWNER/Repo", []registry.RepoRef{{Owner: "owner", Repo: "repo"}}},
		{"Mixed/repo,mixed/repo", []registry.RepoRef{{Owner: "mixed", Repo: "repo"}}},
	}
	for _, tt := range tests {
		got, _ := parseRepoRefs(tt.input, registry.DockerHub)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseRepoRefs(%q, DockerHub) = %+v, want %+v", tt.input, got, tt.want)
		}
	}
}

func TestParseRepoRefs_CanonicalizesWholeRef(t *testing.T) {
	for _, reg := range []registry.ID{registry.DockerHub, registry.GHCR} {
		got, warns := parseRepoRefs("OWNER/Registry-Stats,owner/registry-stats", reg)
		want := []registry.RepoRef{{Owner: "owner", Repo: "registry-stats"}}
		if !slices.Equal(got, want) || len(warns) != 0 {
			t.Errorf("parseRepoRefs(mixed case, %v) = (%+v, %+v), want (%+v, no warnings)", reg, got, warns, want)
		}
	}

	got, warns := parseRepoRefs("cplieger/*,cplieger/Registry-Stats", registry.GHCR)
	want := []registry.RepoRef{{Owner: "cplieger", Repo: "*"}, {Owner: "cplieger", Repo: "registry-stats"}}
	if !slices.Equal(got, want) || len(warns) != 0 {
		t.Errorf("parseRepoRefs(wildcard and explicit, GHCR) = (%+v, %+v), want (%+v, no warnings)", got, warns, want)
	}
}

func TestParseRepoRefs_preservesFirstOccurrenceOrder(t *testing.T) {
	refs, _ := parseRepoRefs("z/last,a/first,z/last,owner2/*,m/middle,a/first", registry.DockerHub)
	want := []registry.RepoRef{
		{Owner: "z", Repo: "last"},
		{Owner: "a", Repo: "first"},
		{Owner: "owner2", Repo: "*"},
		{Owner: "m", Repo: "middle"},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("parseRepoRefs(%q, DockerHub) refs = %+v, want %+v", "z/last,a/first,z/last,owner2/*,m/middle,a/first", refs, want)
	}
}

func TestParseRepoRefs_acceptsOnlyExactWildcard(t *testing.T) {
	for name, input := range map[string]string{
		"star_prefix": "owner/*x",
		"star_suffix": "owner/x*",
		"double_star": "owner/**",
	} {
		t.Run(name, func(t *testing.T) {
			refs, _ := parseRepoRefs(input, registry.DockerHub)
			if len(refs) != 0 {
				t.Errorf("parseRepoRefs(%q, DockerHub) = %+v, want no refs", input, refs)
			}
		})
	}

	refs, warns := parseRepoRefs("owner/*", registry.DockerHub)
	if len(warns) != 0 {
		t.Fatalf("parseRepoRefs(owner/*, DockerHub) warnings = %v, want none", warns)
	}
	if len(refs) != 1 {
		t.Fatalf("parseRepoRefs(owner/*, DockerHub) refs = %+v, want one ref", refs)
	}
	if refs[0].Owner != "owner" || refs[0].Repo != "*" {
		t.Errorf("parseRepoRefs(owner/*, DockerHub) ref = %+v, want owner/*", refs[0])
	}
}

func TestParseRepoRefs_warnsForEveryRejectedTokenOnce(t *testing.T) {
	input := "good/one,bad, ,owner/bad?repo,good/one,also-bad,ok/two,owner/bad%repo"
	refs, warns := parseRepoRefs(input, registry.GHCR)
	wantRefs := []registry.RepoRef{
		{Owner: "good", Repo: "one"},
		{Owner: "ok", Repo: "two"},
	}
	if !slices.Equal(refs, wantRefs) {
		t.Errorf("parseRepoRefs(%q, GHCR) refs = %+v, want %+v", input, refs, wantRefs)
	}

	wantWarns := []Warning{
		{
			Msg: "skipping invalid repo ref",
			Attrs: []slog.Attr{
				slog.String("input", "bad"),
				slog.String("expected", "owner/repo or owner/*"),
			},
		},
		{
			Msg: "skipping repo ref with unsafe characters",
			Attrs: []slog.Attr{
				slog.String("input", "owner/bad?repo"),
				slog.String("reason", "path element not a safe URL segment"),
			},
		},
		{
			Msg: "skipping invalid repo ref",
			Attrs: []slog.Attr{
				slog.String("input", "also-bad"),
				slog.String("expected", "owner/repo or owner/*"),
			},
		},
		{
			Msg: "skipping repo ref with unsafe characters",
			Attrs: []slog.Attr{
				slog.String("input", "owner/bad%repo"),
				slog.String("reason", "invalid percent-escape"),
			},
		},
	}
	if !slices.EqualFunc(warns, wantWarns, warningEqual) {
		t.Errorf("parseRepoRefs(%q, GHCR) warnings = %+v, want %+v", input, warns, wantWarns)
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
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
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

// TestLoadZeroValues: POLL_INTERVAL_HOURS=0 is a valid value (one-shot
// mode), not coerced to the positive fallback.
func TestLoadZeroValues(t *testing.T) {
	t.Setenv("POLL_INTERVAL_HOURS", "0")
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")

	cfg, _ := Load()

	if cfg.PollInterval != 0 {
		t.Errorf("PollInterval = %v, want 0 (one-shot mode)", cfg.PollInterval)
	}
}

func TestParseRepoRefs_output_always_safe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.String().Draw(t, "input")
		for _, reg := range []registry.ID{registry.DockerHub, registry.GHCR} {
			refs, _ := parseRepoRefs(input, reg)
			for _, ref := range refs {
				if reg == registry.GHCR && ref.Repo != "*" {
					name, ok := urlsafe.PackageName(ref.Owner, url.PathEscape(ref.Repo))
					if !ok || name != ref.Repo {
						t.Fatalf("parseRepoRefs(%q, GHCR) produced unsafe repo %q", input, ref.Repo)
					}
				} else if ref.Repo != "*" && !urlsafe.IsSafeURLSegment(ref.Repo) {
					t.Fatalf("parseRepoRefs(%q, DockerHub) produced unsafe repo %q", input, ref.Repo)
				}
				if !urlsafe.IsSafeURLSegment(ref.Owner) {
					t.Fatalf("parseRepoRefs(%q, %v) produced unsafe owner %q", input, reg, ref.Owner)
				}
			}
		}
	})
}

// clampMaxPollHours mirrors Load's clamp threshold (24 * 365).
const clampMaxPollHours = 24 * 365

// warningsContain reports whether any returned Warning's message contains
// substr.
func warningsContain(warns []Warning, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w.Msg, substr) {
			return true
		}
	}
	return false
}

func warningEqual(a, b Warning) bool {
	return a.Msg == b.Msg && slices.EqualFunc(a.Attrs, b.Attrs, slog.Attr.Equal)
}

// TestWarningsCarryStructuredAttrs pins the k/v shape main emits: flattening
// these into prose would break any structured log query or alert keyed on the
// attribute rather than the message text.
func TestWarningsCarryStructuredAttrs(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("POLL_INTERVAL_HOURS", "notanumber")
	t.Setenv("LOG_LEVEL", "bogus")

	_, warns := Load()

	want := map[string][]slog.Attr{
		"invalid POLL_INTERVAL_HOURS, using default of 1 hour": {slog.String("value", "notanumber")},
		"invalid LOG_LEVEL, using default": {
			slog.String("value", "bogus"),
			slog.String("default", "info"),
		},
	}
	got := make(map[string][]slog.Attr, len(warns))
	for _, w := range warns {
		got[w.Msg] = w.Attrs
	}
	for msg, attrs := range want {
		gotAttrs, ok := got[msg]
		if !ok {
			t.Errorf("no warning with message %q (got %v)", msg, got)
			continue
		}
		if !slices.EqualFunc(gotAttrs, attrs, slog.Attr.Equal) {
			t.Errorf("warning %q attrs = %v, want %v", msg, gotAttrs, attrs)
		}
	}

	for _, key := range []string{"DOCKERHUB_REPOS", "GHCR_REPOS", "LOG_LEVEL", "LISTEN_ADDR"} {
		t.Setenv(key, "")
	}
	t.Setenv("POLL_INTERVAL_HOURS", "-5")
	_, warns = Load()
	wantNegative := []Warning{{
		Msg:   "invalid POLL_INTERVAL_HOURS, using default of 1 hour",
		Attrs: []slog.Attr{slog.String("value", "-5")},
	}}
	if !slices.EqualFunc(warns, wantNegative, warningEqual) {
		t.Errorf("Load(POLL_INTERVAL_HOURS=-5) warnings = %+v, want %+v", warns, wantNegative)
	}
}

// TestLoad_clamp_boundary_silent_at_exact_max: a POLL_INTERVAL_HOURS value
// exactly equal to the clamp threshold is accepted as-is, with no clamp
// warning — the only observable difference between clamping and not is
// the warning.
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

// TestLoad_clamp_warns_one_above_max confirms the clamp branch fires one
// hour above the threshold.
func TestLoad_clamp_warns_one_above_max(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("POLL_INTERVAL_HOURS", strconv.Itoa(clampMaxPollHours+1))

	cfg, warns := Load()

	want := time.Duration(clampMaxPollHours) * time.Hour
	if cfg.PollInterval != want {
		t.Errorf("Load(POLL_INTERVAL_HOURS=%d).PollInterval = %v, want %v (clamped)",
			clampMaxPollHours+1, cfg.PollInterval, want)
	}
	wantWarns := []Warning{{
		Msg: "POLL_INTERVAL_HOURS clamped",
		Attrs: []slog.Attr{
			slog.Int("requested", clampMaxPollHours+1),
			slog.Int("max", clampMaxPollHours),
		},
	}}
	if !slices.EqualFunc(warns, wantWarns, warningEqual) {
		t.Errorf("Load above max warnings = %+v, want %+v", warns, wantWarns)
	}
}

// TestLoad_ListenAddr_trims verifies edge whitespace comes off LISTEN_ADDR
// before the bind and that the normalization is reported rather than silent.
func TestLoad_ListenAddr_trims(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		want      string
		wantWarns []Warning
	}{
		{
			name: "leading_space",
			env:  " :9100",
			want: ":9100",
			wantWarns: []Warning{{
				Msg: "trimmed whitespace from LISTEN_ADDR",
				Attrs: []slog.Attr{
					slog.String("value", " :9100"),
					slog.String("using", ":9100"),
				},
			}},
		},
		{
			name: "whitespace_only",
			env:  "   ",
			want: defaultListenAddr,
			wantWarns: []Warning{{
				Msg: "trimmed whitespace from LISTEN_ADDR",
				Attrs: []slog.Attr{
					slog.String("value", "   "),
					slog.String("using", defaultListenAddr),
				},
			}},
		},
		{name: "already_clean", env: ":9100", want: ":9100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"DOCKERHUB_REPOS", "GHCR_REPOS", "POLL_INTERVAL_HOURS", "LOG_LEVEL"} {
				t.Setenv(key, "")
			}
			t.Setenv("LISTEN_ADDR", tt.env)

			cfg, warns := Load()

			if cfg.ListenAddr != tt.want {
				t.Errorf("Load(LISTEN_ADDR=%q).ListenAddr = %q, want %q", tt.env, cfg.ListenAddr, tt.want)
			}
			if !slices.EqualFunc(warns, tt.wantWarns, warningEqual) {
				t.Errorf("Load(LISTEN_ADDR=%q) warnings = %+v, want %+v", tt.env, warns, tt.wantWarns)
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

// TestLoad_validLogLevel_silent: the warn fires only on invalid input.
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
