package collect_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/collect"
	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
)

// fakeSource is a canned-response Source used to exercise
// the orchestrator in isolation from any real HTTP path.
type fakeSource struct {
	// lastRefs captures the refs Collect saw so tests can assert the
	// orchestrator plumbs RefsFor(Source().String()) through correctly.
	entries   []registry.Entry
	lastRefs  []registry.RepoRef
	attempted int
	source    registry.ID
	healthy   bool
}

// Compile-time assertion: *fakeSource satisfies Source.
var _ collect.Source = (*fakeSource)(nil)

func (f *fakeSource) Source() registry.ID { return f.source }

func (f *fakeSource) Collect(
	_ context.Context,
	refs []registry.RepoRef,
) ([]registry.Entry, int, bool) {
	f.lastRefs = refs
	return f.entries, f.attempted, f.healthy
}

// newFakeDockerHub and newFakeGHCR build fakeSources whose Source() matches
// the real dockerhub/ghcr clients, so the orchestrator routes them the same way.
func newFakeDockerHub() *fakeSource {
	return &fakeSource{source: registry.DockerHub}
}

func newFakeGHCR() *fakeSource {
	return &fakeSource{source: registry.GHCR}
}

// fixedTime returns a clock that always returns the same instant.
func fixedTime(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRun_healthy_returns_stamped_records_for_both_registries(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{
		{Owner: "owner", Repo: "app", Pulls: 42, TagCount: 2},
	}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 500}}
	gh.attempted = 1
	gh.healthy = true

	fixed := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		Now:     fixedTime(fixed),
		RefsFor: func(name string) []registry.RepoRef {
			switch name {
			case registry.DockerHub.String():
				return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
			case registry.GHCR.String():
				return []registry.RepoRef{{Owner: "owner", Repo: "pkg"}}
			}
			return nil
		},
	})
	if !healthy {
		t.Error("Run() healthy = false, want true")
	}
	want := []obs.ImageMetric{
		{Registry: "dockerhub", Owner: "owner", Repo: "app", Pulls: 42, Tags: 2},
		{Registry: "ghcr", Owner: "owner", Repo: "pkg", Pulls: 500, Tags: 0},
	}
	if len(images) != len(want) {
		t.Fatalf("images = %+v, want %+v", images, want)
	}
	for i := range want {
		if images[i] != want[i] {
			t.Errorf("images[%d] = %+v, want %+v", i, images[i], want[i])
		}
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
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR() // would fail if invoked (healthy stays false)

	_, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		Now:     time.Now,
		RefsFor: func(name string) []registry.RepoRef {
			if name == registry.DockerHub.String() {
				return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
			}
			return nil
		},
	})
	if !healthy {
		t.Error("Run() healthy = false, want true (ghcr was skipped, not failed)")
	}
	if gh.lastRefs != nil {
		t.Errorf("gh should not have been invoked (empty refs), lastRefs = %+v", gh.lastRefs)
	}
}

func TestRun_no_sources_configured_returns_unhealthy(t *testing.T) {
	ctx := t.Context()
	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{},
		Logger:  testsupport.QuietLogger(),
	})
	if healthy {
		t.Error("Run() healthy = true, want false for empty-cycle path")
	}
	if len(images) != 0 {
		t.Errorf("images = %+v, want empty", images)
	}
}

func TestRun_all_sources_empty_entries_returns_unhealthy(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.attempted = 3 // healthy stays false
	gh := newFakeGHCR()
	gh.attempted = 2

	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "x", Repo: "y"}}
		},
	})
	if healthy {
		t.Error("Run() healthy = true, want false when all collections failed")
	}
	if len(images) != 0 {
		t.Errorf("images = %+v, want empty", images)
	}
}

func TestRun_partial_success_returns_records_with_degraded_flag(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.healthy = true

	// GHCR has refs but returns no entries and flags unhealthy. The
	// cycle still serves data because DockerHub produced some.
	gh := newFakeGHCR()
	gh.attempted = 2

	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "o", Repo: "r"}}
		},
	})
	if healthy {
		t.Error("Run() healthy = true, want false (GHCR flagged unhealthy)")
	}
	if len(images) != 1 {
		t.Errorf("images = %+v, want the one DockerHub record served", images)
	}
}

