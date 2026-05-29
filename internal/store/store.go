package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"registry-stats/internal/api"
	"registry-stats/internal/model"
)

// maxSnapshotSize caps the bytes loadSnapshot will read from a single
// file. 50 MB is well above any realistic snapshot (DockerHub + GHCR
// for hundreds of packages fits in well under 10 MB) and guards against
// a pathological file (tampering, partial-fsync corruption) exhausting
// memory during an API call.
const maxSnapshotSize = 50 << 20

// FS persists snapshots as /dir/YYYY-MM-DD.json using an atomic
// create-temp + rename pattern. Concurrent Save calls for the same
// date are serialized only through the filesystem's rename semantics;
// the cache stays consistent because Invalidate runs after rename.
//
// FS satisfies api.Store — the only interface its callers depend on.
// Direct field access is not part of the contract; use NewFS to
// construct an instance.
type FS struct {
	loadSF       singleflight.Group
	cache        *SnapshotCache
	pullIdx      *PullIndex
	idxReady     chan struct{} // closed when background rebuildIndex completes
	dir          string
	disableWrite bool
}

// Compile-time assertion that FS satisfies api.Store. Change detection
// lives here rather than in a per-test file so the contract is visible
// at the definition site.
var _ api.Store = (*FS)(nil)

// NewFS returns a store rooted at dir with a fresh snapshot cache.
// The cache is process-local; no on-disk index is kept. The pull index
// is rebuilt in a background goroutine so the constructor returns
// immediately without blocking on disk I/O. PullSeries callers block
// on the idxReady channel until the rebuild completes.
// When disableWrite is true, Save becomes a no-op (no JSON files written).
func NewFS(dir string, opts ...Option) *FS {
	s := &FS{dir: dir, cache: NewCache(), pullIdx: NewPullIndex(), idxReady: make(chan struct{})}
	for _, o := range opts {
		o(s)
	}
	go func() {
		s.rebuildIndex()
		close(s.idxReady)
	}()
	return s
}

// Option configures a FS store.
type Option func(*FS)

// WithDisableWrite makes Save a no-op (no JSON files written to disk).
func WithDisableWrite() Option {
	return func(s *FS) { s.disableWrite = true }
}

// Save atomically writes snap to <dir>/<snap.Timestamp YYYY-MM-DD>.json.
// Uses CreateTemp + fsync + rename so a power loss between write and
// rename can't leave a zero-length snapshot on disk (which would
// corrupt daily-delta calculations). Invalidates the cache entry for
// the written date so the next reader re-parses from disk.
//
// ctx is accepted for interface symmetry with future remote stores;
// the local filesystem implementation does not honor cancellation
// (os.Rename has no Context variant and the write is expected to be
// sub-second).
func (s *FS) Save(_ context.Context, snap *model.Snapshot) error {
	if s.disableWrite {
		// Still update the in-memory pull index so /metrics gauges work.
		s.pullIdx.Update(snap.Timestamp.Format("2006-01-02"), snap)
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	filename := snap.Timestamp.Format("2006-01-02") + ".json"
	destPath := filepath.Join(s.dir, filename)

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, ".snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp snapshot: %w", err)
	}
	// fsync before close + rename so the file's data blocks are on
	// disk before the directory entry flip. Without this, a power
	// loss between rename and the next journal flush can leave a
	// zero-length snapshot on disk after reboot.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync temp snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp snapshot: %w", err)
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename snapshot: %w", err)
	}
	// Best-effort directory fsync so the renamed entry itself
	// survives power loss. Failures here aren't worth returning —
	// the data file is already durable from tmp.Sync() above — but
	// log at debug so ops can trace storage-layer oddities.
	if dir, err := os.Open(s.dir); err == nil {
		if syncErr := dir.Sync(); syncErr != nil {
			slog.Debug("dir sync failed", "dir", s.dir, "error", syncErr)
		}
		if closeErr := dir.Close(); closeErr != nil {
			slog.Debug("dir close failed", "dir", s.dir, "error", closeErr)
		}
	}

	s.cache.Invalidate(snap.Timestamp.Format("2006-01-02"))
	s.pullIdx.Update(snap.Timestamp.Format("2006-01-02"), snap)

	slog.Info("snapshot saved", "file", filename, "size", len(data))
	return nil
}

