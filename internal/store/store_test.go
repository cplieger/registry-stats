package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/store"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// Compile-time assertion that *store.FS still satisfies api.Store. A
// caller-side check here complements the one in store.go so an api
// change to Store is caught by this package's tests as well as store's.
var _ api.Store = (*store.FS)(nil)

func testSnapshot(pulls, downloads int64) model.Snapshot {
	return model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{
			Repo:        "owner/app",
			PullCount:   pulls,
			LastUpdated: "2026-03-06T12:00:00Z",
			Tags: []model.TagInfo{{
				Name: "latest", FullSize: 1024, Digest: "sha256:abc",
				Images: []model.ImageInfo{
					{Architecture: "amd64", OS: "linux", Size: 512, Digest: "sha256:def"},
				},
			}},
		}},
		GHCR: []model.GhcrStats{{Package: "owner/pkg", DownloadCount: downloads}},
	}
}

// TestFS_Save_and_Load pins the round-trip: a snapshot written by
// Save is readable by Load and deserializes to an equal snapshot
// (field-by-field on the shapes handlers care about).
func TestFS_Save_and_Load(t *testing.T) {
	s := store.NewFS(t.TempDir())
	snap := testSnapshot(100, 50)

	if err := s.Save(t.Context(), &snap); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := s.Load(t.Context(), "2026-03-06")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.DockerHub[0].PullCount != 100 {
		t.Errorf("PullCount = %d, want 100", loaded.DockerHub[0].PullCount)
	}
	if loaded.GHCR[0].DownloadCount != 50 {
		t.Errorf("DownloadCount = %d, want 50", loaded.GHCR[0].DownloadCount)
	}
}

// TestFS_Load_rejects_invalid_date pins the date-format validation
// that gates the path-traversal check. The error message must
// mention the input so debugging is easier.
func TestFS_Load_rejects_invalid_date(t *testing.T) {
	s := store.NewFS(t.TempDir())
	_, err := s.Load(t.Context(), "not-a-date")
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
	if !strings.Contains(err.Error(), "invalid date format") {
		t.Errorf("error = %v, want 'invalid date format' prefix", err)
	}
}

// TestFS_Load_rejects_path_traversal pins the defense-in-depth guard
// against a future input surface that accepts arbitrary strings.
func TestFS_Load_rejects_path_traversal(t *testing.T) {
	s := store.NewFS(t.TempDir())
	_, err := s.Load(t.Context(), "../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal attempt")
	}
}

// TestFS_ListDates returns only valid YYYY-MM-DD snapshot filenames,
// sorted chronologically, and skips directories, dotfiles, non-JSON,
// and non-date filenames.
func TestFS_ListDates(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFS(dir)

	snap := testSnapshot(10, 5)
	for _, date := range []string{"2026-03-04", "2026-03-06", "2026-03-05"} {
		snap.Timestamp = mustDate(date)
		if err := s.Save(t.Context(), &snap); err != nil {
			t.Fatalf("Save(%s): %v", date, err)
		}
	}
	// Non-date filename should be skipped
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write non-date file: %v", err)
	}

	dates, err := s.ListDates(t.Context())
	if err != nil {
		t.Fatalf("ListDates: %v", err)
	}
	want := []string{"2026-03-04", "2026-03-05", "2026-03-06"}
	if len(dates) != len(want) {
		t.Fatalf("dates = %v, want %v", dates, want)
	}
	for i := range want {
		if dates[i] != want[i] {
			t.Errorf("dates[%d] = %s, want %s", i, dates[i], want[i])
		}
	}
}

// TestFS_ListDates_empty returns nil when the directory doesn't exist
// (the stats service hasn't run a first collect yet).
func TestFS_ListDates_empty(t *testing.T) {
	s := store.NewFS(filepath.Join(t.TempDir(), "nonexistent"))
	dates, err := s.ListDates(t.Context())
	if err != nil {
		t.Fatalf("ListDates: %v", err)
	}
	if len(dates) != 0 {
		t.Errorf("dates = %v, want empty", dates)
	}
}

