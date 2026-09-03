package collect_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	// orchestrator plumbs RefsFor(Source()) through correctly.
	entries       []registry.Entry
	lastRefs      []registry.RepoRef
	attempted     int
	source        registry.ID
	listingFailed bool
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
	return f.entries, f.attempted, f.listingFailed
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
		{Owner: "owner", Repo: "app", Pulls: 42},
	}
	dh.attempted = 1
	dh.listingFailed = false

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 500}}
	gh.attempted = 1
	gh.listingFailed = false

	images := collect.Run(ctx, collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(source registry.ID) []registry.RepoRef {
			switch source {
			case registry.DockerHub:
				return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
			case registry.GHCR:
				return []registry.RepoRef{{Owner: "owner", Repo: "pkg"}}
			}
			return nil
		},
	})
	want := []obs.ImageMetric{
		{Registry: "dockerhub", Owner: "owner", Repo: "app", Pulls: 42},
		{Registry: "ghcr", Owner: "owner", Repo: "pkg", Pulls: 500},
	}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("images = %+v, want %+v", images, want)
	}
	// RefsFor plumbed through correctly.
	if len(dh.lastRefs) != 1 || dh.lastRefs[0].Repo != "app" {
		t.Errorf("dh.lastRefs = %+v, want [{owner app}]", dh.lastRefs)
	}
	if len(gh.lastRefs) != 1 || gh.lastRefs[0].Repo != "pkg" {
		t.Errorf("gh.lastRefs = %+v, want [{owner pkg}]", gh.lastRefs)
	}
}

func TestRun_success_logs_lifecycle(t *testing.T) {
	src := newFakeDockerHub()
	src.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	src.attempted = 1

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{src},
		Logger:  logger,
		RefsFor: func(registry.ID) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
		},
	})

	logs := buf.String()
	started := strings.Index(logs, `level=INFO msg="starting collection"`)
	completed := strings.Index(logs, `level=INFO msg="collection complete" images=1 duration=`)
	if started < 0 || completed < 0 || started >= completed {
		t.Errorf("Run() lifecycle logs = %q, want ordered start and completion with images=1 and duration", logs)
	}
}

