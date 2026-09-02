// Package config parses registry-stats configuration from environment
// variables. The env var names and the meaning of their values are an
// inviolate contract: the in-memory representation here can evolve
// freely, the env surface cannot. LISTEN_ADDR is edge-trimmed, because
// padding otherwise fails the bind with the cause invisible.
//
// This package never logs: every non-fatal parse problem is returned as a
// Warning value for the caller to emit.
package config

import (
	"cmp"
	"errors"
	"log/slog"
	"net/url"
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
}

// attrValue is the slog key the whole-value warnings carry that value
// under. A skipped repo-list entry carries "input" instead: it is one
// token out of the value, not the value.
const attrValue = "value"

// Warning is a non-fatal configuration note for the caller to log. Attrs
// carries structured attributes rather than a pre-rendered sentence, so
// attribute-keyed queries keep working. Attrs holds only top-level string
// or int attributes: no groups, so a caller can bound a string value
// without resolving one. Attribute values are raw environment input, and a
// caller that emits them owns capping and sanitizing them first
// (runesafe.SanitizeSingleLineCapped).
type Warning struct {
	Msg   string
	Attrs []slog.Attr
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
			Attrs: []slog.Attr{slog.String(attrValue, raw)},
		})
		pollIntervalHours = 1
	case !ok:
		pollIntervalHours = 1
	case pollIntervalHours < 0:
		warns = append(warns, Warning{
			Msg:   "invalid POLL_INTERVAL_HOURS, using default of 1 hour",
			Attrs: []slog.Attr{slog.String(attrValue, strconv.Itoa(pollIntervalHours))},
		})
		pollIntervalHours = 1
	}
	// Clamp to a sensible upper bound: time.Duration is int64
	// nanoseconds, so an unbounded hour count wraps. 2562048h yields a
	// negative duration and scheduler.RunLoop returns at once, silently
	// disabling the collect loop; 5124096h wraps back through zero to
	// 25m26s and polls both registries every 25 minutes instead.
	const maxPollHours = 24 * 365
	if pollIntervalHours > maxPollHours {
		warns = append(warns, Warning{
			Msg: "POLL_INTERVAL_HOURS clamped",
			Attrs: []slog.Attr{
				slog.Int("requested", pollIntervalHours),
				slog.Int("max", maxPollHours),
			},
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
			Msg: "invalid LOG_LEVEL, using default",
			Attrs: []slog.Attr{
				slog.String(attrValue, rawLogLevel),
				slog.String("default", "info"),
			},
		})
	}

	rawListenAddr := envx.String("LISTEN_ADDR")
	listenAddr := strings.TrimSpace(rawListenAddr)
	effectiveListenAddr := cmp.Or(listenAddr, defaultListenAddr)
	if listenAddr != rawListenAddr {
		warns = append(warns, Warning{
			Msg: "trimmed whitespace from LISTEN_ADDR",
			Attrs: []slog.Attr{
				slog.String(attrValue, rawListenAddr),
				slog.String("using", effectiveListenAddr),
			},
		})
	}

	dhRepos, dhWarns := parseRepoRefs(envx.String("DOCKERHUB_REPOS"), registry.DockerHub)
	warns = append(warns, dhWarns...)
	ghRepos, ghWarns := parseRepoRefs(envx.String("GHCR_REPOS"), registry.GHCR)
	warns = append(warns, ghWarns...)

	return Config{
		DockerHubRepos: dhRepos,
		GHCRRepos:      ghRepos,
		PollInterval:   pollInterval,
		ListenAddr:     effectiveListenAddr,
		LogLevel:       logLevel,
	}, warns
}

// parseRepoRefs parses a comma-separated list of "owner/repo" or "owner/*"
// pairs. GHCR repository tokens may contain percent-encoded nested names;
// Docker Hub names remain one segment. Invalid entries are skipped and
// reported as warnings. Each accepted ref is canonicalized to lower case,
// because neither registry distinguishes repository case.
func parseRepoRefs(s string, reg registry.ID) ([]registry.RepoRef, []Warning) {
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
				Msg: "skipping invalid repo ref",
				Attrs: []slog.Attr{
					slog.String("input", p),
					slog.String("expected", "owner/repo or owner/*"),
				},
			})
			continue
		}

		repoName, reason := repo, ""
		if !urlsafe.IsSafeURLSegment(owner) {
			reason = "owner not a safe URL segment"
		} else {
			repoName, reason = resolveRepoName(reg, owner, repo)
		}
		if reason != "" {
			warns = append(warns, Warning{
				Msg: "skipping repo ref with unsafe characters",
				Attrs: []slog.Attr{
					slog.String("input", p),
					slog.String("reason", reason),
				},
			})
			continue
		}

		owner = strings.ToLower(owner)
		if repoName != "*" {
			repoName = strings.ToLower(repoName)
		}
		ref := registry.RepoRef{Owner: owner, Repo: repoName}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs, warns
}

// resolveRepoName validates one token's repository half for reg and returns
// the canonical repository name. A non-empty reason names the refusing rule.
func resolveRepoName(reg registry.ID, owner, repo string) (name, reason string) {
	if repo == "*" {
		return repo, ""
	}
	if reg != registry.GHCR {
		if !urlsafe.IsSafeURLSegment(repo) {
			return "", "repository not a safe URL segment"
		}
		return repo, ""
	}
	name, ok := urlsafe.PackageName(owner, repo)
	if ok {
		return name, ""
	}
	return "", ghcrRefusalReason(owner, repo)
}

// ghcrRefusalReason names the rule PackageName refused. It does not decide
// acceptance and is called only after PackageName returns false.
func ghcrRefusalReason(owner, repo string) string {
	name, err := url.PathUnescape(repo)
	switch {
	case strings.Contains(repo, "/"):
		return "raw slash; percent-encode nested names"
	case err != nil:
		return "invalid percent-escape"
	case len(owner)+1+len(name) > urlsafe.MaxSegmentBytes:
		return "repository name over 255 bytes"
	default:
		return "path element not a safe URL segment"
	}
}