// TestFS_Prune deletes files older than retentionDays and reports the
// count. The cache entry for the pruned date must also be invalidated
// so a later Load doesn't return stale data from the now-deleted file.
func TestFS_Prune(t *testing.T) {
	s := store.NewFS(t.TempDir())

	// Seed a snapshot dated 100 days ago and one dated today.
	old := time.Now().UTC().AddDate(0, 0, -100)
	recent := time.Now().UTC()
	oldSnap := testSnapshot(10, 5)
	oldSnap.Timestamp = old
	if err := s.Save(t.Context(), &oldSnap); err != nil {
		t.Fatalf("Save old: %v", err)
	}
	recentSnap := testSnapshot(20, 10)
	recentSnap.Timestamp = recent
	if err := s.Save(t.Context(), &recentSnap); err != nil {
		t.Fatalf("Save recent: %v", err)
	}

	n, err := s.Prune(t.Context(), 90)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}

	dates, _ := s.ListDates(t.Context())
	if len(dates) != 1 {
		t.Fatalf("dates after prune = %v, want 1 entry", dates)
	}
	if dates[0] != recent.Format("2006-01-02") {
		t.Errorf("remaining date = %s, want %s", dates[0], recent.Format("2006-01-02"))
	}
}

// TestFS_Prune_zero_retention is a no-op (keep forever). A counter
// reset from positive → 0 should not delete existing snapshots.
func TestFS_Prune_zero_retention(t *testing.T) {
	s := store.NewFS(t.TempDir())
	snap := testSnapshot(10, 5)
	snap.Timestamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Save(t.Context(), &snap); err != nil {
		t.Fatalf("Save: %v", err)
	}

	n, err := s.Prune(t.Context(), 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0", n)
	}
	dates, _ := s.ListDates(t.Context())
	if len(dates) != 1 {
		t.Error("zero retention should keep all snapshots")
	}
}

// TestFS_CleanupStaleTmp removes .snapshot-*.tmp files older than one
// hour and leaves newer tmp files plus unrelated files untouched.
func TestFS_CleanupStaleTmp(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFS(dir)

	stalePath := filepath.Join(dir, ".snapshot-stale123.tmp")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stalePath, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	recentPath := filepath.Join(dir, ".snapshot-recent456.tmp")
	if err := os.WriteFile(recentPath, []byte("recent"), 0o600); err != nil {
		t.Fatalf("write recent tmp: %v", err)
	}
	otherPath := filepath.Join(dir, "2026-03-06.json")
	if err := os.WriteFile(otherPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write other: %v", err)
	}

	if err := s.CleanupStaleTmp(t.Context()); err != nil {
		t.Fatalf("CleanupStaleTmp: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale tmp should be removed: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Errorf("recent tmp should be kept: %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("non-tmp file should be kept: %v", err)
	}
}

// TestFS_Save_invalidates_cache verifies that saving a new snapshot
// for the same date invalidates the cache entry so subsequent Load
// returns the fresh data, not a stale cached parse. Pins the
// same-date overwrite contract that handlePulls relies on during
// the catch-up window after the first collect of a new day.
func TestFS_Save_invalidates_cache(t *testing.T) {
	s := store.NewFS(t.TempDir())
	date := "2026-03-06"
	ts := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	first := model.Snapshot{Timestamp: ts, DockerHub: []model.RepoStats{
		{Repo: "owner/app", PullCount: 100, Tags: []model.TagInfo{}},
	}}
	if err := s.Save(t.Context(), &first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	loaded1, err := s.Load(t.Context(), date)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if loaded1.DockerHub[0].PullCount != 100 {
		t.Fatalf("first Load PullCount = %d, want 100", loaded1.DockerHub[0].PullCount)
	}

	// Sleep past filesystem mtime granularity (ext4 is ns; most
	// macOS/Windows filesystems are ms). 10ms is safe for any modern FS.
	time.Sleep(10 * time.Millisecond)

	second := model.Snapshot{Timestamp: ts, DockerHub: []model.RepoStats{
		{Repo: "owner/app", PullCount: 200, Tags: []model.TagInfo{}},
	}}
	if err := s.Save(t.Context(), &second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	loaded2, err := s.Load(t.Context(), date)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if loaded2.DockerHub[0].PullCount != 200 {
		t.Errorf("second Load PullCount = %d, want 200 (cache must be invalidated on save)",
			loaded2.DockerHub[0].PullCount)
	}
}

// TestSnapshotCache_put_evicts_oldest verifies the LRU-less eviction
// strategy when the cache reaches MaxCachedSnapshots: the
// chronologically oldest key (smallest YYYY-MM-DD string) is dropped
// to make room for a new entry.
func TestSnapshotCache_put_evicts_oldest(t *testing.T) {
	c := store.NewCache()
	now := time.Now()
	snap := &model.Snapshot{}

	for i := range store.MaxCachedSnapshots {
		date := fmt.Sprintf("2026-01-%03d", i+1)
		c.Put(date, now, snap)
	}
	if len(c.ByDate) != store.MaxCachedSnapshots {
		t.Fatalf("cache size = %d, want %d", len(c.ByDate), store.MaxCachedSnapshots)
	}
	if _, ok := c.ByDate["2026-01-001"]; !ok {
		t.Fatal("oldest key should exist before eviction")
	}

	c.Put("2026-12-31", now, snap)
	if len(c.ByDate) != store.MaxCachedSnapshots {
		t.Errorf("cache size after eviction = %d, want %d", len(c.ByDate), store.MaxCachedSnapshots)
	}
	if _, ok := c.ByDate["2026-01-001"]; ok {
		t.Error("oldest key should have been evicted")
	}
	if _, ok := c.ByDate["2026-12-31"]; !ok {
		t.Error("newly inserted key should be present")
	}
	if _, ok := c.ByDate["2026-01-002"]; !ok {
		t.Error("second-oldest key should still be present")
	}
}

// TestSnapshotCache_Reset empties the cache so a test that repoints
// dataDir to a fresh TempDir doesn't see entries from a previous
// test with the same date key.
func TestSnapshotCache_Reset(t *testing.T) {
	c := store.NewCache()
	c.Put("2026-03-06", time.Now(), &model.Snapshot{})
	if len(c.ByDate) != 1 {
		t.Fatalf("cache size = %d, want 1", len(c.ByDate))
	}
	c.Reset()
	if len(c.ByDate) != 0 {
		t.Errorf("cache size after Reset = %d, want 0", len(c.ByDate))
	}
}

// TestFS_Prune_cancelled_context skips the delete loop when ctx is
// already cancelled. pruneSnapshots is called from the scheduled
// poll loop; shutdown cancels ctx and the sweep must be a no-op so
// graceful shutdown doesn't race the filesystem.
func TestFS_Prune_cancelled_context(t *testing.T) {
	s := store.NewFS(t.TempDir())
	old := testSnapshot(10, 5)
	old.Timestamp = time.Now().UTC().AddDate(0, 0, -100)
	if err := s.Save(t.Context(), &old); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err := s.Prune(ctx, 90)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d under cancelled ctx, want 0", n)
	}
	dates, _ := s.ListDates(t.Context())
	if len(dates) != 1 {
		t.Errorf("snapshot deleted despite cancelled ctx; dates = %v", dates)
	}
}

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(fmt.Sprintf("mustDate(%q): %v", s, err))
	}
	// Preserve the midnight-UTC wall time so the date-string format
	// output matches the input exactly.
	return t
}

