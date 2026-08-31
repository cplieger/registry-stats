package collect_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	// cancel, when set, cancels the cycle's context from inside Collect, so a
	// test can stage a source that was invoked and then interrupted.
	cancel context.CancelFunc
}

// Compile-time assertion: *fakeSource satisfies Source.
var _ collect.Source = (*fakeSource)(nil)

func (f *fakeSource) Source() registry.ID { return f.source }

func (f *fakeSource) Collect(
	_ context.Context,
	refs []registry.RepoRef,
) ([]registry.Entry, int, bool) {
	f.lastRefs = refs
	if f.cancel != nil {
		f.cancel()
	}
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

	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
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

	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []registry.RepoRef {
			if name == registry.DockerHub.String() {
				return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
			}
			return nil
		},
	})
	if len(images) != 1 {
		t.Errorf("images = %+v, want the one dockerhub record", images)
	}
	if gh.lastRefs != nil {
		t.Errorf("gh should not have been invoked (empty refs), lastRefs = %+v", gh.lastRefs)
	}
}

// TestRun_records_counters_only_for_invoked_sources pins the denominator the
// shipped RegistryStatsCollectStalled and RegistryStatsSourceDegraded rules
// read: a source with no configured refs moves neither counter, and an
// invoked unhealthy source moves both.
func TestRun_records_counters_only_for_invoked_sources(t *testing.T) {
	obs.CollectsTotal.Reset()
	obs.CollectErrors.Reset()
	t.Cleanup(func() {
		obs.CollectsTotal.Reset()
		obs.CollectErrors.Reset()
	})

	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.healthy = true

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 2}}
	gh.attempted = 1
	gh.healthy = false

	scrape := func() string {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()
		obs.Handler()(w, r)
		return w.Body.String()
	}

	collect.Run(t.Context(), collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []registry.RepoRef {
			if name == registry.DockerHub.String() {
				return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
			}
			return nil
		},
	})

	body := scrape()
	if !strings.Contains(body, `registrystats_collects_total{source="dockerhub"} 1`) {
		t.Errorf("Run() first scrape missing one dockerhub collect:\n%s", body)
	}
	if strings.Contains(body, `source="ghcr"`) {
		t.Errorf("Run() first scrape contains skipped ghcr source:\n%s", body)
	}

	collect.Run(t.Context(), collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "owner", Repo: "configured"}}
		},
	})

	body = scrape()
	for _, want := range []string{
		`registrystats_collects_total{source="dockerhub"} 2`,
		`registrystats_collects_total{source="ghcr"} 1`,
		`registrystats_collect_errors_total{source="ghcr"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Run() second scrape missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `registrystats_collect_errors_total{source="dockerhub"}`) {
		t.Errorf("Run() second scrape reports healthy dockerhub as an error:\n%s", body)
	}
}

// TestRun_empty_cycle_log_classifies_cause pins the terminal record for each
// cause of an empty cycle, which is the only observable the four states have
// (they share one return). Order is part of the contract: a cycle that is
// both cancelled and degraded reports the interruption, because that is the
// fact explaining the empty result.
func TestRun_empty_cycle_log_classifies_cause(t *testing.T) {
	degraded := newFakeDockerHub()
	degraded.attempted = 2 // entries stay empty, healthy stays false

	emptyButHealthy := newFakeDockerHub()
	emptyButHealthy.healthy = true

	tests := []struct {
		name     string
		sources  []collect.Source
		refsFor  func(string) []registry.RepoRef
		cancel   bool
		want     string
		unwanted []string
	}{
		{
			name:    "no_configured_refs_reports_configuration",
			sources: []collect.Source{degraded},
			want:    `level=WARN msg="no repos configured"`,
			unwanted: []string{
				`msg="all collections failed"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "all_sources_failed_reports_failure",
			sources: []collect.Source{degraded},
			refsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			want:    `level=ERROR msg="all collections failed"`,
			unwanted: []string{
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "healthy_but_empty_reports_nothing_to_poll",
			sources: []collect.Source{emptyButHealthy},
			refsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			want:    `level=WARN msg="no images found for the configured refs"`,
			unwanted: []string{
				`msg="all collections failed"`,
				`msg="no repos configured"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "cancelled_cycle_reports_interruption_before_failure",
			sources: []collect.Source{degraded},
			refsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			cancel:  true,
			want:    `level=WARN msg="collection interrupted"`,
			unwanted: []string{
				`msg="all collections failed"`,
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			ctx := t.Context()
			sources := tt.sources
			if tt.cancel {
				// The source cancels the cycle from inside Collect, so it IS
				// invoked and reports unhealthy: the switch is then reached
				// with both facts true and must report the interruption.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				cancelling := newFakeDockerHub()
				cancelling.attempted = 2
				cancelling.cancel = cancel
				sources = []collect.Source{cancelling}
			}

			collect.Run(ctx, collect.Options{
				Sources: sources,
				Logger:  logger,
				RefsFor: tt.refsFor,
			})

			logs := buf.String()
			if !strings.Contains(logs, tt.want) {
				t.Errorf("Run() empty cycle did not log %q; logs:\n%s", tt.want, logs)
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(logs, unwanted) {
					t.Errorf("Run() empty cycle also logged %q; logs:\n%s", unwanted, logs)
				}
			}
		})
	}
}

// TestRun_cancelled_source_moves_no_error_counter pins the neutrality a
// redeploy needs: a source cancelled mid-cycle reports unhealthy of its own
// return, but the orchestrator neither counts it as a collection error nor
// says it was degraded, so no shipped rule fires on a shutdown. The dead
// context also stops the loop, so no later source mints a collects_total
// sample for a cycle it never performed.
func TestRun_cancelled_source_moves_no_error_counter(t *testing.T) {
	obs.CollectsTotal.Reset()
	obs.CollectErrors.Reset()
	t.Cleanup(func() {
		obs.CollectsTotal.Reset()
		obs.CollectErrors.Reset()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dh := newFakeDockerHub()
	dh.attempted = 1
	dh.cancel = cancel
	gh := newFakeGHCR()
	gh.healthy = true

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  logger,
		RefsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
	})

	if gh.lastRefs != nil {
		t.Errorf("the second source was invoked on a dead context, lastRefs = %+v", gh.lastRefs)
	}
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	obs.Handler()(w, r)
	body := w.Body.String()
	if strings.Contains(body, `registrystats_collect_errors_total{source="dockerhub"} 1`) {
		t.Errorf("a cancelled cycle moved collect_errors_total:\n%s", body)
	}
	if strings.Contains(body, `registrystats_collects_total{source="ghcr"}`) {
		t.Errorf("a source skipped on a dead context still minted a collects_total sample:\n%s", body)
	}
	if logs := buf.String(); strings.Contains(logs, `msg="source reported unhealthy"`) {
		t.Errorf("a cancelled cycle reported the source unhealthy; logs:\n%s", logs)
	}
}

func TestRun_no_sources_configured_returns_no_images(t *testing.T) {
	ctx := t.Context()
	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{},
		Logger:  testsupport.QuietLogger(),
	})
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

	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(name string) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "o", Repo: "r"}}
		},
	})
	if len(images) != 1 {
		t.Errorf("images = %+v, want the one DockerHub record served", images)
	}
}

// TestRun_unhealthy_source_still_serves_entries pins that an unhealthy
// source's data survives the stamp: main sends every returned image to
// obs.SetImage and derives marker health from len(images), so gating the
// stamp on the source verdict would silently drop the only data collected.
func TestRun_unhealthy_source_still_serves_entries(t *testing.T) {
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 7, TagCount: 3}}
	dh.attempted = 2 // one of two refs failed, so the source reports unhealthy

	images := collect.Run(t.Context(), collect.Options{
		Sources: []collect.Source{dh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(string) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "owner", Repo: "app"}, {Owner: "owner", Repo: "gone"}}
		},
	})

	want := obs.ImageMetric{Registry: "dockerhub", Owner: "owner", Repo: "app", Pulls: 7, Tags: 3}
	if len(images) != 1 {
		t.Fatalf("images = %+v, want one record from the unhealthy source", images)
	}
	if images[0] != want {
		t.Errorf("images[0] = %+v, want %+v", images[0], want)
	}
}

func TestRun_nil_refsfor_skips_all_sources(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub() // healthy stays false
	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh},
		Logger:  testsupport.QuietLogger(),
		// RefsFor: nil
	})
	if len(images) != 0 {
		t.Errorf("images = %+v, want none (every source skipped)", images)
	}
	if dh.lastRefs != nil {
		t.Errorf("dh.lastRefs = %+v, want nil (skipped)", dh.lastRefs)
	}
}

func TestRun_defaults_logger(t *testing.T) {
	// Verify Run does not panic when Logger is nil; it should fall back to
	// slog.Default().
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "o", Repo: "a", Pulls: 1}}
	dh.attempted = 1
	dh.healthy = true

	images := collect.Run(ctx, collect.Options{
		Sources: []collect.Source{dh},
		RefsFor: func(string) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "a"}} },
	})
	if len(images) != 1 {
		t.Fatalf("Run() = %+v, want one record", images)
	}
}
