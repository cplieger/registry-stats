package collect_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"registry-stats/internal/api"
	"registry-stats/internal/collect"
	"registry-stats/internal/model"
	"registry-stats/internal/testsupport"
)

// fakeSource is a canned-response api.RegistrySource used to exercise
// the orchestrator in isolation from any real HTTP path.
type fakeSource struct {
	name      string
	source    model.RegistrySource
	entries   []model.RegistryEntry
	attempted int
	healthy   bool
	// lastRefs captures the refs Collect saw so tests can assert the
	// orchestrator plumbs RefsFor(name) through correctly.
	lastRefs []model.RepoRef
}

// Compile-time assertion: *fakeSource satisfies api.RegistrySource.
var _ api.RegistrySource = (*fakeSource)(nil)

func (f *fakeSource) Name() string                 { return f.name }
func (f *fakeSource) Source() model.RegistrySource { return f.source }

func (f *fakeSource) Collect(
	_ context.Context,
	refs []model.RepoRef,
) ([]model.RegistryEntry, int, bool) {
	f.lastRefs = refs
	return f.entries, f.attempted, f.healthy
}

// newFakeDockerHub and newFakeGHCR build fakeSources whose Name()
// and Source() stay in sync with the real dockerhub/ghcr clients.
// Keeps every test's arrange block terse without forcing each to
// remember to set both fields.
func newFakeDockerHub() *fakeSource {
	return &fakeSource{name: model.SourceDockerHub.String(), source: model.SourceDockerHub}
}
func newFakeGHCR() *fakeSource {
	return &fakeSource{name: model.SourceGHCR.String(), source: model.SourceGHCR}
}

// memStore is an in-memory api.Store used so tests can assert what
// the orchestrator asked to persist without touching the filesystem.
// It wraps testsupport.MemStore and adds a Saved slice to track
// every snapshot passed to Save (for assertion on call count).
type memStore struct {
	*testsupport.MemStore

	saved []*model.Snapshot
}

func newMemStore() *memStore {
	return &memStore{MemStore: testsupport.NewMemStore()}
}

func (m *memStore) Save(ctx context.Context, snap *model.Snapshot) error {
	if err := m.MemStore.Save(ctx, snap); err != nil {
		return err
	}
	cp := *snap
	m.saved = append(m.saved, &cp)
	return nil
}

// Compile-time assertion: *memStore satisfies api.Store.
var _ api.Store = (*memStore)(nil)

// fixedTime returns a clock that always returns the same instant.
func fixedTime(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRun_healthy_saves_snapshot_with_both_registries(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{
		{Name: "owner/app", LastUpdated: "2026-03-06T12:00:00Z", PullCount: 42,
			Tags: []model.TagInfo{{Name: "latest"}}},
	}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []model.RegistryEntry{{Name: "owner/pkg", DownloadCount: 500}}
	gh.attempted = 1
	gh.healthy = true

	store := newMemStore()
	fixed := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		Now:     fixedTime(fixed),
		RefsFor: func(name string) []model.RepoRef {
			switch name {
			case model.SourceDockerHub.String():
				return []model.RepoRef{{Owner: "owner", Repo: "app"}}
			case model.SourceGHCR.String():
				return []model.RepoRef{{Owner: "owner", Repo: "pkg"}}
			}
			return nil
		},
	})

	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if !healthy {
		t.Error("Run() healthy = false, want true")
	}
	if snap == nil {
		t.Fatal("Run() snap = nil, want non-nil")
	}
	if !snap.Timestamp.Equal(fixed) {
		t.Errorf("snap.Timestamp = %v, want %v", snap.Timestamp, fixed)
	}
	if len(snap.DockerHub) != 1 || snap.DockerHub[0].Repo != "owner/app" || snap.DockerHub[0].PullCount != 42 {
		t.Errorf("snap.DockerHub = %+v, want [owner/app 42]", snap.DockerHub)
	}
	if len(snap.DockerHub[0].Tags) != 1 || snap.DockerHub[0].Tags[0].Name != "latest" {
		t.Errorf("snap.DockerHub[0].Tags = %+v, want [latest]", snap.DockerHub[0].Tags)
	}
	if len(snap.GHCR) != 1 || snap.GHCR[0].Package != "owner/pkg" || snap.GHCR[0].DownloadCount != 500 {
		t.Errorf("snap.GHCR = %+v, want [owner/pkg 500]", snap.GHCR)
	}
	if len(store.saved) != 1 {
		t.Errorf("store.saved = %d, want 1", len(store.saved))
	}
	// RefsFor plumbed through correctly.
	if len(dh.lastRefs) != 1 || dh.lastRefs[0].Repo != "app" {
		t.Errorf("dh.lastRefs = %+v, want [{owner app}]", dh.lastRefs)
	}
	if len(gh.lastRefs) != 1 || gh.lastRefs[0].Repo != "pkg" {
		t.Errorf("gh.lastRefs = %+v, want [{owner pkg}]", gh.lastRefs)
	}
}

