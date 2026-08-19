// Package config parses registry-stats configuration from environment
// variables. Env var names (DOCKERHUB_REPOS, GHCR_REPOS,
// POLL_INTERVAL_HOURS, LOG_LEVEL) are an inviolate contract — the in-memory
// representation here can evolve freely, but the env var names and their
// parsing semantics MUST stay identical to preserve compose-file contracts.
// Reads go through envx (the fleet's env layer); typed values are trimmed
// and a whitespace-only value reads as unset, per envx semantics.
//
// This package never logs: every non-fatal parse problem is returned as a
// Warning value for main to emit through the configured handler
// (go-rulebook C1). A Warning carries its structured attributes, not a
// pre-rendered sentence, so the emitted line keeps the k/v shape a Loki
// query can filter on.
//
// One behavior change rides along, deliberately: the `health` subcommand
// calls PollInterval and DISCARDS its warnings, so a malformed
// POLL_INTERVAL_HOURS no longer produces a line from the probe process. It
// never double-logged within one process (health.RunProbe exits), but the
// probe runs many times an hour against the same bad value, and the serving
// process already reports it once at startup.
package config

import (
	"cmp"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
	"github.com/cplieger/slogx"
)

// Default values for env-var-backed fields. Exported for test assertions.
const (
	DefaultListenAddr = ":9100"
)

// Config is the effective runtime configuration after env var parsing.
type Config struct {
	ListenAddr     string             // TCP listen address (env LISTEN_ADDR)
	DockerHubRepos []registry.RepoRef // Docker Hub repos: "owner/repo" or "owner/*" (wildcard = all public)
	GHCRRepos      []registry.RepoRef // GHCR packages: "owner/repo" or "owner/*" (wildcard = all public)
	PollInterval   time.Duration      // time between collections (0 = one-shot, collect once then serve)
	LogLevel       slog.Level         // parsed from LOG_LEVEL env var
	EnableMetrics  bool               // serve /metrics endpoint (env ENABLE_METRICS)
}

// attrValue is the slog key every "this env value was rejected" warning uses
// for the offending input, so the three sites cannot drift apart and a Loki
// filter has one attribute to key on.
const attrValue = "value"

// Warning is a non-fatal configuration note (an invalid or clamped value)
// for the caller to log. Attrs carries the structured slog key/value pairs
// the message used to be logged with, so relocating the emission to main
// does not flatten them into prose. Warnings never abort startup.
type Warning struct {
	Msg   string
	Attrs []any
}

// PollInterval returns the effective POLL_INTERVAL_HOURS as a duration
// (0 = one-shot), parsed and clamped with the same rules Load applies.
// Exported separately so the health subcommand can derive its probe max-age
// from the same source of truth before config load; that caller discards
// the warnings (Load's caller emits them once).
func PollInterval() (time.Duration, []Warning) {
	var warns []Warning
	pollIntervalHours, ok, err := envx.IntStrict("POLL_INTERVAL_HOURS")
	switch {
	case err != nil:
		raw := ""
		if perr, isParseErr := errors.AsType[*envx.ParseError](err); isParseErr {
			raw = perr.Value
		}
		warns = append(warns, Warning{
			Msg:   "invalid POLL_INTERVAL_HOURS, using default of 1 hour",
			Attrs: []any{attrValue, raw},
		})
		pollIntervalHours = 1
	case !ok:
		pollIntervalHours = 1
	case pollIntervalHours < 0:
		warns = append(warns, Warning{
			Msg:   "invalid POLL_INTERVAL_HOURS, using default of 1 hour",
			Attrs: []any{attrValue, strconv.Itoa(pollIntervalHours)},
		})
		pollIntervalHours = 1
	}
	// Clamp to a sensible upper bound: multiplying a huge int by time.Hour
	// overflows time.Duration (int64 ns, max ~292 years) into a NEGATIVE
	// duration, and scheduler.RunLoop returns immediately on a non-positive
	// Interval — so the overflow would silently disable the collect loop
	// rather than fail loudly, while the container still reported healthy off
	// the boot marker. 1 year is already nonsense for a stats poller.
	const maxPollHours = 24 * 365
	if pollIntervalHours > maxPollHours {
		warns = append(warns, Warning{
			Msg:   "POLL_INTERVAL_HOURS clamped",
			Attrs: []any{"requested", pollIntervalHours, "max", maxPollHours},
		})
		pollIntervalHours = maxPollHours
	}
	return time.Duration(pollIntervalHours) * time.Hour, warns
}

// Load reads configuration from environment variables with sensible
// defaults, returning the typed Config plus the non-fatal warnings for the
// caller to log. Clamps poll interval to a bounded maximum to prevent
// time.Duration overflow.
func Load() (Config, []Warning) {
	pollInterval, warns := PollInterval()

	rawLogLevel := envx.String("LOG_LEVEL")
	logLevel, logLevelOK := slogx.ParseLevel(rawLogLevel, slog.LevelInfo)
	if !logLevelOK {
		warns = append(warns, Warning{
			Msg:   "invalid LOG_LEVEL, using default",
			Attrs: []any{attrValue, rawLogLevel, "default", "info"},
		})
	}

	dhRepos, dhWarns := ParseRepoRefs(envx.String("DOCKERHUB_REPOS"))
	warns = append(warns, dhWarns...)
	ghRepos, ghWarns := ParseRepoRefs(envx.String("GHCR_REPOS"))
	warns = append(warns, ghWarns...)

	return Config{
		DockerHubRepos: dhRepos,
		GHCRRepos:      ghRepos,
		PollInterval:   pollInterval,
		ListenAddr:     cmp.Or(envx.String("LISTEN_ADDR"), DefaultListenAddr),
		LogLevel:       logLevel,
		EnableMetrics:  parseBoolEnv(cmp.Or(envx.String("ENABLE_METRICS"), "true")),
	}, warns
}

// parseBoolEnv returns true unless s is explicitly "false" or "0".
//
// Deliberately NOT envx.Bool: this app's documented contract is
// default-enabled with exactly two disable spellings, so envx's wider
// vocabulary ("no"/"off" false, "yes"/"on" true, warning on anything else)
// would silently flip a value like "no" from enabled to disabled under an
// inviolate compose-file contract.
func parseBoolEnv(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0":
		return false
	default:
		return true
	}
}

// ParseRepoRefs parses a comma-separated list of "owner/repo" or "owner/*"
// pairs. Invalid entries (missing slash, unsafe characters) are skipped,
// each reported as a Warning.
func ParseRepoRefs(s string) ([]registry.RepoRef, []Warning) {
	if s == "" {
		return nil, nil
	}
	var refs []registry.RepoRef
	var warns []Warning
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		owner, repo, ok := strings.Cut(p, "/")
		if !ok || owner == "" || repo == "" {
			warns = append(warns, Warning{
				Msg:   "skipping invalid repo ref",
				Attrs: []any{"input", p, "expected", "owner/repo or owner/*"},
			})
			continue
		}
		if !urlsafe.IsSafeURLSegment(owner) || (repo != "*" && !urlsafe.IsSafeURLSegment(repo)) {
			warns = append(warns, Warning{
				Msg:   "skipping repo ref with unsafe characters",
				Attrs: []any{"input", p},
			})
			continue
		}
		refs = append(refs, registry.RepoRef{Owner: owner, Repo: repo})
	}
	return refs, warns
}
