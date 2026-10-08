// Package registry holds domain types shared across registry-stats packages.
package registry

import "time"

// Entry is one collected image: an owner, a repository, the registry's
// cumulative pull count for it, and the optional times the registry reports.
type Entry struct {
	// LastPushed is GHCR's package-wide "Last published" time. It is zero on
	// Docker Hub and when the package page did not state a readable one.
	LastPushed time.Time
	// Updated is Docker Hub's repository "last_updated" time, which a push and
	// a metadata edit both move. It is zero on GHCR and when Docker Hub sends null.
	Updated time.Time
	Owner   string
	Repo    string
	Pulls   int64
}

// Collection is one source's entries and cycle accounting.
type Collection struct {
	// The entries are what this source MEASURED this cycle, not a claim that
	// the population is complete; a source that truncated its listing says so
	// through its own diagnostics, and the published series for the rows it did
	// not read are absent for that cycle.
	Entries []Entry
	// Absent lists the per-image fetches the registry answered with a
	// definitive not-found.
	Absent []RepoRef
	// Unread lists the images the source knew of, from an explicit ref or a
	// listing row, whose fetch ended in neither an entry nor a not-found.
	Unread []RepoRef
	// Listed names every wildcard owner whose listing was read whole: no lost
	// page, no page cap, and nothing that left the listing unchecked.
	Listed []string
	// Fetched counts per-image metadata fetches that yielded an entry; a row a
	// listing supplied without its own request counts on neither side.
	Fetched int
	// Attempted counts per-image metadata fetches tried, on the same rule.
	Attempted int
	// Definitive counts the attempted fetches that ended in an entry or a
	// not-found, so Definitive < Attempted means some answer is unknown.
	Definitive int
	// ListingFailed reports an owner listing that could not be read as a listing.
	ListingFailed bool
}

// Detail is one image's tag and version counts. Docker Hub reports its tag
// count as Tagged and leaves Untagged zero; GHCR reports its tagged and
// untagged version counts.
type Detail struct {
	Tagged   int64
	Untagged int64
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
