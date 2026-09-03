// Package registry holds domain types shared across registry-stats packages.
package registry

// Entry is one collected image: an owner, a repository, and the registry's
// cumulative pull count for it.
type Entry struct {
	Owner string
	Repo  string
	Pulls int64
}

// RepoRef is an owner/repository pair. Repo is "*" for refs expanded at collection time.
type RepoRef struct {
	Owner string
	Repo  string
}

// ID identifies a container registry.
type ID uint8

// The registries this exporter polls. Unknown is ID's zero value and names no
// registry; String reports it as the empty string.
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
