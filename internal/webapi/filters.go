// Package webapi holds the HTTP API surface for registry-stats: the
// five Grafana-facing handlers (/api/health, /api/snapshot, /api/pulls,
// /api/pulls/daily, /api/summary), the lifecycle wiring (access log,
// graceful shutdown), and the pure filter/query helpers the handlers
// share.
//
// Handlers depend only on the api.Store and api.HealthSignal interfaces.
// The composition root in main.go constructs concrete instances
// (*store.FS, *health.Marker) and passes them via Deps to New. This
// isolates the HTTP surface from persistence and healthcheck concerns
// so each can evolve independently.
//
// Inviolate contract: the JSON response shapes, query parameter names,
// status codes, and defensive headers (X-Content-Type-Options,
// X-Frame-Options, Referrer-Policy, Cache-Control) match the
// pre-refactor main.go surface byte-for-byte. Grafana dashboards and
// Loki alerts pinned on any of those remain compatible.
package webapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cplieger/registry-stats/internal/model"
)

// grafanaAll is Grafana's placeholder for "all values" when a
// template variable is set to "All". Treated as no filter
// (include everything) by parseRepoFilter.
const grafanaAll = "$__all"

// stripGrafanaBraces strips the surrounding `{}` that Grafana adds
// when a multi-value variable is substituted into a URL. Single-value
// substitutions and `$__all` are returned unchanged. Shared by
// parseRepoFilter; the registry= path now parses via
// model.ParseRegistryFilter which does its own brace stripping.
func stripGrafanaBraces(s string) string {
	return strings.TrimPrefix(strings.TrimSuffix(s, "}"), "{")
}

// parseRegistryQuery reads the raw registry= query value and returns
// a typed filter. Accepts the full Grafana vocabulary (empty,
// $__all, {$__all}, {ghcr}, {a,b}, and the bare lowercase names).
// Thin shim so handlers don't need to import model for the
// one-liner.
func parseRegistryQuery(raw string) model.RegistryFilter {
	return model.ParseRegistryFilter(raw)
}

// parseRepoFilter builds a repo filter set from query params. Handles
// single values, comma-separated lists, and repeated params
// (?repo=a&repo=b). Returns nil (no filter) for empty input and for
// Grafana's "$__all" placeholder.
func parseRepoFilter(values []string) map[string]bool {
	var m map[string]bool
	for _, s := range values {
		s = stripGrafanaBraces(s)
		if s == "" || s == grafanaAll {
			return nil
		}
		for p := range strings.SplitSeq(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				if m == nil {
					m = make(map[string]bool)
				}
				m[p] = true
			}
		}
	}
	return m
}

// forEachSummaryEntry walks a snapshot and yields one
// model.SummaryEntry per repo/package that passes the repo + registry
// filters. Zero-download GHCR packages are skipped (a zero scrape is
// treated as "not applicable" to avoid a fake prev=N→curr=0 drop in
// the daily-delta series).
//
// The callback receives the typed model.RegistrySource alongside the
// entry so callers (pullsDaily) can key carry-forward maps by the
// typed source without parsing the string back. The entry's Registry
// field stays as the lowercase on-wire name (src.String()) to
// preserve JSON byte-for-byte output for the summary endpoint.
func forEachSummaryEntry(
	snap *model.Snapshot,
	repoFilter map[string]bool,
	filter model.RegistryFilter,
	fn func(src model.RegistrySource, e model.SummaryEntry),
) {
	for src, e := range snap.Entries() {
		if !filter.Includes(src) {
			continue
		}
		if repoFilter != nil && !repoFilter[e.Name] {
			continue
		}
		fn(src, e)
	}
}

// dateToISO converts a YYYY-MM-DD date string to ISO 8601 format for
// Grafana (which expects timestamps with explicit T and Z markers).
func dateToISO(date string) string {
	return date + "T00:00:00Z"
}

// writeJSON serializes v to w as JSON with the API's standard
// defense-in-depth headers. The API data is already public (Docker
// Hub pull counts, GHCR download counts) and the service is
// LAN-only, but these headers guard against content-type confusion
// and clickjacking if the service is ever reverse-proxied publicly.
//
// Intentionally does not propagate encoding errors: the HTTP
// connection is half-written by the time Encode returns an error,
// so returning one to the caller would invite a second WriteHeader
// call from http.Error. Log + drop matches the pre-refactor
// behaviour.
func writeJSON(w http.ResponseWriter, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("failed to write JSON response", "error", err)
	}
}
