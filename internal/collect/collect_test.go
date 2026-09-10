package collect_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	// orchestrator passes each work item's refs through unchanged.
	entries       []registry.Entry
	lastRefs      []registry.RepoRef
	fetched       int
	attempted     int
	source        registry.ID
	listingFailed bool
	// cancel, when set, cancels the cycle's context from inside Collect, so a
	// test can stage a source that was invoked and then interrupted.
	cancel context.CancelFunc
}

func (f *fakeSource) Source() registry.ID { return f.source }

func (f *fakeSource) Collect(
	_ context.Context,
	refs []registry.RepoRef,
) registry.Collection {
	f.lastRefs = refs
	if f.cancel != nil {
		f.cancel()
	}
	return registry.Collection{
		Entries:       f.entries,
		Fetched:       f.fetched,
		Attempted:     f.attempted,
		ListingFailed: f.listingFailed,
	}
}

// newFakeDockerHub and newFakeGHCR build fakeSources whose Source() matches
// the real dockerhub/ghcr clients, so the orchestrator routes them the same way.
func newFakeDockerHub() *fakeSource {
	return &fakeSource{source: registry.DockerHub}
}

func newFakeGHCR() *fakeSource {
	return &fakeSource{source: registry.GHCR}
}

func workItem(source collect.Source, refs ...registry.RepoRef) collect.SourceRefs {
	return collect.SourceRefs{Source: source, Refs: refs}
}

func TestRun_healthy_returns_stamped_records_for_both_registries(t *testing.T) {
	ctx := t.Context()
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{
		{Owner: "owner", Repo: "app", Pulls: 42},
	}
	dh.fetched = 1
	dh.attempted = 1
	dh.listingFailed = false

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 500}}
	gh.fetched = 1
	gh.attempted = 1
	gh.listingFailed = false

	images := collect.Run(ctx, collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			{Source: dh, Refs: []registry.RepoRef{{Owner: "owner", Repo: "app"}}},
			{Source: gh, Refs: []registry.RepoRef{{Owner: "owner", Repo: "pkg"}}},
		},
		Logger: testsupport.QuietLogger(),
	})
	want := []obs.ImageMetric{
		{Registry: registry.DockerHub, Owner: "owner", Repo: "app", Pulls: 42},
		{Registry: registry.GHCR, Owner: "owner", Repo: "pkg", Pulls: 500},
	}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("images = %+v, want %+v", images, want)
	}
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
	src.fetched = 1
	src.attempted = 1

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(src, registry.RepoRef{Owner: "owner", Repo: "app"}),
		},
		Logger: logger,
	})

	logs := buf.String()
	started := strings.Index(logs, `level=INFO msg="starting collection"`)
	completed := strings.Index(logs, `level=INFO msg="collection complete" images=1 duration=`)
	if started < 0 || completed < 0 || started >= completed {
		t.Errorf("Run() lifecycle logs = %q, want ordered start and completion with images=1 and duration", logs)
	}
}

