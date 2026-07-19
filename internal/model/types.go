// Package model holds the pure data types shared across registry-stats
// packages: the flat per-image record RegistrySource implementations emit,
// the owner/repo ref parsed from env config, and the typed registry
// identity. Types here carry no behavior beyond RegistrySource.String;
// nothing in this package is ever serialized (v2 is stateless).
package model

// RegistryEntry is the flat per-image record a RegistrySource emits for
// one collected image: the owner/repo label parts kept separate (they
// feed the {registry,owner,repo} gauge labels directly, without a
// join/split round-trip), the cumulative pull/download count, and the
// repo's total tag count. TagCount 0 means "emit no image_tags series
// this cycle": GHCR never populates it, and Docker Hub leaves it 0 when
// the count fetch fails or the repo genuinely has no tags.
type RegistryEntry struct {
	Owner    string
	Repo     string
	Pulls    int64
	TagCount int
}

// RepoRef is an owner/repo pair parsed from env var input. Repo is "*" for
// wildcard refs that expand at collection time.
type RepoRef struct {
	Owner string
	Repo  string
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