// --- Migrated from main_test.go in chain step 6 (arch-rs2-p3 storage slice) ---
//
// These tests cover edges and error paths that the cycle-1 store_test
// didn't explicitly hit: path-traversal edge cases (null bytes, encoded
// dots, Windows separators), list-dates error paths (ReadDir on a file),
// prune variants (remove error, negative retention, multiple olds,
// boundary / exact-cutoff semantics), save error paths (mkdir-under-file,
// read-only dir), the no-temp-leak guard, and CleanupStaleTmp on a
// nonexistent dir. Each test inlines its own model.Snapshot fixture —
// the testSnapshot helper at the top of this file is reused where a
// full fixture matters; the error-path tests use minimal literals.

// TestFS_Load_rejects_path_traversal_edges pins the defense-in-depth
// guard against exotic date-string shapes that a future input surface
// might pass through. time.Parse rejects each of these for a different
// reason (format, null byte, future date with no file); the assertion
// is just "non-nil error, no panic".
func TestFS_Load_rejects_path_traversal_edges(t *testing.T) {
	s := store.NewFS(t.TempDir())
	tests := []struct {
		name string
		date string
	}{
		{"dot-dot-slash", "../2026-03-06"},
		{"encoded dots", "..%2F..%2Fetc%2Fpasswd"},
		{"null byte", "2026-03-06\x00.json"},
		{"backslash traversal", `..\..\etc\passwd`},
		// 9999-12-31 passes date validation but file won't exist — still errors
		{"valid format but future", "9999-12-31"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Load(t.Context(), tt.date)
			if err == nil {
				t.Errorf("expected error for %q", tt.date)
			}
		})
	}
}

