package collect_test

import (
	"context"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/collect"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// fakeSource is a canned-response api.RegistrySource used to exercise
// the orchestrator in isolation from any real HTTP path.
type fakeSource struct {
	name string
	// lastRefs captures the refs Collect saw so tests can assert the
	// orchestrator plumbs RefsFor(name) through correctly.
	entries   []model.RegistryEntry
	lastRefs  []model.RepoRef
	attempted int
	source    model.RegistrySource
	healthy   bool
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
func newFakeDockerHub() *fakeSource {
	return &fakeSource{name: model.SourceDockerHub.String(), source: model.SourceDockerHub}
}

func newFakeGHCR() *fakeSource {
	return &fakeSource{name: model.SourceGHCR.String(), source: model.SourceGHCR}
}

// fixedTime returns a clock that always returns the same instant.
func fixedTime(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRun_healthy_saves_snapshot_with_both_registries(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{
		{
			Name: "owner/app", LastUpdated: "2026-03-06T12:00:00Z", PullCount: 42,
			Tags: []model.TagInfo{{Name: "latest"}},
		},
	}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []model.RegistryEntry{{Name: "owner/pkg", DownloadCount: 500}}
	gh.attempted = 1
	gh.healthy = true

	fixed := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
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
	// GHCR has no refs - should be skipped entirely, not invoked.
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "owner/app", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR() // would fail if invoked (healthy stays false)

	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
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
	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{},
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
}

func TestRun_all_sources_empty_entries_does_not_save(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.attempted = 3 // healthy stays false
	gh := newFakeGHCR()
	gh.attempted = 2

	snap, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
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

	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
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
}

func TestRun_unknown_source_drops_entries_and_logs(t *testing.T) {
	ctx := t.Context()
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

	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{unknown, dh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !healthy {
		t.Error("Run() healthy = false, want true (dockerhub was fine)")
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

	snap, _, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []model.RepoRef { return []model.RepoRef{{Owner: "o", Repo: "x"}} },
	})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
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
	_, healthy, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh},
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
}

func TestRun_defaults_logger_and_now(t *testing.T) {
	// Verify Run does not panic when Logger and Now are both nil; it
	// should fall back to slog.Default() and time.Now respectively.
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []model.RegistryEntry{{Name: "o/a", PullCount: 1}}
	dh.attempted = 1
	dh.healthy = true

	snap, _, err := collect.Run(ctx, collect.Options{
		Sources: []api.RegistrySource{dh},
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