// TestRun_records_counters_only_for_invoked_sources pins the denominator the
// shipped RegistryStatsCollectStalled and RegistryStatsSourceDegraded rules
// read: a source with no configured refs moves neither counter, and an
// invoked unhealthy source moves both.
func TestRun_records_counters_only_for_invoked_sources(t *testing.T) {
	m := obs.New()

	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.listingFailed = false

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 2}}
	gh.attempted = 1
	gh.listingFailed = true

	scrape := func() string {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		w := httptest.NewRecorder()
		m.Handler()(w, r)
		return w.Body.String()
	}

	collect.Run(t.Context(), collect.Options{
		Metrics: m,
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(source registry.ID) []registry.RepoRef {
			if source == registry.DockerHub {
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
		Metrics: m,
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(registry.ID) []registry.RepoRef {
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

func TestRun_derivesSourceHealthFromResults(t *testing.T) {
	tests := []struct {
		name          string
		entries       int
		attempted     int
		listingFailed bool
		wantHealthy   bool
	}{
		{name: "zero_attempts", wantHealthy: true},
		{name: "all_failed", attempted: 3},
		{name: "majority_failed", entries: 1, attempted: 3},
		{name: "exactly_half_failed", entries: 1, attempted: 2, wantHealthy: true},
		{name: "all_succeeded", entries: 2, attempted: 2, wantHealthy: true},
		{name: "listing_failed", entries: 2, attempted: 2, listingFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeDockerHub()
			src.attempted = tt.attempted
			src.listingFailed = tt.listingFailed
			for range tt.entries {
				src.entries = append(src.entries, registry.Entry{Owner: "owner", Repo: "repo"})
			}

			buf := &bytes.Buffer{}
			logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			collect.Run(t.Context(), collect.Options{
				Metrics: obs.New(),
				Sources: []collect.Source{src},
				Logger:  logger,
				RefsFor: func(registry.ID) []registry.RepoRef {
					return []registry.RepoRef{{Owner: "owner", Repo: "configured"}}
				},
			})

			unhealthy := strings.Contains(buf.String(), `msg="source reported unhealthy"`)
			if unhealthy == tt.wantHealthy {
				t.Errorf("Run(entries=%d, attempted=%d, listingFailed=%v) healthy = %v, want %v; logs:\n%s",
					tt.entries, tt.attempted, tt.listingFailed, !unhealthy, tt.wantHealthy, buf.String())
			}
		})
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
	emptyButHealthy.listingFailed = false

	tests := []struct {
		name     string
		sources  []collect.Source
		refsFor  func(registry.ID) []registry.RepoRef
		cancel   bool
		want     string
		unwanted []string
	}{
		{
			name:    "no_configured_refs_reports_configuration",
			sources: []collect.Source{degraded},
			refsFor: func(registry.ID) []registry.RepoRef { return nil },
			want:    `level=WARN msg="no repos configured"`,
			unwanted: []string{
				`msg="no images collected, at least one source failed"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "all_sources_failed_reports_failure",
			sources: []collect.Source{degraded},
			refsFor: func(registry.ID) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			want:    `level=ERROR msg="no images collected, at least one source failed"`,
			unwanted: []string{
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "mixed_health_without_images_reports_failure",
			sources: []collect.Source{emptyButHealthy, degraded},
			refsFor: func(registry.ID) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			want:    `level=ERROR msg="no images collected, at least one source failed"`,
			unwanted: []string{
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "healthy_but_empty_reports_nothing_to_poll",
			sources: []collect.Source{emptyButHealthy},
			refsFor: func(registry.ID) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			want:    `level=WARN msg="no images found for the configured refs"`,
			unwanted: []string{
				`msg="no images collected, at least one source failed"`,
				`msg="no repos configured"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name:    "cancelled_cycle_reports_interruption_before_failure",
			sources: []collect.Source{degraded},
			refsFor: func(registry.ID) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
			cancel:  true,
			want:    `level=WARN msg="collection interrupted"`,
			unwanted: []string{
				`msg="no images collected, at least one source failed"`,
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
				Metrics: obs.New(),
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
// says it was degraded, so no shipped rule fires on a shutdown. The
// accounting is asymmetric: the invoked source still mints its own
// collects_total sample, the RegistryStatsCollectStalled denominator, while
// the dead context stops the loop so no later source mints one for a cycle
// it never performed.
func TestRun_cancelled_source_moves_no_error_counter(t *testing.T) {
	m := obs.New()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dh := newFakeDockerHub()
	dh.attempted = 1
	dh.cancel = cancel
	gh := newFakeGHCR()
	gh.listingFailed = false

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	collect.Run(ctx, collect.Options{
		Metrics: m,
		Sources: []collect.Source{dh, gh},
		Logger:  logger,
		RefsFor: func(registry.ID) []registry.RepoRef { return []registry.RepoRef{{Owner: "o", Repo: "r"}} },
	})

	if gh.lastRefs != nil {
		t.Errorf("the second source was invoked on a dead context, lastRefs = %+v", gh.lastRefs)
	}
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `registrystats_collects_total{source="dockerhub"} 1`) {
		t.Errorf("a cancelled invoked source did not mint a collects_total sample:\n%s", body)
	}
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

func TestRun_cancelled_source_with_entries_reports_interruption(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.cancel = cancel

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	images := collect.Run(ctx, collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{dh},
		Logger:  logger,
		RefsFor: func(registry.ID) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "owner", Repo: "app"}}
		},
	})

	if len(images) != 1 {
		t.Errorf("Run(cancelled non-empty source) returned %d images, want 1", len(images))
	}
	logs := buf.String()
	if !strings.Contains(logs, `level=WARN msg="collection interrupted"`) {
		t.Errorf("Run(cancelled non-empty source) logs = %q, want interruption record", logs)
	}
	hasInterruptedCount := strings.Contains(logs, "images=1") || strings.Contains(logs, "collected=1")
	if !hasInterruptedCount {
		t.Errorf("Run(cancelled non-empty source) logs = %q, want interruption count 1", logs)
	}
	if strings.Contains(logs, `msg="partial collection failure`) {
		t.Errorf("Run(cancelled non-empty source) logs = %q, do not want partial-failure record", logs)
	}
	if strings.Contains(logs, `msg="collection complete"`) {
		t.Errorf("Run(cancelled non-empty source) logs = %q, do not want completion record", logs)
	}
}

func TestRun_partial_success_returns_records_with_degraded_flag(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.attempted = 1
	dh.listingFailed = false

	// GHCR has refs but returns no entries and flags unhealthy. The
	// cycle still serves data because DockerHub produced some.
	gh := newFakeGHCR()
	gh.attempted = 2

	images := collect.Run(ctx, collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{dh, gh},
		Logger:  testsupport.QuietLogger(),
		RefsFor: func(source registry.ID) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "o", Repo: "r"}}
		},
	})
	if len(images) != 1 {
		t.Errorf("images = %+v, want the one DockerHub record served", images)
	}
}

func TestRun_logs_partial_collection_failure_only_for_degraded_cycle(t *testing.T) {
	t.Run("degraded_cycle", func(t *testing.T) {
		serving := newFakeDockerHub()
		serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
		serving.attempted = 1

		failed := newFakeGHCR()
		failed.attempted = 1

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		collect.Run(t.Context(), collect.Options{
			Metrics: obs.New(),
			Sources: []collect.Source{serving, failed},
			Logger:  logger,
			RefsFor: func(registry.ID) []registry.RepoRef {
				return []registry.RepoRef{{Owner: "owner", Repo: "configured"}}
			},
		})

		logs := buf.String()
		if !strings.Contains(logs, `level=WARN msg="partial collection failure" images=1`) {
			t.Errorf("Run() degraded serving cycle missing partial-failure record; logs:\n%s", logs)
		}
	})

	t.Run("healthy_cycle", func(t *testing.T) {
		serving := newFakeDockerHub()
		serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
		serving.attempted = 1

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		collect.Run(t.Context(), collect.Options{
			Metrics: obs.New(),
			Sources: []collect.Source{serving},
			Logger:  logger,
			RefsFor: func(registry.ID) []registry.RepoRef {
				return []registry.RepoRef{{Owner: "owner", Repo: "configured"}}
			},
		})

		if logs := buf.String(); strings.Contains(logs, `msg="partial collection failure"`) {
			t.Errorf("Run() healthy serving cycle logged a partial failure; logs:\n%s", logs)
		}
	})
}

func TestRun_degraded_cycle_names_failing_source(t *testing.T) {
	serving := newFakeDockerHub()
	serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	serving.attempted = 1

	failed := newFakeGHCR()
	failed.attempted = 1

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{serving, failed},
		Logger:  logger,
		RefsFor: func(registry.ID) []registry.RepoRef {
			return []registry.RepoRef{{Owner: "owner", Repo: "configured"}}
		},
	})

	logs := buf.String()
	unhealthy := strings.Index(logs, `msg="source reported unhealthy" source=ghcr`)
	partial := strings.Index(logs, `msg="partial collection failure"`)
	if unhealthy < 0 || partial < 0 || unhealthy >= partial {
		t.Errorf("Run() degraded-cycle logs = %q, want GHCR unhealthy record before partial-failure summary", logs)
	}
	if strings.Contains(logs, `msg="source reported unhealthy" source=dockerhub`) {
		t.Errorf("Run() degraded-cycle logs attribute failure to serving Docker Hub source: %q", logs)
	}
}

// TestRun_unhealthy_source_still_serves_entries pins that an unhealthy
// source's data survives the stamp.
func TestRun_unhealthy_source_still_serves_entries(t *testing.T) {
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 7}}
	dh.attempted = 2
	dh.listingFailed = true

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	images := collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.Source{dh},
		Logger:  logger,
		RefsFor: func(registry.ID) []registry.RepoRef {
			return []registry.RepoRef{
				{Owner: "owner", Repo: "app"},
				{Owner: "owner", Repo: "gone"},
			}
		},
	})

	want := obs.ImageMetric{Registry: "dockerhub", Owner: "owner", Repo: "app", Pulls: 7}
	if len(images) != 1 {
		t.Fatalf("images = %+v, want one record from the unhealthy source", images)
	}
	if !reflect.DeepEqual(images[0], want) {
		t.Errorf("images[0] = %+v, want %+v", images[0], want)
	}
	logs := buf.String()
	if !strings.Contains(logs, `msg="source reported unhealthy"`) ||
		!strings.Contains(logs, `succeeded=1`) || !strings.Contains(logs, `attempted=2`) {
		t.Errorf("Run() unhealthy source log missing succeeded=1 attempted=2; logs:\n%s", logs)
	}
}
