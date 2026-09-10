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

// attrValue is the slog key for a warning's whole environment value.
const attrValue = "value"

// Warning carries a non-fatal configuration message and top-level string or
// int attributes. This package never logs.
type Warning struct {
	// Rewording a Msg silently disarms any alerts/logql.yaml rule matching
	// it: RegistryStatsConfigRejected matches three of the five below.
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
// pairs. Invalid entries are skipped and reported with the rule that refused
// them. Each accepted ref is canonicalized to lower case.
func parseRepoRefs(s string, reg registry.ID) ([]registry.RepoRef, []Warning) {
	var refs []registry.RepoRef
	var warns []Warning
	seen := make(map[registry.RepoRef]bool)
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ref, err := resolveRef(reg, p)
		if err != nil {
			warns = append(warns, Warning{
				Msg: "skipping unusable repo ref",
				Attrs: []slog.Attr{
					slog.String("input", p),
					slog.String("reason", err.Error()),
				},
			})
			continue
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs, warns
}

// resolveRef returns the canonical lower-case ref because Docker Hub 404s an
// upper-case ref, ghcr.io will not register one, and the fold makes deduplication
// case-insensitive. A non-nil error names the refusing rule and is not a sentinel.
func resolveRef(reg registry.ID, token string) (registry.RepoRef, error) {
	owner, repo, ok := strings.Cut(token, "/")
	if !ok || owner == "" || repo == "" {
		return registry.RepoRef{}, errors.New("not owner/repo or owner/*")
	}
	if !urlsafe.IsSafeURLSegment(owner) {
		return registry.RepoRef{}, errors.New("owner not a safe URL segment")
	}
	name, err := repoName(reg, urlsafe.Owner(owner), repo)
	if err != nil {
		return registry.RepoRef{}, err
	}
	return registry.RepoRef{
		Owner: strings.ToLower(owner),
		Repo:  strings.ToLower(name),
	}, nil
}

// repoName accepts a wildcard for either registry, one safe segment for Docker
// Hub, and a decoded safe package name for GHCR.
func repoName(reg registry.ID, owner urlsafe.Owner, repo string) (string, error) {
	if repo == "*" {
		if err := urlsafe.CheckReference(owner, repo); err != nil {
			return "", err
		}
		return repo, nil
	}
	if reg != registry.GHCR {
		if !urlsafe.IsSafeURLSegment(repo) {
			return "", errors.New("repository not a safe URL segment")
		}
		if err := urlsafe.CheckReference(owner, repo); err != nil {
			return "", err
		}
		return repo, nil
	}
	return urlsafe.PackageName(owner, repo)
}