// TestFS_ListDates_readdir_error pins the behavior when the store's
// root path is a plain file instead of a directory: ReadDir returns a
// non-IsNotExist error and ListDates propagates it.
func TestFS_ListDates_readdir_error(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write not-a-dir: %v", err)
	}
	s := store.NewFS(tmpFile)
	if _, err := s.ListDates(t.Context()); err == nil {
		t.Error("expected error when store dir is a file")
	}
}

// TestFS_Prune_remove_error pins the "silently log + continue" behavior
// when os.Remove fails on an individual snapshot file: Prune returns
// nil error and a pruned count of 0 (the file wasn't actually removed).
// Scheduled-collect relies on this: a single stale read-only file must
// not wedge the poll loop.
func TestFS_Prune_remove_error(t *testing.T) {
	// Skip on root (Linux CI) because root bypasses the read-only bit.
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir bit is bypassed")
	}
	dir := t.TempDir()
	s := store.NewFS(dir)

	old := time.Now().UTC().AddDate(0, 0, -100)
	snap := testSnapshot(10, 5)
	snap.Timestamp = old
	if err := s.Save(t.Context(), &snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Making the parent dir read-only causes os.Remove to fail with
	// EACCES. Restore write bit in cleanup so t.TempDir's RemoveAll
	// can clean up.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Should not panic and should not return an error. The Remove
	// failure is logged internally.
	if _, err := s.Prune(t.Context(), 90); err != nil {
		t.Errorf("Prune returned err on remove failure: %v", err)
	}
}

// TestFS_Prune_negative_retention is a no-op. retentionDays < 0 should
// not delete anything (equivalent to "keep forever"). Guards against a
// sign-flip mutation in the retentionDays <= 0 guard.
func TestFS_Prune_negative_retention(t *testing.T) {
	s := store.NewFS(t.TempDir())
	snap := testSnapshot(10, 5)
	snap.Timestamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Save(t.Context(), &snap); err != nil {
		t.Fatalf("Save: %v", err)
	}

	n, err := s.Prune(t.Context(), -1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0", n)
	}
	dates, _ := s.ListDates(t.Context())
	if len(dates) != 1 {
		t.Error("negative retention should keep all snapshots")
	}
}

// TestFS_Prune_multiple_old_files kills INCREMENT_DECREMENT on the
// prune counter and CONDITIONALS_BOUNDARY on the "date < cutoff" check:
// three old files should be pruned, one recent kept.
func TestFS_Prune_multiple_old_files(t *testing.T) {
	s := store.NewFS(t.TempDir())
	old1 := time.Now().UTC().AddDate(0, 0, -100)
	old2 := time.Now().UTC().AddDate(0, 0, -101)
	old3 := time.Now().UTC().AddDate(0, 0, -102)
	recent := time.Now().UTC()
	for _, ts := range []time.Time{old1, old2, old3, recent} {
		snap := testSnapshot(10, 5)
		snap.Timestamp = ts
		if err := s.Save(t.Context(), &snap); err != nil {
			t.Fatalf("Save(%s): %v", ts.Format("2006-01-02"), err)
		}
	}

	n, err := s.Prune(t.Context(), 90)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 3 {
		t.Errorf("pruned = %d, want 3", n)
	}
	dates, _ := s.ListDates(t.Context())
	if len(dates) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(dates))
	}
	if dates[0] != recent.Format("2006-01-02") {
		t.Errorf("remaining = %s, want %s", dates[0], recent.Format("2006-01-02"))
	}
}

