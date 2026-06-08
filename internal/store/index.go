package store

import (
	"sync"

	"github.com/cplieger/registry-stats/internal/model"
)

// PullIndex maintains a pre-computed time-series of per-repo pull
// counts, updated atomically by Save and Prune. Handlers read from
// it via Entries() instead of loading every snapshot from disk.
// The index is process-local (like SnapshotCache) — no on-disk
// persistence. On startup, NewFS rebuilds it from existing snapshots.
type PullIndex struct {
	entries []model.PullEntry // sorted by date, then repo
	mu      sync.RWMutex
}

// NewPullIndex returns an empty index ready for population.
func NewPullIndex() *PullIndex {
	return &PullIndex{}
}

// Entries returns a snapshot of all index entries. The returned slice
// is a copy safe for concurrent iteration by handlers.
func (idx *PullIndex) Entries() []model.PullEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]model.PullEntry, len(idx.entries))
	copy(out, idx.entries)
	return out
}

// Update replaces all entries for the given date with entries derived
// from the snapshot. Called by Save after a successful write.
func (idx *PullIndex) Update(date string, snap *model.Snapshot) {
	newEntries := entriesFromSnapshot(date, snap)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Remove existing entries for this date, then append new ones.
	filtered := idx.entries[:0]
	for _, e := range idx.entries {
		if e.Date != date {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered, newEntries...)
	idx.entries = filtered
}

// PruneOlderThan removes all entries with Date < cutoff.
func (idx *PullIndex) PruneOlderThan(cutoff string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	filtered := idx.entries[:0]
	for _, e := range idx.entries {
		if e.Date >= cutoff {
			filtered = append(filtered, e)
		}
	}
	idx.entries = filtered
}

// Rebuild replaces the entire index from a set of (date, snapshot)
// pairs. Used at startup to populate from existing files.
func (idx *PullIndex) Rebuild(snapshots map[string]*model.Snapshot) {
	all := make([]model.PullEntry, 0, len(snapshots)*4)
	for date, snap := range snapshots {
		all = append(all, entriesFromSnapshot(date, snap)...)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.entries = all
}

// entriesFromSnapshot extracts PullEntry records from a snapshot.
func entriesFromSnapshot(date string, snap *model.Snapshot) []model.PullEntry {
	return snap.PullEntries(date)
}
