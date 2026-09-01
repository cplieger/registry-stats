// Package registry holds domain types shared across registry-stats packages.
package registry

// Entry is one collected image. TagCount is nil when no count was fetched this
// cycle or the registry does not publish one; a pointed-to zero is measured.
type Entry struct {
	Owner    string
	Repo     string
	Pulls    int64
	TagCount *int
}

// RepoRef is an owner/repository pair. Repo is "*" for refs expanded at collection time.
type RepoRef struct {
	Owner string
	Repo  string
}

// ID identifies a container registry.
type ID uint8

const (
	Unknown ID = iota
	DockerHub
	GHCR
)

// String returns the lowercase wire name; unknown IDs return an empty string.
func (id ID) String() string {
	switch id {
	case DockerHub:
		return "dockerhub"
	case GHCR:
		return "ghcr"
	default:
		return ""
	}
}