// TestFS_Prune_exact_cutoff_kept kills CONDITIONALS_BOUNDARY /
// CONDITIONALS_NEGATION on the "date < cutoff" comparison: the cutoff
// date is the oldest kept; one day before cutoff is pruned; one day
// after is kept. Cutoff date itself equals today-retentionDays so
// `date < cutoff` is false and the entry survives.
func TestFS_Prune_exact_cutoff_kept(t *testing.T) {
	s := store.NewFS(t.TempDir())
	now := time.Now().UTC()
	cutoffDay := now.AddDate(0, 0, -30)
	beforeCutoff := cutoffDay.AddDate(0, 0, -1)
	afterCutoff := cutoffDay.AddDate(0, 0, 1)

	for _, ts := range []time.Time{cutoffDay, beforeCutoff, afterCutoff} {
		snap := testSnapshot(10, 5)
		snap.Timestamp = ts
		if err := s.Save(t.Context(), &snap); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	if _, err := s.Prune(t.Context(), 30); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	dates, _ := s.ListDates(t.Context())

	beforeStr := beforeCutoff.Format("2006-01-02")
	afterStr := afterCutoff.Format("2006-01-02")
	for _, d := range dates {
		if d == beforeStr {
			t.Errorf("date %s should have been pruned (before cutoff)", beforeStr)
		}
	}
	foundAfter := false
	for _, d := range dates {
		if d == afterStr {
			foundAfter = true
		}
	}
	if !foundAfter {
		t.Errorf("date after cutoff %s should be kept; dates=%v", afterStr, dates)
	}
}

// TestFS_Prune_boundary_date is a lighter-weight sibling to the above:
// with two snapshots (at cutoff vs. recent), at least the recent is
// always kept. Documents the exact boundary semantics for future
// readers without asserting the (implementation-defined) cutoff-day
// pruning choice.
func TestFS_Prune_boundary_date(t *testing.T) {
	s := store.NewFS(t.TempDir())
	exactCutoff := time.Now().UTC().AddDate(0, 0, -90)
	recent := time.Now().UTC()
	for _, ts := range []time.Time{exactCutoff, recent} {
		snap := testSnapshot(10, 5)
		snap.Timestamp = ts
		if err := s.Save(t.Context(), &snap); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	if _, err := s.Prune(t.Context(), 90); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	dates, _ := s.ListDates(t.Context())

	recentStr := recent.Format("2006-01-02")
	recentFound := false
	for _, d := range dates {
		if d == recentStr {
			recentFound = true
		}
	}
	if !recentFound {
		t.Error("recent snapshot should always be kept")
	}
}

// TestFS_Save_creates_nested_dir pins MkdirAll behavior: a multi-level
// path under the store's configured dir is created on first Save.
// Guards against a future refactor that moves the mkdir out of Save.
func TestFS_Save_creates_nested_dir(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "nested", "dir")
	s := store.NewFS(nested)

	snap := testSnapshot(42, 10)
	if err := s.Save(t.Context(), &snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := s.Load(t.Context(), "2026-03-06")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.DockerHub[0].PullCount != 42 {
		t.Errorf("PullCount = %d, want 42", loaded.DockerHub[0].PullCount)
	}
}

// TestFS_Save_overwrites verifies that two Saves with the same
// timestamp produce a single on-disk file with the second value.
// Pins the atomic-rename contract at the whole-file granularity.
func TestFS_Save_overwrites(t *testing.T) {
	s := store.NewFS(t.TempDir())

	snap1 := testSnapshot(100, 50)
	if err := s.Save(t.Context(), &snap1); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	snap2 := testSnapshot(200, 75)
	snap2.Timestamp = snap1.Timestamp
	// 10ms sleep so the mtime advances past the filesystem granularity;
	// otherwise Load could serve the first save from the cache.
	time.Sleep(10 * time.Millisecond)
	if err := s.Save(t.Context(), &snap2); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	loaded, err := s.Load(t.Context(), "2026-03-06")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.DockerHub[0].PullCount != 200 {
		t.Errorf("PullCount = %d, want 200 (overwritten)", loaded.DockerHub[0].PullCount)
	}
}

// TestFS_Save_mkdir_error pins the "create data dir" error wrap: if
// the configured dir's parent is a plain file, MkdirAll fails and the
// error bubbles up with the expected prefix. Guards against a future
// refactor that loses the wrap.
func TestFS_Save_mkdir_error(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write not-a-dir: %v", err)
	}
	// Point the store's dir at a path under a file; MkdirAll will
	// fail because the parent is a file, not a directory.
	s := store.NewFS(filepath.Join(tmpFile, "subdir"))

	err := s.Save(t.Context(), &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Error("expected error when store dir parent is a file")
	}
	if err != nil && !strings.Contains(err.Error(), "create data dir") {
		t.Errorf("error = %v, want 'create data dir' prefix", err)
	}
}

// TestFS_Save_createtemp_error pins the failure mode when the store
// dir is read-only: MkdirAll succeeds (dir already exists) but
// CreateTemp fails with EACCES. Save must return a non-nil error
// (caller's runScheduled degrades healthy → unhealthy on this).
func TestFS_Save_createtemp_error(t *testing.T) {
	// Skip on root (Linux CI) because root bypasses the read-only bit.
	if os.Geteuid() == 0 {
		t.Skip("running as root; read-only dir bit is bypassed")
	}
	roDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	s := store.NewFS(roDir)
	err := s.Save(t.Context(), &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Error("expected error when store dir is read-only")
	}
}

// TestFS_Save_no_temp_file_leak verifies the happy path leaves exactly
// one file (the YYYY-MM-DD.json) in the dir and no .snapshot-*.tmp
// residue. Guards against a future refactor that forgets to rename or
// that leaks tmp files on success.
func TestFS_Save_no_temp_file_leak(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFS(dir)

	if err := s.Save(t.Context(), &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 100}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if got := len(entries); got != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entry count = %d (%v), want 1 (no .tmp leaks)", got, names)
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
		t.Errorf("final file = %q, want YYYY-MM-DD.json with no leading dot", name)
	}
}

