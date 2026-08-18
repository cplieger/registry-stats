// Package registry holds the pure domain types shared across registry-stats
// packages: the flat per-image Entry a source emits, the owner/repo RepoRef
// parsed from env config, and the typed registry ID. Types here carry no
// behavior beyond ID.String; nothing in this package is ever serialized
// (v2 is stateless). Named for the domain it describes — container
// registries — not "model".
package registry

// Entry is the flat per-image record a source emits for
// one collected image: the owner/repo label parts kept separate (they
// feed the {registry,owner,repo} gauge labels directly, without a
// join/split round-trip), the cumulative pull/download count, and the
// repo's total tag count. TagCount 0 means "emit no image_tags series
// this cycle": GHCR never populates it, and Docker Hub leaves it 0 when
// the count fetch fails or the repo genuinely has no tags.
type Entry struct {
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

// ID is the typed identity of a container registry that registry-stats
// scrapes.
type ID uint8

// ID values.
const (
	Unknown ID = iota
	DockerHub
	GHCR
)

// String returns the lowercase on-wire name of a registry ID.
func (r ID) String() string {
	switch r {
	case DockerHub:
		return "dockerhub"
	case GHCR:
		return "ghcr"
	default:
		return ""
	}
}