// TestRun_records_counters_for_invoked_sources pins the counters the shipped
// RegistryStatsCollectStalled and RegistryStatsSourceDegraded rules read: an
// invoked healthy source moves only collects_total, an invoked unhealthy one
// moves collect_errors_total too.
func TestRun_records_counters_for_invoked_sources(t *testing.T) {
	m := obs.New()

	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	dh.fetched = 1
	dh.attempted = 1
	dh.listingFailed = false

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 2}}
	gh.fetched = 1
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
		Sources: []collect.SourceRefs{
			workItem(dh, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			workItem(gh, registry.RepoRef{Owner: "owner", Repo: "configured"}),
		},
		Logger: testsupport.QuietLogger(),
	})

	body := scrape()
	for _, want := range []string{
		`registrystats_collects_total{source="dockerhub"} 1`,
		`registrystats_collects_total{source="ghcr"} 1`,
		`registrystats_collect_errors_total{source="ghcr"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Run() scrape missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `registrystats_collect_errors_total{source="dockerhub"}`) {
		t.Errorf("Run() scrape reports healthy dockerhub as an error:\n%s", body)
	}
}

func TestRun_zero_attempt_source_advances_cycle_counter(t *testing.T) {
	m := obs.New()
	m.MintCollectSources([]registry.ID{registry.DockerHub})
	src := newFakeDockerHub()

	collect.Run(t.Context(), collect.Options{
		Metrics: m,
		Sources: []collect.SourceRefs{
			workItem(src, registry.RepoRef{Owner: "owner", Repo: "*"}),
		},
		Logger: testsupport.QuietLogger(),
	})

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `registrystats_collects_total{source="dockerhub"} 1`) {
		t.Errorf("Run(zero-attempt source) metrics missing one collect:\n%s", body)
	}
	if !strings.Contains(body, `registrystats_collect_errors_total{source="dockerhub"} 0`) {
		t.Errorf("Run(zero-attempt source) metrics did not retain a zero error count:\n%s", body)
	}
}

func TestRun_derivesSourceHealthFromResults(t *testing.T) {
	tests := []struct {
		name          string
		entries       int
		fetched       int
		attempted     int
		listingFailed bool
		wantHealthy   bool
	}{
		{name: "exactly_half_failed", entries: 1, fetched: 1, attempted: 2, wantHealthy: true},
		{name: "wildcard_rows_do_not_absorb_failures", entries: 25, fetched: 1, attempted: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeDockerHub()
			src.fetched = tt.fetched
			src.attempted = tt.attempted
			src.listingFailed = tt.listingFailed
			for range tt.entries {
				src.entries = append(src.entries, registry.Entry{Owner: "owner", Repo: "repo"})
			}

			buf := &bytes.Buffer{}
			logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			collect.Run(t.Context(), collect.Options{
				Metrics: obs.New(),
				Sources: []collect.SourceRefs{
					workItem(src, registry.RepoRef{Owner: "owner", Repo: "configured"}),
				},
				Logger: logger,
			})

			unhealthy := strings.Contains(buf.String(), `msg="source reported unhealthy"`)
			if unhealthy == tt.wantHealthy {
				t.Errorf("Run(entries=%d, fetched=%d, attempted=%d, listingFailed=%v) healthy = %v, want %v; logs:\n%s",
					tt.entries, tt.fetched, tt.attempted, tt.listingFailed, !unhealthy, tt.wantHealthy, buf.String())
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
		sources  []collect.SourceRefs
		cancel   bool
		want     string
		unwanted []string
	}{
		{
			name: "no_sources_reports_configuration",
			want: `level=WARN msg="no repos configured"`,
			unwanted: []string{
				`msg="no images collected, at least one source failed"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name: "all_sources_failed_reports_failure",
			sources: []collect.SourceRefs{
				workItem(degraded, registry.RepoRef{Owner: "o", Repo: "r"}),
			},
			want: `level=ERROR msg="no images collected, at least one source failed"`,
			unwanted: []string{
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name: "mixed_health_without_images_reports_failure",
			sources: []collect.SourceRefs{
				workItem(emptyButHealthy, registry.RepoRef{Owner: "o", Repo: "r"}),
				workItem(degraded, registry.RepoRef{Owner: "o", Repo: "r"}),
			},
			want: `level=ERROR msg="no images collected, at least one source failed"`,
			unwanted: []string{
				`msg="no repos configured"`,
				`msg="no images found for the configured refs"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name: "healthy_but_empty_reports_nothing_to_poll",
			sources: []collect.SourceRefs{
				workItem(emptyButHealthy, registry.RepoRef{Owner: "o", Repo: "r"}),
			},
			want: `level=WARN msg="no images found for the configured refs"`,
			unwanted: []string{
				`msg="no images collected, at least one source failed"`,
				`msg="no repos configured"`,
				`msg="collection interrupted"`,
			},
		},
		{
			name: "cancelled_cycle_reports_interruption_before_failure",
			sources: []collect.SourceRefs{
				workItem(degraded, registry.RepoRef{Owner: "o", Repo: "r"}),
			},
			cancel: true,
			want:   `level=WARN msg="collection interrupted"`,
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
				sources = []collect.SourceRefs{
					workItem(cancelling, registry.RepoRef{Owner: "o", Repo: "r"}),
				}
			}

			collect.Run(ctx, collect.Options{
				Metrics: obs.New(),
				Sources: sources,
				Logger:  logger,
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
// collects_total sample, while the dead context stops the loop so no later
// source mints one for a cycle it never performed.
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
		Sources: []collect.SourceRefs{
			workItem(dh, registry.RepoRef{Owner: "o", Repo: "r"}),
			workItem(gh, registry.RepoRef{Owner: "o", Repo: "r"}),
		},
		Logger: logger,
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
	dh.fetched = 1
	dh.attempted = 1
	dh.cancel = cancel

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	images := collect.Run(ctx, collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(dh, registry.RepoRef{Owner: "owner", Repo: "app"}),
		},
		Logger: logger,
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

func TestRun_degraded_source_does_not_stop_later_source(t *testing.T) {
	failed := newFakeDockerHub()
	failed.attempted = 1

	serving := newFakeGHCR()
	serving.entries = []registry.Entry{{Owner: "owner", Repo: "pkg", Pulls: 9}}
	serving.fetched = 1
	serving.attempted = 1

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	images := collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(failed, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			workItem(serving, registry.RepoRef{Owner: "owner", Repo: "configured"}),
		},
		Logger: logger,
	})

	want := []obs.ImageMetric{{Registry: registry.GHCR, Owner: "owner", Repo: "pkg", Pulls: 9}}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("Run(unhealthy first, healthy second) images = %+v, want %+v", images, want)
	}
	if logs := buf.String(); !strings.Contains(logs, `msg="partial collection failure" images=1`) {
		t.Errorf("Run(unhealthy first, healthy second) logs = %q, want partial collection", logs)
	}
}

func TestRun_logs_partial_collection_failure_only_for_degraded_cycle(t *testing.T) {
	t.Run("degraded_cycle", func(t *testing.T) {
		serving := newFakeDockerHub()
		serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
		serving.fetched = 1
		serving.attempted = 1

		failed := newFakeGHCR()
		failed.attempted = 1

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		collect.Run(t.Context(), collect.Options{
			Metrics: obs.New(),
			Sources: []collect.SourceRefs{
				workItem(serving, registry.RepoRef{Owner: "owner", Repo: "configured"}),
				workItem(failed, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			},
			Logger: logger,
		})

		logs := buf.String()
		partial := strings.Index(logs, `level=WARN msg="partial collection failure" images=1`)
		completed := strings.Index(logs, `level=INFO msg="collection complete" images=1 duration=`)
		if partial < 0 || completed < 0 || partial >= completed {
			t.Errorf("Run() degraded serving-cycle logs = %q, want partial failure followed by completion", logs)
		}
	})

	t.Run("healthy_cycle", func(t *testing.T) {
		serving := newFakeDockerHub()
		serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
		serving.fetched = 1
		serving.attempted = 1

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		collect.Run(t.Context(), collect.Options{
			Metrics: obs.New(),
			Sources: []collect.SourceRefs{
				workItem(serving, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			},
			Logger: logger,
		})

		if logs := buf.String(); strings.Contains(logs, `msg="partial collection failure"`) {
			t.Errorf("Run() healthy serving cycle logged a partial failure; logs:\n%s", logs)
		}
	})
}

func TestRun_healthy_short_population_is_an_absolute_complete_cycle(t *testing.T) {
	m := obs.New()
	m.MintCollectSources([]registry.ID{registry.DockerHub, registry.GHCR})

	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "stable", Pulls: 10}}
	dh.fetched = 1
	dh.attempted = 1

	gh := newFakeGHCR()
	gh.entries = []registry.Entry{
		{Owner: "owner", Repo: "kept", Pulls: 20},
		{Owner: "owner", Repo: "gone", Pulls: 30},
	}
	gh.fetched = 2
	gh.attempted = 2

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	run := func() []obs.ImageMetric {
		return collect.Run(t.Context(), collect.Options{
			Metrics: m,
			Sources: []collect.SourceRefs{
				workItem(dh, registry.RepoRef{Owner: "owner", Repo: "configured"}),
				workItem(gh, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			},
			Logger: logger,
		})
	}

	if first := run(); len(first) != 3 {
		t.Fatalf("Run(first complete cycle) returned %d images, want 3", len(first))
	}

	gh.entries = []registry.Entry{{Owner: "owner", Repo: "kept", Pulls: 21}}
	gh.fetched = 1
	gh.attempted = 1
	buf.Reset()
	images := run()

	want := []obs.ImageMetric{
		{Registry: registry.DockerHub, Owner: "owner", Repo: "stable", Pulls: 10},
		{Registry: registry.GHCR, Owner: "owner", Repo: "kept", Pulls: 21},
	}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("Run(short healthy population) images = %+v, want %+v", images, want)
	}
	logs := buf.String()
	if !strings.Contains(logs, `level=INFO msg="collection complete" images=2`) {
		t.Errorf("Run(short healthy population) logs = %q, want complete cycle with two images", logs)
	}
	if strings.Contains(logs, `msg="partial collection failure"`) ||
		strings.Contains(logs, `msg="source reported unhealthy"`) ||
		strings.Contains(logs, `msg="no images found for the configured refs"`) {
		t.Errorf("Run(short healthy population) logs = %q, want no degradation record", logs)
	}

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	if body := w.Body.String(); !strings.Contains(body, `registrystats_collect_errors_total{source="ghcr"} 0`) {
		t.Errorf("Run(short healthy population) metrics missing zero GHCR error count:\n%s", body)
	}
}

func TestRun_degraded_cycle_names_failing_source(t *testing.T) {
	serving := newFakeDockerHub()
	serving.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	serving.fetched = 1
	serving.attempted = 1

	failed := newFakeGHCR()
	failed.attempted = 1

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(serving, registry.RepoRef{Owner: "owner", Repo: "configured"}),
			workItem(failed, registry.RepoRef{Owner: "owner", Repo: "configured"}),
		},
		Logger: logger,
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

func TestRun_degraded_serving_cycle_stays_below_error_level(t *testing.T) {
	src := newFakeDockerHub()
	src.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 1}}
	src.fetched = 1
	src.attempted = 1
	src.listingFailed = true

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(src, registry.RepoRef{Owner: "owner", Repo: "app"}),
		},
		Logger: logger,
	})

	logs := buf.String()
	if !strings.Contains(logs, `level=WARN msg="source reported unhealthy"`) {
		t.Errorf("Run(degraded serving source) logs = %q, want WARN source-health record", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("Run(degraded serving source) logs = %q, want no ERROR record", logs)
	}
}

// TestRun_unhealthy_source_still_serves_entries pins that an unhealthy
// source's data survives the stamp.
func TestRun_unhealthy_source_still_serves_entries(t *testing.T) {
	dh := newFakeDockerHub()
	dh.entries = []registry.Entry{{Owner: "owner", Repo: "app", Pulls: 7}}
	dh.fetched = 1
	dh.attempted = 2
	dh.listingFailed = true

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	images := collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Sources: []collect.SourceRefs{
			workItem(dh,
				registry.RepoRef{Owner: "owner", Repo: "app"},
				registry.RepoRef{Owner: "owner", Repo: "gone"}),
		},
		Logger: logger,
	})

	want := obs.ImageMetric{Registry: registry.DockerHub, Owner: "owner", Repo: "app", Pulls: 7}
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

func TestRun_unhealthy_source_logs_listing_failure_cause(t *testing.T) {
	tests := []struct {
		name          string
		fetched       int
		attempted     int
		listingFailed bool
		want          string
	}{
		{name: "wholesale_listing_failure", fetched: 1, attempted: 1, listingFailed: true, want: "listing_failed=true"},
		{name: "majority_fetch_failure", attempted: 1, want: "listing_failed=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newFakeDockerHub()
			src.fetched = tt.fetched
			src.attempted = tt.attempted
			src.listingFailed = tt.listingFailed

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			collect.Run(t.Context(), collect.Options{
				Metrics: obs.New(),
				Sources: []collect.SourceRefs{
					workItem(src, registry.RepoRef{Owner: "owner", Repo: "configured"}),
				},
				Logger: logger,
			})

			logs := buf.String()
			if !strings.Contains(logs, `msg="source reported unhealthy"`) || !strings.Contains(logs, tt.want) {
				t.Errorf("Run(%s) logs = %q, want unhealthy record with %s", tt.name, logs, tt.want)
			}
		})
	}
}

func TestRun_noReposLogMatchesConfigRejectedRule(t *testing.T) {
	const (
		configRejected = "- alert: RegistryStatsConfigRejected"
		nextAlert      = "- alert: RegistryStatsError"
		matchedMessage = "no repos configured"
	)

	rules, err := os.ReadFile("../../alerts/logql.yaml")
	if err != nil {
		t.Fatalf("Setup: read alerts/logql.yaml: %v", err)
	}
	_, rule, ok := strings.Cut(string(rules), configRejected)
	if !ok {
		t.Fatalf("alerts/logql.yaml is missing %q", configRejected)
	}
	rule, _, ok = strings.Cut(rule, nextAlert)
	if !ok {
		t.Fatalf("alerts/logql.yaml is missing %q after %q", nextAlert, configRejected)
	}
	if !strings.Contains(rule, matchedMessage) {
		t.Errorf("%s rule does not match %q", configRejected, matchedMessage)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	collect.Run(t.Context(), collect.Options{
		Metrics: obs.New(),
		Logger:  logger,
	})

	want := `level=WARN msg="` + matchedMessage + `"`
	if logs := buf.String(); !strings.Contains(logs, want) {
		t.Errorf("Run(no repos) logs = %q, want record matching %q", logs, want)
	}
}
