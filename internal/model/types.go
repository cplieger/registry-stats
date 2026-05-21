// Package model holds the pure data types that describe a registry-stats
// snapshot. Types here carry no behavior beyond JSON struct tags; the tags
// define the on-disk /data/YYYY-MM-DD.json contract and the HTTP API
// response shapes, so they MUST stay identical across refactors.
package model

import (
	"encoding/json"
	"iter"
	"strings"
	"time"
)

// Snapshot is the root on-disk object written once per collection cycle.
type Snapshot struct {
	Timestamp time.Time   `json:"timestamp"`
	DockerHub []RepoStats `json:"docker_hub,omitempty"`
	GHCR      []GhcrStats `json:"ghcr,omitempty"`
}

// Entries yields a flattened stream of (RegistrySource, SummaryEntry)
// pairs across all registry slices in the snapshot. Zero-download GHCR
// packages are skipped (a zero scrape is treated as "not applicable").
// This centralises the source→slice routing so consumers iterate a
// uniform stream without knowing the per-registry storage layout.
func (s *Snapshot) Entries() iter.Seq2[RegistrySource, SummaryEntry] {
	return func(yield func(RegistrySource, SummaryEntry) bool) {
		for _, dh := range s.DockerHub {
			if !yield(SourceDockerHub, SummaryEntry{
				Registry:  SourceDockerHub.String(),
				Name:      dh.Repo,
				PullCount: dh.PullCount,
				TagCount:  len(dh.Tags),
			}) {
				return
			}
		}
		for _, gh := range s.GHCR {
			if gh.DownloadCount == 0 {
				continue
			}
			if !yield(SourceGHCR, SummaryEntry{
				Registry:  SourceGHCR.String(),
				Name:      gh.Package,
				PullCount: gh.DownloadCount,
			}) {
				return
			}
		}
	}
}

// PullEntries returns PullEntry records for all repos in the snapshot,
// skipping zero-download GHCR packages. The date parameter is stamped
// onto each entry (it comes from the caller's storage key, not the
// snapshot's Timestamp).
func (s *Snapshot) PullEntries(date string) []PullEntry {
	var entries []PullEntry
	for _, dh := range s.DockerHub {
		entries = append(entries, PullEntry{
			Date:      date,
			Source:    SourceDockerHub,
			Repo:      dh.Repo,
			PullCount: dh.PullCount,
		})
	}
	for _, gh := range s.GHCR {
		if gh.DownloadCount == 0 {
			continue
		}
		entries = append(entries, PullEntry{
			Date:      date,
			Source:    SourceGHCR,
			Repo:      gh.Package,
			PullCount: gh.DownloadCount,
		})
	}
	return entries
}

// RepoStats is a Docker Hub repo's pull count plus tag metadata.
type RepoStats struct {
	Repo        string    `json:"repo"`
	LastUpdated string    `json:"last_updated"`
	Tags        []TagInfo `json:"tags"`
	PullCount   int64     `json:"pull_count"`
}

// TagInfo is a single tag as returned by the Docker Hub /tags/ endpoint.
type TagInfo struct {
	Name        string      `json:"name"`
	LastUpdated string      `json:"last_updated"`
	Digest      string      `json:"digest"`
	Images      []ImageInfo `json:"images,omitempty"`
	FullSize    int64       `json:"full_size"`
}

// ImageInfo is a single per-architecture manifest inside a multi-arch tag.
type ImageInfo struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
}

// GhcrStats is a GHCR package's scraped download count.
type GhcrStats struct {
	Package       string `json:"package"`
	DownloadCount int64  `json:"download_count"`
}

// RepoRef is an owner/repo pair parsed from env var input. Repo is "*" for
// wildcard refs that expand at collection time.
type RepoRef struct {
	Owner string
	Repo  string
}

// SummaryEntry carries the per-registry view that handleSummary needs.
// filteredPulls also builds on it (summing across registries for the same name).
type SummaryEntry struct {
	Registry  string
	Name      string
	PullCount int64
	TagCount  int
}

// RepoPull is a repo name + pull count extracted from a snapshot.
type RepoPull struct {
	Repo      string
	PullCount int64
}

// PullEntry is a single (date, source, repo, pullCount) record in the
// pre-computed pull index. Handlers consume these directly instead of
// iterating every snapshot file per request.
type PullEntry struct {
	Date      string
	Repo      string
	PullCount int64
	Source    RegistrySource
}

