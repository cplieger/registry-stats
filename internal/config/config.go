// Package config parses registry-stats configuration from environment
// variables. The env var names (DOCKERHUB_REPOS, GHCR_REPOS,
// POLL_INTERVAL_HOURS, LOG_LEVEL, LISTEN_ADDR, ENABLE_METRICS) and the
// meaning of their values are an inviolate contract: the in-memory
// representation here can evolve freely, the env surface cannot.
// LISTEN_ADDR is edge-trimmed, because padding otherwise fails the bind
// with the cause invisible.
//
// This package never logs: every non-fatal parse problem is returned as a
// Warning value for the caller to emit.
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

// defaultListenAddr is the default TCP listen address (env LISTEN_ADDR).
const (
	defaultListenAddr = ":9100"
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

// attrValue is the slog key the whole-value warnings carry that value under
// (POLL_INTERVAL_HOURS twice, LOG_LEVEL, LISTEN_ADDR), so those sites cannot
// drift apart. A skipped repo-list entry carries "input" instead: it is one
// token out of the value, not the value.
const attrValue = "value"

// Warning is a non-fatal configuration note (an invalid or clamped value)
// for the caller to log. Attrs carries slog key/value pairs rather than a
// pre-rendered sentence, so attribute-keyed queries keep working. Warnings
// never abort startup.
type Warning struct {
	Msg   string
	Attrs []any
}

// PollInterval returns the effective POLL_INTERVAL_HOURS as a duration
// (0 = one-shot), clamped to one year, plus the non-fatal warnings its
// parse produced. Exported so the health subcommand can derive its probe
// max-age without loading the rest of the configuration.
func PollInterval() (time.Duration, []Warning) {
	var warns []Warning
	pollIntervalHours, ok, err := envx.IntStrict("POLL_INTERVAL_HOURS")
	switch {
	case err != nil:
		var raw string
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
	// Clamp to a sensible upper bound: time.Duration is int64 nanoseconds
	// (~292 years), so an unbounded hour count wraps. 2562048 hours yields a
	// negative duration and scheduler.RunLoop returns at once on a
	// non-positive Interval, so the collect loop would never run while the
	// container still reported healthy off the boot marker; 5124096 hours
	// wraps back through zero to 25m26s. 1 year is already nonsense for a
	// stats poller.
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
// caller to log.
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

	// LISTEN_ADDR trims where the other envx.String reads do not: net.Listen
	// resolves the padding in " :9100" as a hostname, so the bind fails with
	// its cause invisible ("lookup  : no such host").
	rawListenAddr := envx.String("LISTEN_ADDR")
	listenAddr := strings.TrimSpace(rawListenAddr)
	if listenAddr != rawListenAddr {
		warns = append(warns, Warning{
			Msg:   "trimmed whitespace from LISTEN_ADDR",
			Attrs: []any{attrValue, rawListenAddr, "using", cmp.Or(listenAddr, defaultListenAddr)},
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
		ListenAddr:     cmp.Or(listenAddr, defaultListenAddr),
		LogLevel:       logLevel,
		EnableMetrics:  parseBoolEnv(envx.String("ENABLE_METRICS")),
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
// each reported as a Warning. Exact duplicates are dropped, keeping the
// first occurrence and input order: a repeated ref costs a repeated owner
// listing in every registry and contributes no entry.
func ParseRepoRefs(s string) ([]registry.RepoRef, []Warning) {
	if s == "" {
		return nil, nil
	}
	var refs []registry.RepoRef
	var warns []Warning
	seen := make(map[registry.RepoRef]bool)
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
		ref := registry.RepoRef{Owner: owner, Repo: repo}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs, warns
}