// Load reads and parses the snapshot for the given date (YYYY-MM-DD).
// Validates the date format to prevent path traversal, caps file size
// at 50 MB, and serves from the (date, mtime) cache when possible.
func (s *FS) Load(_ context.Context, date string) (*model.Snapshot, error) {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("invalid date format %q: %w", date, err)
	}
	filename := date + ".json"
	path := filepath.Join(s.dir, filename)
	// Defense-in-depth: verify the resolved path stays inside s.dir.
	// time.Parse already constrains `date` to digits+hyphens, but this
	// guard catches (a) future refactors that weaken that validation
	// and (b) dev-host (Windows) path separators that time.Parse
	// wouldn't reject. Use filepath.Clean + HasPrefix containment
	// (rather than the previous Dir-equality check) because CodeQL's
	// go/path-injection analyzer recognises this pattern as a
	// sanitiser, while the Dir comparison alone wasn't enough to
	// prove safety to the dataflow engine.
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(s.dir) + string(filepath.Separator)
	if !strings.HasPrefix(cleanPath, cleanDir) {
		return nil, fmt.Errorf("path traversal blocked: %q", date)
	}

	info, err := os.Stat(cleanPath)
	if err != nil {
		return nil, err
	}
	if cached := s.cache.Get(date, info.ModTime()); cached != nil {
		return cached, nil
	}

	// Deduplicate concurrent cache-miss reads for the same date so
	// parallel HTTP requests (e.g. Grafana panel refresh) don't
	// redundantly read+unmarshal the same file.
	v, err, _ := s.loadSF.Do(date, func() (any, error) {
		// Re-check cache: another caller in the same flight may
		// have populated it before this closure runs.
		if cached := s.cache.Get(date, info.ModTime()); cached != nil {
			return cached, nil
		}

		f, openErr := os.Open(cleanPath)
		if openErr != nil {
			return nil, openErr
		}
		defer f.Close()

		if info.Size() > maxSnapshotSize {
			return nil, fmt.Errorf("snapshot too large: %d bytes", info.Size())
		}

		data, readErr := io.ReadAll(io.LimitReader(f, maxSnapshotSize))
		if readErr != nil {
			return nil, readErr
		}
		var snap model.Snapshot
		if unmarshalErr := json.Unmarshal(data, &snap); unmarshalErr != nil {
			return nil, unmarshalErr
		}
		s.cache.Put(date, info.ModTime(), &snap)
		return &snap, nil
	})
	if err != nil {
		return nil, err
	}
	snap, ok := v.(*model.Snapshot)
	if !ok {
		return nil, fmt.Errorf("store.Load: singleflight returned unexpected type %T", v)
	}
	return snap, nil
}

// ListDates returns all snapshot dates in chronological order. Skips
// non-date filenames, directories, and atomic-write temp files.
func (s *FS) ListDates(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		date := strings.TrimSuffix(name, ".json")
		if _, err := time.Parse("2006-01-02", date); err != nil {
			continue
		}
		dates = append(dates, date)
	}
	slices.Sort(dates)
	return dates, nil
}

// Prune deletes snapshot files older than retentionDays and returns
// the number pruned. retentionDays <= 0 is a no-op (keep forever).
// Honors ctx cancellation so shutdown doesn't race the sweep.
func (s *FS) Prune(ctx context.Context, retentionDays int) (int, error) {
	if ctx.Err() != nil {
		return 0, nil
	}
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02")
	slog.Debug("checking snapshot retention", "cutoff", cutoff, "retention_days", retentionDays)
	dates, err := s.ListDates(ctx)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, date := range dates {
		if ctx.Err() != nil {
			return pruned, nil
		}
		if date < cutoff {
			path := filepath.Join(s.dir, date+".json")
			if err := os.Remove(path); err != nil {
				slog.Error("failed to prune snapshot", "date", date, "error", err)
			} else {
				s.cache.Invalidate(date)
				pruned++
			}
		}
	}
	s.pullIdx.PruneOlderThan(cutoff)
	return pruned, nil
}

// CleanupStaleTmp removes leftover .snapshot-*.tmp files in the data
// directory that are older than one hour. Save's atomic-write pattern
// (CreateTemp + rename) leaks a temp file if the process is killed
// between the two steps; ListDates skips them but they would otherwise
// accumulate forever and eventually exhaust disk.
func (s *FS) CleanupStaleTmp(_ context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		slog.Debug("failed to read data dir for tmp sweep", "error", err)
		return nil
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, ".snapshot-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(s.dir, name)
		if err := os.Remove(path); err != nil {
			slog.Debug("failed to remove stale tmp file", "file", name, "error", err)
		} else {
			slog.Info("cleaned up stale tmp file", "file", name)
		}
	}
	return nil
}

// PullSeries returns the pre-computed pull-count time-series from the
// in-memory index. Blocks until the background index rebuild (started
// by NewFS) completes, then returns a copy safe for concurrent use.
// Design choice: blocking (rather than returning a "not ready" error)
// keeps the API contract unchanged and is simpler for callers; the
// wait is sub-second on healthy storage.
func (s *FS) PullSeries(_ context.Context) []model.PullEntry {
	<-s.idxReady
	return s.pullIdx.Entries()
}

// rebuildIndex populates the pull index from all existing snapshot
// files on disk. Called once at construction time. Errors on individual
// files are logged and skipped (same resilience as the handler path).
func (s *FS) rebuildIndex() {
	ctx := context.Background()
	dates, err := s.ListDates(ctx)
	if err != nil {
		slog.Debug("pull index rebuild: list dates failed", "error", err)
		return
	}
	snapshots := make(map[string]*model.Snapshot, len(dates))
	for _, date := range dates {
		snap, err := s.Load(ctx, date)
		if err != nil {
			slog.Debug("pull index rebuild: skip corrupt snapshot", "date", date, "error", err)
			continue
		}
		snapshots[date] = snap
	}
	s.pullIdx.Rebuild(snapshots)
}