// RegistryEntry is the registry-agnostic Collect() result used by
// api.RegistrySource implementations. Later steps map it into the
// per-registry on-disk arrays (docker_hub / ghcr). Zero-value fields are
// ignored for the registry that doesn't populate them (Tags/PullCount are
// Docker Hub-only, DownloadCount is GHCR-only).
type RegistryEntry struct {
	Name          string
	LastUpdated   string
	Tags          []TagInfo
	PullCount     int64
	DownloadCount int64
}

// RegistrySource is the typed identity of a container registry that
// registry-stats scrapes. The zero value (SourceUnknown) represents
// "registry not classified"; handlers that build carry-forward maps
// key by RegistrySource + name so a misplaced empty string can never
// silently collide with a real source. The String() method produces
// the lowercase on-wire name used in the JSON summary row's
// `registry` field and in WARN/ERROR log k/v pairs, preserving
// byte-identical output for Grafana dashboards and Loki alerts.
type RegistrySource uint8

// RegistrySource values. SourceUnknown exists only to catch unset /
// defaulted values in filter plumbing; production code paths should
// always hold one of the concrete sources.
const (
	SourceUnknown RegistrySource = iota
	SourceDockerHub
	SourceGHCR
)

// String returns the lowercase on-wire name of a RegistrySource. It
// MUST match the registry= query-parameter vocabulary and the
// SummaryEntry.Registry JSON field (inviolate: HTTP API surface).
// SourceUnknown returns "" so callers that write it into a response
// without checking first surface the mis-classification as an empty
// field rather than a bogus label.
func (r RegistrySource) String() string {
	switch r {
	case SourceDockerHub:
		return "dockerhub"
	case SourceGHCR:
		return "ghcr"
	default:
		return ""
	}
}

// ParseRegistrySource maps the lowercase on-wire name back to its
// typed value. Unknown input returns SourceUnknown; callers decide
// whether that is an error or (for the registry= query param) a
// "include everything" signal.
func ParseRegistrySource(s string) RegistrySource {
	switch s {
	case "dockerhub":
		return SourceDockerHub
	case "ghcr":
		return SourceGHCR
	default:
		return SourceUnknown
	}
}

// MarshalJSON renders a RegistrySource as its lowercase name. Today
// SummaryEntry.Registry stays a plain string on the wire (see the
// handler rows' `registry` field), so this method is defensive
// future-proofing for any struct that may embed RegistrySource
// directly: it keeps the JSON vocabulary consistent.
func (r RegistrySource) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// RegistryFilter is the typed view of the registry= query parameter.
// Zero value means "include every registry" (the Grafana default
// and the behaviour of an unset / $__all / {$__all} / multi-value
// brace input). When Set is true the filter restricts to Only.
//
// The filter is populated via ParseRegistryFilter so the three
// handler call sites share one parse of the raw query string
// (previously each handler re-implemented the brace-stripping
// dance).
type RegistryFilter struct {
	Only RegistrySource
	Set  bool
}

// ParseRegistryFilter turns a registry= query value into a typed
// filter. Accepts the full vocabulary that the pre-refactor
// stripGrafanaBraces + registryIncludes pair handled:
//
//   - "" and "$__all" → include every source (zero value).
//   - "{$__all}" → include every source (Grafana brace-wrapping).
//   - "dockerhub", "ghcr" → restrict to that source.
//   - "{dockerhub}", "{ghcr}" → restrict (brace-wrapped single).
//   - "{a,b}" (comma inside braces) → include every source. Multi-
//     value braces come from Grafana when the template variable is
//     set to multiple values; the pre-refactor registryIncludes
//     treated any unknown stripped value as "include both", and
//     because {a,b} → "a,b" (unknown as a source name) it fell into
//     the "include both" branch. We preserve that semantics.
//   - unknown single values → include every source (same fallback).
func ParseRegistryFilter(raw string) RegistryFilter {
	stripped := strings.TrimPrefix(strings.TrimSuffix(raw, "}"), "{")
	if stripped == "" || stripped == "$__all" {
		return RegistryFilter{}
	}
	if strings.Contains(stripped, ",") {
		// Multi-value Grafana brace expansion falls back to
		// "include every source" to match the pre-refactor
		// registryIncludes default branch.
		return RegistryFilter{}
	}
	src := ParseRegistrySource(stripped)
	if src == SourceUnknown {
		// Unknown single value: preserve the pre-refactor
		// "include both" behaviour.
		return RegistryFilter{}
	}
	return RegistryFilter{Only: src, Set: true}
}

// Includes reports whether the filter allows entries from r. A
// zero-value filter (Set == false) includes every source.
func (f RegistryFilter) Includes(r RegistrySource) bool {
	if !f.Set {
		return true
	}
	return f.Only == r
}
