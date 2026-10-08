package collect

import (
	"cmp"
	"slices"
	"strings"

	"github.com/cplieger/registry-stats/internal/obs"
	"github.com/cplieger/registry-stats/internal/registry"
)

// MaxDetailedNames caps the owner/repo names that get push-time, presence,
// tag and version series, so those families stay bounded however many
// images a wildcard expands to. Names are taken in byte order of
// "owner/repo", and a name's images on both registries are in or out
// together, so a cross-registry comparison never sees half a pair.
const MaxDetailedNames = 250

// sourceResult is one invoked source's collection and the refs it was given.
type sourceResult struct {
	refs       []registry.RepoRef
	images     []obs.ImageMetric
	collection registry.Collection
	source     registry.ID
	healthy    bool
	answered   bool
}

// account turns the cycle's per-source results into the published cycle:
// it marks the images inside the detail cap, computes each source's
// completeness and omitted count, and the presence of every detailed name.
func account(results []sourceResult) Cycle {
	detailed := detailedNames(results)
	var cycle Cycle
	for i := range results {
		r := &results[i]
		omitted := 0
		for _, image := range r.images {
			image.Detailed = detailed[registry.RepoRef{Owner: image.Owner, Repo: image.Repo}]
			if !image.Detailed {
				omitted++
			}
			cycle.Images = append(cycle.Images, image)
		}
		cycle.Sources = append(cycle.Sources, obs.SourceCycle{
			Source:   r.source,
			Answered: r.answered,
			Complete: complete(&r.collection, r.refs),
			Omitted:  omitted,
		})
		cycle.Presence = append(cycle.Presence, presence(r, detailed)...)
	}
	return cycle
}

// detailedNames returns the names inside MaxDetailedNames: every name any
// source measured this cycle plus every explicit ref, in byte order.
func detailedNames(results []sourceResult) map[registry.RepoRef]bool {
	seen := make(map[registry.RepoRef]bool)
	for i := range results {
		r := &results[i]
		for _, e := range r.collection.Entries {
			seen[registry.RepoRef{Owner: e.Owner, Repo: e.Repo}] = true
		}
		for _, ref := range r.refs {
			if ref.Repo != "*" {
				seen[ref] = true
			}
		}
	}
	names := make([]registry.RepoRef, 0, len(seen))
	for ref := range seen {
		names = append(names, ref)
	}
	slices.SortFunc(names, compareNames)
	detailed := make(map[registry.RepoRef]bool, min(len(names), MaxDetailedNames))
	for _, ref := range names[:min(len(names), MaxDetailedNames)] {
		detailed[ref] = true
	}
	return detailed
}

// compareNames orders refs by the bytes of "owner/repo", the order
// MaxDetailedNames documents.
func compareNames(a, b registry.RepoRef) int {
	return cmp.Compare(a.Owner+"/"+a.Repo, b.Owner+"/"+b.Repo)
}

// complete reports a source cycle in which every image fetch ended in an
// entry or a not-found and every wildcard owner's listing was read whole.
func complete(c *registry.Collection, refs []registry.RepoRef) bool {
	if c.ListingFailed || c.Definitive != c.Attempted {
		return false
	}
	for _, ref := range refs {
		if ref.Repo == "*" && !slices.Contains(c.Listed, ref.Owner) {
			return false
		}
	}
	return true
}

// presence decides, for each detailed name the source's configuration
// covers, whether the cycle read it (true) or definitively found it absent
// (false). An explicit ref is decided by its own fetch; a name covered only
// by a wildcard is absent when that owner's listing was read whole and held
// no row for it. Everything else has no presence series: the configuration
// does not cover it, Docker Hub cannot hold it, or the read of it was not
// definitive.
func presence(r *sourceResult, detailed map[registry.RepoRef]bool) []obs.Presence {
	read := make(map[registry.RepoRef]bool, len(r.collection.Entries))
	for _, e := range r.collection.Entries {
		read[registry.RepoRef{Owner: e.Owner, Repo: e.Repo}] = true
	}
	absent := toSet(r.collection.Absent)
	unread := toSet(r.collection.Unread)
	explicit := make(map[registry.RepoRef]bool)
	wildcard := make(map[string]bool)
	for _, ref := range r.refs {
		if ref.Repo == "*" {
			wildcard[ref.Owner] = true
		} else {
			explicit[ref] = true
		}
	}

	names := make([]registry.RepoRef, 0, len(detailed))
	for ref := range detailed {
		names = append(names, ref)
	}
	slices.SortFunc(names, compareNames)
	out := make([]obs.Presence, 0, len(names))
	for _, ref := range names {
		if r.source == registry.DockerHub && strings.Contains(ref.Repo, "/") {
			// A Docker Hub repository name is one path segment, so a nested GHCR
			// package has no Docker Hub pair to be absent from.
			continue
		}
		var present bool
		switch {
		case read[ref]:
			present = true
		case absent[ref]:
		case unread[ref], explicit[ref]:
			continue
		case wildcard[ref.Owner] && slices.Contains(r.collection.Listed, ref.Owner):
		default:
			continue
		}
		out = append(out, obs.Presence{Source: r.source, Owner: ref.Owner, Repo: ref.Repo, Present: present})
	}
	return out
}

func toSet(refs []registry.RepoRef) map[registry.RepoRef]bool {
	set := make(map[registry.RepoRef]bool, len(refs))
	for _, ref := range refs {
		set[ref] = true
	}
	return set
}
