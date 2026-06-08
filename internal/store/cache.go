// Package store persists registry-stats snapshots on the local filesystem.
//
// The FS type wraps a data directory plus a (date, mtime)-keyed cache of
// parsed snapshots so handler paths that walk the full retention window
// (handlePulls, handlePullsDaily) don't re-parse every file on every
// request. The on-disk format (/data/YYYY-MM-DD.json, one file per day)
// and the cache eviction threshold (MaxCachedSnapshots) are part of the
// inviolate contract — changing either would break the existing Grafana
// dashboards that depend on the snapshot layout.
package store

import (
	"sync"
	"time"

	"github.com/cplieger/registry-stats/internal/model"
)

// MaxCachedSnapshots caps the in-memory snapshot cache. 120 entries
// covers the default 90-day retention with headroom for date-range
// queries that span slightly beyond retention. With RETENTION_DAYS=0
// (keep forever) this prevents unbounded memory growth.
const MaxCachedSnapshots = 120

// CachedSnap is one entry in the SnapshotCache. Mtime is the modtime of
// the source file at the moment the cache entry was populated; a later
// Get with a newer mtime misses the cache and the reader re-parses.
// Fields are exported so tests can construct zero-value entries directly.
type CachedSnap struct {
	Mtime time.Time
	Snap  *model.Snapshot
}

// SnapshotCache is a small process-local LRU-less cache for parsed
// snapshot files. Entries are keyed by (date, mtime); if the on-disk
// file's mtime changes, the cache miss triggers a fresh read. The
// cache is shared across handlers and the collect() path via the FS
// instance owning it.
//
// ByDate and Mu are exported so tests can reset the cache directly
// (the handler test suite in the main package points dataDir at a
// fresh t.TempDir() per test and needs to discard stale entries from
// earlier runs without plumbing a testing hook through every handler).
type SnapshotCache struct {
	ByDate map[string]CachedSnap
	Mu     sync.Mutex
}

// NewCache returns an empty cache ready for use.
func NewCache() *SnapshotCache {
	return &SnapshotCache{ByDate: make(map[string]CachedSnap)}
}

// Reset empties the cache under lock. Tests that repoint dataDir call
// this so a previous test's entries for the same date key (under a
// different temp dir) can't be served from cache.
func (c *SnapshotCache) Reset() {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	c.ByDate = make(map[string]CachedSnap)
}

// Get returns the cached snapshot for date if its stored mtime equals
// the file's current mtime; otherwise returns nil (caller re-reads).
func (c *SnapshotCache) Get(date string, mtime time.Time) *model.Snapshot {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if entry, ok := c.ByDate[date]; ok && entry.Mtime.Equal(mtime) {
		return entry.Snap
	}
	return nil
}

// Put stores snap under (date, mtime), evicting the chronologically
// oldest entry if the cache is at capacity. Dates are YYYY-MM-DD
// strings that sort chronologically, so a linear scan for the minimum
// key is correct and fast for <= MaxCachedSnapshots entries.
func (c *SnapshotCache) Put(date string, mtime time.Time, snap *model.Snapshot) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if len(c.ByDate) >= MaxCachedSnapshots {
		oldest := ""
		for k := range c.ByDate {
			if oldest == "" || k < oldest {
				oldest = k
			}
		}
		if oldest != "" {
			delete(c.ByDate, oldest)
		}
	}
	c.ByDate[date] = CachedSnap{Mtime: mtime, Snap: snap}
}

// Invalidate removes the entry for date if present. Called on save
// (so subsequent readers re-parse the fresh file) and on prune.
func (c *SnapshotCache) Invalidate(date string) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	delete(c.ByDate, date)
}
