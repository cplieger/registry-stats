// Package model holds the pure data types that describe a registry-stats
// snapshot. Types here carry no behavior beyond JSON struct tags. The
// TagInfo/ImageInfo tags map the Docker Hub /tags/ API response (parsed via
// dockerhub.ParseTagPage) and must stay identical to that upstream shape. The
// Snapshot/RepoStats/GhcrStats tags are legacy on-disk-store keys retained
// only by the model round-trip tests; v2 is stateless and never marshals
// these types in production.
package model

import "time"

// Snapshot is the root object assembled once per collection cycle.
type Snapshot struct {
	Timestamp time.Time   `json:"timestamp"`
	DockerHub []RepoStats `json:"docker_hub,omitempty"`
	GHCR      []GhcrStats `json:"ghcr,omitempty"`
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

// RegistryEntry is the registry-agnostic Collect() result used by
// api.RegistrySource implementations. Later steps map it into the
// per-registry snapshot slices (Snapshot.DockerHub / Snapshot.GHCR). Zero-value fields are
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
// registry-stats scrapes.
type RegistrySource uint8

// RegistrySource values.
const (
	SourceUnknown RegistrySource = iota
	SourceDockerHub
	SourceGHCR
)

// String returns the lowercase on-wire name of a RegistrySource.
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