func TestRun_skips_empty_refs(t *testing.T) {
	ctx := t.Context()
	// GHCR has no refs — should be skipped entirely, not invoked.
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "owner/app", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR() // would fail if invoked (healthy stays false)

	store := newMemStore()
	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		Now:     time.Now,
		RefsFor: func(name string) []model.RepoRef {
			if name == model.SourceDockerHub.String() {
				return []model.RepoRef{{Owner: "owner", Repo: "app"}}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !healthy {
		t.Error("Run() healthy = false, want true (ghcr was skipped, not failed)")
	}
	if gh.lastRefs != nil {
		t.Errorf("gh should not have been invoked (empty refs), lastRefs = %+v", gh.lastRefs)
	}
}

func TestRun_no_sources_configured_does_not_save(t *testing.T) {
	ctx := t.Context()
	store := newMemStore()
	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if healthy {
		t.Error("Run() healthy = true, want false for empty-snapshot path")
	}
	if snap == nil {
		t.Fatal("Run() snap = nil, want non-nil empty snapshot")
	}
	if len(store.saved) != 0 {
		t.Errorf("store.saved = %d, want 0 (empty snapshots never persist)", len(store.saved))
	}
}

func TestRun_all_sources_empty_entries_does_not_save(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.attempted = 3 // healthy stays false
	gh := newFakeGHCR()
	gh.attempted = 2
	store := newMemStore()

	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []model.RepoRef {
			return []model.RepoRef{{Owner: "x", Repo: "y"}}
		},
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if healthy {
		t.Error("Run() healthy = true, want false when all collections failed")
	}
	if snap == nil || len(snap.DockerHub) != 0 || len(snap.GHCR) != 0 {
		t.Errorf("snap = %+v, want empty snapshot", snap)
	}
	if len(store.saved) != 0 {
		t.Error("all-empty cycle should not save; guard kicks in before Save")
	}
}

func TestRun_partial_success_saves_with_degraded_flag(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "owner/app", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	// GHCR has refs but returns no entries and flags unhealthy. The
	// snapshot still saves because DockerHub produced data.
	gh := newFakeGHCR()
	gh.attempted = 2
	store := newMemStore()

	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []model.RepoRef {
			return []model.RepoRef{{Owner: "o", Repo: "r"}}
		},
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if healthy {
		t.Error("Run() healthy = true, want false (GHCR flagged unhealthy)")
	}
	if len(store.saved) != 1 {
		t.Errorf("store.saved = %d, want 1 (DockerHub has data, partial save)", len(store.saved))
	}
	if len(store.saved) == 1 && len(store.saved[0].DockerHub) != 1 {
		t.Errorf("saved DockerHub = %+v, want 1 entry", store.saved[0].DockerHub)
	}
}

func TestRun_save_error_returns_err(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "o/a", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	saveErr := errors.New("disk full")
	store := newMemStore()
	store.SaveErr = saveErr

	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if err == nil || !errors.Is(err, saveErr) {
		t.Errorf("Run() err = %v, want wrapping %v", err, saveErr)
	}
	if healthy {
		t.Error("Run() healthy = true, want false on save error")
	}
	if snap != nil {
		t.Errorf("Run() snap = %+v, want nil on save error", snap)
	}
}

func TestRun_unknown_source_drops_entries_and_logs(t *testing.T) {
	ctx := t.Context()
	// Unknown source: name string isn't one of dockerhub/ghcr and
	// Source() returns SourceUnknown so the orchestrator falls into
	// the default branch.
	unknown := &fakeSource{
		name:      "mystery",
		source:    model.SourceUnknown,
		entries:   []model.RegistryEntry{{Name: "x/y", PullCount: 1}},
		attempted: 1, healthy: true,
	}
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "o/a", PullCount: 2}}
	dh.attempted = 1
	dh.healthy = true

	store := newMemStore()
	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{unknown, dh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !healthy {
		t.Error("Run() healthy = false, want true (dockerhub was fine)")
	}
	if len(store.saved) != 1 || len(store.saved[0].DockerHub) != 1 {
		t.Errorf("store.saved DockerHub = %+v, want 1 entry", store.saved)
	}
	// Unknown source's entries never appear in any typed slice.
	if len(store.saved[0].GHCR) != 0 {
		t.Errorf("snap.GHCR = %+v, want empty (unknown source should drop)", store.saved[0].GHCR)
	}
}

func TestRun_entries_with_empty_name_are_dropped(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{
		{Name: "", PullCount: 9}, // dropped
		{Name: "o/good", PullCount: 1},
	}
	dh.attempted = 2
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []model.RegistryEntry{
		{Name: "", DownloadCount: 7}, // dropped
		{Name: "o/p", DownloadCount: 3},
	}
	gh.attempted = 2
	gh.healthy = true

	store := newMemStore()
	_, _, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "x"}} },
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("store.saved = %d, want 1", len(store.saved))
	}
	snap := store.saved[0]
	if len(snap.DockerHub) != 1 || snap.DockerHub[0].Repo != "o/good" {
		t.Errorf("DockerHub empty-name should drop; got %+v", snap.DockerHub)
	}
	if len(snap.GHCR) != 1 || snap.GHCR[0].Package != "o/p" {
		t.Errorf("GHCR empty-name should drop; got %+v", snap.GHCR)
	}
}

func TestRun_nil_refsfor_skips_all_sources(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub() // healthy stays false
	store := newMemStore()
	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh},
		Store:   store,
		Logger:  testsupport.QuietLogger(),
		// RefsFor: nil
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if healthy {
		t.Error("Run() healthy = true, want false for no-op cycle")
	}
	if dh.lastRefs != nil {
		t.Errorf("dh.lastRefs = %+v, want nil (skipped)", dh.lastRefs)
	}
	if len(store.saved) != 0 {
		t.Error("store.saved should be empty when no source is invoked")
	}
}

func TestRun_defaults_logger_and_now(t *testing.T) {
	// Verify Run doesn't panic when Logger and Now are both nil; it
	// should fall back to slog.Default() and time.Now respectively.
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "o/a", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	store := newMemStore()
	snap, _, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh},
		Store:   store,
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if snap == nil {
		t.Fatal("snap = nil")
	}
	// Timestamp should be recent (within a generous 1-minute window).
	if time.Since(snap.Timestamp) > time.Minute {
		t.Errorf("snap.Timestamp = %v, should be recent", snap.Timestamp)
	}
}