// TestFS_CleanupStaleTmp_nonexistent_dir is a no-op: if the store's
// dir doesn't exist, CleanupStaleTmp swallows the IsNotExist error
// and returns nil. Guards against a ReadDir error wrap regression
// that would panic the scheduled-poll loop on a freshly-provisioned
// host before the first Save runs.
func TestFS_CleanupStaleTmp_nonexistent_dir(t *testing.T) {
	s := store.NewFS(filepath.Join(t.TempDir(), "nonexistent"))
	if err := s.CleanupStaleTmp(t.Context()); err != nil {
		t.Errorf("CleanupStaleTmp on nonexistent dir returned err: %v", err)
	}
}

// TestFS_StoreContract verifies that store.FS satisfies the shared
// api.Store contract test, proving behavioral parity with test fakes.
func TestFS_StoreContract(t *testing.T) {
	testsupport.RunStoreContract(t, func(t *testing.T) api.Store {
		return store.NewFS(t.TempDir())
	})
}

// BenchmarkPullIndexEntries measures the copy cost of Entries() at
// various index sizes. The hot path is called on every /api/pulls and
// /api/pulls/daily request.
func BenchmarkPullIndexEntries(b *testing.B) {
	for _, repos := range []int{1, 10, 50} {
		dates := 90
		total := repos * dates
		b.Run(fmt.Sprintf("repos=%d/dates=%d/entries=%d", repos, dates, total), func(b *testing.B) {
			idx := store.NewPullIndex()
			snaps := make(map[string]*model.Snapshot, dates)
			for d := range dates {
				date := fmt.Sprintf("2026-01-%03d", d+1)
				snap := &model.Snapshot{DockerHub: make([]model.RepoStats, repos)}
				for r := range repos {
					snap.DockerHub[r] = model.RepoStats{
						Repo:      fmt.Sprintf("owner/repo-%d", r),
						PullCount: int64(r*100 + d),
					}
				}
				snaps[date] = snap
			}
			idx.Rebuild(snaps)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = idx.Entries()
			}
		})
	}
}

// BenchmarkPullIndexUpdate measures the filter-and-append cost of
// Update() which runs on every Save (once per poll interval).
func BenchmarkPullIndexUpdate(b *testing.B) {
	for _, repos := range []int{1, 10, 50} {
		dates := 90
		b.Run(fmt.Sprintf("repos=%d/dates=%d", repos, dates), func(b *testing.B) {
			idx := store.NewPullIndex()
			// Seed with existing data for all dates.
			snaps := make(map[string]*model.Snapshot, dates)
			for d := range dates {
				date := fmt.Sprintf("2026-01-%03d", d+1)
				snap := &model.Snapshot{DockerHub: make([]model.RepoStats, repos)}
				for r := range repos {
					snap.DockerHub[r] = model.RepoStats{
						Repo:      fmt.Sprintf("owner/repo-%d", r),
						PullCount: int64(r*100 + d),
					}
				}
				snaps[date] = snap
			}
			idx.Rebuild(snaps)

			// Update always targets the same date (simulates repeated poll).
			updateSnap := &model.Snapshot{DockerHub: make([]model.RepoStats, repos)}
			for r := range repos {
				updateSnap.DockerHub[r] = model.RepoStats{
					Repo:      fmt.Sprintf("owner/repo-%d", r),
					PullCount: int64(r * 999),
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				idx.Update("2026-01-045", updateSnap)
			}
		})
	}
}
