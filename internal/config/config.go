// Package config parses registry-stats configuration from environment
// variables. Env var names (RETENTION_DAYS, DOCKERHUB_REPOS, GHCR_REPOS,
// POLL_INTERVAL_HOURS, LOG_LEVEL) are an inviolate contract — the in-memory
// representation here can evolve freely, but the env var names and their
// parsing semantics MUST stay identical to preserve compose-file contracts.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"registry-stats/internal/model"
	"registry-stats/internal/urlsafe"
)

// Default values for env-var-backed fields. Exported for test assertions.
const (
	DefaultListenAddr = ":9100"
	DefaultDataDir    = "/data"
)

// Config is the effective runtime configuration after env var parsing.
type Config struct {
	ListenAddr     string          // TCP listen address (env LISTEN_ADDR)
	DataDir        string          // snapshot storage directory (env DATA_DIR)
	DockerHubRepos []model.RepoRef // Docker Hub repos: "owner/repo" or "owner/*" (wildcard = all public)
	GHCRRepos      []model.RepoRef // GHCR packages: "owner/repo" or "owner/*" (wildcard = all public)
	PollInterval   time.Duration   // time between collections (0 = one-shot, collect once then serve)
	RetentionDays  int             // auto-delete snapshots older than this (0 = keep forever)
	LogLevel       slog.Level      // parsed from LOG_LEVEL env var
	EnableJSONAPI  bool            // serve /api/snapshot, /api/pulls and write JSON files (env ENABLE_JSON_API)
	EnableMetrics  bool            // serve /metrics endpoint (env ENABLE_METRICS)
}

// LoadConfig reads configuration from environment variables with sensible
// defaults. Clamps retention and poll interval to bounded maxima to prevent
// time.Duration overflow and pathological prune-cutoff calculations.
func LoadConfig() Config {
	retentionDays, err := strconv.Atoi(GetEnv("RETENTION_DAYS", "90"))
	if err != nil || retentionDays < 0 {
		retentionDays = 90
	}
	// Clamp to a sensible upper bound. time.Time.AddDate normalizes via
	// time.Date which silently wraps year overflow on huge day counts,
	// producing a Format("2006-01-02") string that sorts below every real
	// snapshot filename — pruning silently becomes a no-op and /data grows
	// forever. 10 years is already absurd for a stats poller.
	const maxRetentionDays = 365 * 10
	if retentionDays > maxRetentionDays {
		slog.Warn("RETENTION_DAYS clamped", "requested", retentionDays, "max", maxRetentionDays)
		retentionDays = maxRetentionDays
	}
	if retentionDays == 0 {
		slog.Warn("RETENTION_DAYS=0 keeps snapshots forever; snapshot cache will grow unbounded over time")
	}
	pollIntervalHours, err := strconv.Atoi(GetEnv("POLL_INTERVAL_HOURS", "1"))
	if err != nil || pollIntervalHours < 0 {
		pollIntervalHours = 1
	}
	// Clamp to a sensible upper bound: multiplying a huge int by time.Hour
	// overflows time.Duration (int64 ns, max ~292 years) into a negative
	// duration, and the jitter calc below would then panic rand.IntN with
	// a negative argument. 1 year is already nonsense for a stats poller.
	const maxPollHours = 24 * 365
	if pollIntervalHours > maxPollHours {
		slog.Warn("POLL_INTERVAL_HOURS clamped", "requested", pollIntervalHours, "max", maxPollHours)
		pollIntervalHours = maxPollHours
	}

	return Config{
		DockerHubRepos: ParseRepoRefs(GetEnv("DOCKERHUB_REPOS", "")),
		GHCRRepos:      ParseRepoRefs(GetEnv("GHCR_REPOS", "")),
		PollInterval:   time.Duration(pollIntervalHours) * time.Hour,
		RetentionDays:  retentionDays,
		ListenAddr:     GetEnv("LISTEN_ADDR", DefaultListenAddr),
		DataDir:        GetEnv("DATA_DIR", DefaultDataDir),
		LogLevel:       parseLogLevel(GetEnv("LOG_LEVEL", "")),
		EnableJSONAPI:  parseBoolEnv(GetEnv("ENABLE_JSON_API", "true")),
		EnableMetrics:  parseBoolEnv(GetEnv("ENABLE_METRICS", "true")),
	}
}

// parseBoolEnv returns true unless s is explicitly "false" or "0".
func parseBoolEnv(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "0":
		return false
	default:
		return true
	}
}

// parseLogLevel converts the LOG_LEVEL env var string to slog.Level.
// Defaults to Info for unrecognized values.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ParseRepoRefs parses a comma-separated list of "owner/repo" or "owner/*"
// pairs. Invalid entries (missing slash, unsafe characters) are skipped
// with a warning.
func ParseRepoRefs(s string) []model.RepoRef {
	if s == "" {
		return nil
	}
	var refs []model.RepoRef
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		owner, repo, ok := strings.Cut(p, "/")
		if !ok || owner == "" || repo == "" {
			slog.Warn("skipping invalid repo ref", "input", p, "expected", "owner/repo or owner/*")
			continue
		}
		if !urlsafe.IsSafeURLSegment(owner) || (repo != "*" && !urlsafe.IsSafeURLSegment(repo)) {
			slog.Warn("skipping repo ref with unsafe characters", "input", p)
			continue
		}
		refs = append(refs, model.RepoRef{Owner: owner, Repo: repo})
	}
	return refs
}

// GetEnv returns os.Getenv(key) when set to a non-empty value, otherwise
// fallback. Distinguishes unset from empty string only in the sense that
// an explicitly-set empty string is treated as unset — sufficient for this
// app's configuration where all values are non-empty or absent.
func GetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