func TestRun_unknown_source_drops_entries_and_keeps_health(t *testing.T) {
	ctx := t.Context()
	unknown := &fakeSource{
		source:    registry.Unknown,
		entries:   []registry.Entry{{Owner: "x", Repo: "y", Pulls: 1}},
		attempted: 1, healthy: true,
	}
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "o", Repo: "a", Pulls: 2}}
	dh.attempted = 1
	dh.healthy = true

	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{unknown, dh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if !healthy {
		t.Error("Run() healthy = false, want true (dockerhub was fine)")
	}
	// The unknown source has no registry label to stamp; its entries
	// must not reach the metric records.
	if len(images) != 1 || images[0].Registry != "dockerhub" || images[0].Repo != "a" {
		t.Errorf("images = %+v, want only the dockerhub record", images)
	}
}

func TestRun_entries_with_empty_repo_are_dropped(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{
		{Owner: "o", Repo: "", Pulls: 9}, // dropped
		{Owner: "o", Repo: "good", Pulls: 1},
	}
	dh.attempted = 2
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{
		{Owner: "o", Repo: "", Pulls: 7}, // dropped
		{Owner: "o", Repo: "p", Pulls: 3},
	}
	gh.attempted = 2
	gh.healthy = true

	images, _ := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "x"}} },
	})
	if len(images) != 2 {
		t.Fatalf("images = %+v, want 2 records (empty-repo entries dropped)", images)
	}
	if images[0].Repo != "good" || images[1].Repo != "p" {
		t.Errorf("images = %+v, want repos good and p", images)
	}
}

func TestRun_nil_refsfor_skips_all_sources(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub() // healthy stays false
	_, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh},
		Logger:  testsupport.QuietLogger(),
		// RefsFor: nil
	})
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
	dh.entries = []registry.Entry{{Owner: "o", Repo: "a", Pulls: 1}}
	dh.attempted = 1
	dh.healthy = true

	images, healthy := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh},
		RefsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if !healthy || len(images) != 1 {
		t.Fatalf("Run() = (%+v, %v), want one record and healthy", images, healthy)
	}
}

// captureLogs returns a logger that writes records (Warn and above) into
// the returned buffer, so a test can assert whether a specific log line
// was emitted. Run surfaces its severe-degradation signal only as a log
// line, so capturing it is the only observable.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf
}

// TestRun_severe_degradation_warn_condition pins the truth table of the
// orchestrator's severe-degradation warn: it fires only for an unhealthy
// DockerHub source that still returned at least one entry. A DockerHub
// source with zero entries, and any non-DockerHub source, must stay
// silent even when unhealthy.
func TestRun_severe_degradation_warn_condition(t *testing.T) {
	const degradedMsg = "docker hub collection severely degraded"
	dhEntries := []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	ghEntries := []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 1}}

	tests := []struct {
		name     string
		entries  []registry.Entry
		source   registry.ID
		wantWarn bool
	}{
		{name: "dockerhub_unhealthy_with_entries_warns", source: registry.DockerHub, entries: dhEntries, wantWarn: true},
		{name: "dockerhub_unhealthy_zero_entries_silent", source: registry.DockerHub, entries: nil, wantWarn: false},
		{name: "ghcr_unhealthy_with_entries_silent", source: registry.GHCR, entries: ghEntries, wantWarn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{
				source:    tt.source,
				entries:   tt.entries,
				attempted: len(tt.entries),
				healthy:   false,
			}
			logger, buf := captureLogs()

			collect.Run(t.Context(), collect.Options{
				Sources: []collect.Source{src},
				Logger:  logger,
				RefsFor: func(string) []registry.RepoRef {
					return []registry.RepoRef{{Owner: "owner", Repo: "x"}}
				},
			})

			if got := strings.Contains(buf.String(), degradedMsg); got != tt.wantWarn {
				t.Errorf("Run() severe-degradation warn emitted = %v, want %v (source=%s, entries=%d)",
					got, tt.wantWarn, tt.source, len(tt.entries))
			}
		})
	}
}
