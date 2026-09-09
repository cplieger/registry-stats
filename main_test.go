package main

// The tests below exercise only the behavior main.go owns that isn't
// already covered by a direct internal/* package test.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/registry-stats/v2/internal/collect"
	"github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/webhttp/v2"
)

// TestRegistryClient_refusesOffAllowlistRedirect pins the SSRF
// containment the readers' docs attribute to this client: a redirect to
// a host outside the Docker/GitHub allowlist is refused rather than
// followed.
func TestRegistryClient_refusesOffAllowlistRedirect(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			targetHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
	defer target.Close()

	src := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer src.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, src.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := registryClient().Do(req)
	if err == nil {
		resp.Body.Close()
		t.Errorf("registryClient() followed a redirect to %s; want it refused", target.URL)
	}
	if got := targetHits.Load(); got != 0 {
		t.Errorf("off-allowlist redirect target was reached %d time(s), want 0", got)
	}
}

func TestCollectLoop_modes(t *testing.T) {
	t.Run("one_shot", func(t *testing.T) {
		calls := 0
		collectLoop(t.Context(), 0, func(context.Context) { calls++ })
		if calls != 1 {
			t.Errorf("collectLoop(interval=0) calls = %d, want 1", calls)
		}
	})

	t.Run("scheduled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			var calls atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				collectLoop(ctx, time.Hour, func(context.Context) { calls.Add(1) })
			}()

			synctest.Wait()
			if got := calls.Load(); got != 1 {
				t.Errorf("collectLoop(interval=1h) calls before the first tick = %d, want 1", got)
			}

			time.Sleep(time.Hour)
			synctest.Wait()
			if got := calls.Load(); got != 2 {
				t.Errorf("collectLoop(interval=1h) calls after one interval = %d, want 2", got)
			}

			cancel()
			<-done
		})
	})
}

func TestMain_healthProbeUsesCycleBudget(t *testing.T) {
	const helperEnv = "REGISTRY_STATS_HEALTH_PROBE_HELPER"
	if os.Getenv(helperEnv) == "1" {
		os.Args = []string{os.Args[0], "health"}
		main()
		return
	}

	path := health.DefaultPath
	oldInfo, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatalf("stat existing health marker: %v", statErr)
	}
	var oldData []byte
	if oldInfo != nil {
		var readErr error
		oldData, readErr = os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read existing health marker: %v", readErr)
		}
	}
	t.Cleanup(func() {
		if oldInfo == nil {
			health.NewMarker(path).Cleanup()
			return
		}
		if err := os.WriteFile(path, oldData, oldInfo.Mode().Perm()); err != nil {
			t.Errorf("restore health marker: %v", err)
			return
		}
		if err := os.Chtimes(path, oldInfo.ModTime(), oldInfo.ModTime()); err != nil {
			t.Errorf("restore health marker time: %v", err)
		}
	})

	const maximumCycleDuration = 92*time.Hour + 44*time.Minute + 40*time.Second
	tests := []struct {
		name     string
		interval string
		age      time.Duration
		wantCode int
	}{
		{name: "inside_cycle_budget", interval: "1", age: maximumCycleDuration + time.Hour - 5*time.Minute, wantCode: 0},
		{name: "outside_cycle_budget", interval: "1", age: maximumCycleDuration + time.Hour + 5*time.Minute, wantCode: 1},
		{name: "configured_interval_is_added", interval: "2", age: maximumCycleDuration + 90*time.Minute, wantCode: 0},
		{name: "one_shot_has_no_deadline", interval: "0", age: maximumCycleDuration + 24*time.Hour, wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("write health marker: %v", err)
			}
			modified := time.Now().Add(-tt.age)
			if err := os.Chtimes(path, modified, modified); err != nil {
				t.Fatalf("age health marker: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMain_healthProbeUsesCycleBudget$")
			cmd.Env = append(os.Environ(), helperEnv+"=1", "POLL_INTERVAL_HOURS="+tt.interval)
			output, err := cmd.CombinedOutput()
			gotCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run health helper: %v", err)
				}
				gotCode = exitErr.ExitCode()
			}
			if gotCode != tt.wantCode {
				t.Errorf("main health with interval=%s and age=%s exit = %d, want %d; output=%q",
					tt.interval, tt.age, gotCode, tt.wantCode, output)
			}
		})
	}
}

// mainFakeSource is a canned collect.Source for driving runCollect.
type mainFakeSource struct {
	onCollect     func()
	src           registry.ID
	entries       []registry.Entry
	listingFailed bool
}

func (f *mainFakeSource) Source() registry.ID { return f.src }

func (f *mainFakeSource) Collect(_ context.Context, _ []registry.RepoRef) registry.Collection {
	if f.onCollect != nil {
		f.onCollect()
	}
	return registry.Collection{
		Entries:       f.entries,
		Fetched:       len(f.entries),
		Attempted:     len(f.entries),
		ListingFailed: f.listingFailed,
	}
}

func TestLogWarnings_sanitizesAndCapsStringAttrs(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	raw := "bad\n" + strings.Repeat("x", 4096)
	warning := config.Warning{
		Msg: "config warning",
		Attrs: []slog.Attr{
			slog.String("value", raw),
			slog.Int("requested", 7),
			slog.String("expected", "owner/repo or owner/*"),
		},
	}
	logWarnings([]config.Warning{warning})

	var record map[string]any
	if err := json.NewDecoder(buf).Decode(&record); err != nil {
		t.Fatalf("decode warning record: %v", err)
	}
	value, ok := record["value"].(string)
	if !ok {
		t.Fatalf("warning value = %#v, want string", record["value"])
	}
	if len(value) > warnValueBytes {
		t.Errorf("warning value length = %d, want at most %d", len(value), warnValueBytes)
	}
	if strings.Contains(value, "\n") || !strings.HasPrefix(value, "bad ") {
		t.Errorf("warning value = %q, want newline replaced with a space", value)
	}
	if got := record["requested"]; got != float64(7) {
		t.Errorf("warning requested = %#v, want 7", got)
	}
	if got := record["expected"]; got != "owner/repo or owner/*" {
		t.Errorf("warning expected = %#v, want the second string attr intact", got)
	}
	if got := warning.Attrs[0].Value.String(); got != raw {
		t.Errorf("logWarnings mutated source attr = %q, want original value", got)
	}
}

// TestLoadConfig_emitsWarningsBeforeApplyingLogLevel pins the ratified
// ordering: a configuration diagnostic reaches the log stream even when
// the configuration being diagnosed sets LOG_LEVEL=error. Swaps
// slog.Default to capture, so no t.Parallel.
func TestLoadConfig_emitsWarningsBeforeApplyingLogLevel(t *testing.T) {
	t.Setenv("DOCKERHUB_REPOS", "library/alpine,bad//ref")
	t.Setenv("GHCR_REPOS", "")
	t.Setenv("LOG_LEVEL", "error")

	buf := &bytes.Buffer{}
	levelVar := &slog.LevelVar{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: levelVar})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	cfg := loadConfig(levelVar)

	if cfg.LogLevel != slog.LevelError {
		t.Fatalf("loadConfig(LOG_LEVEL=error).LogLevel = %v, want %v", cfg.LogLevel, slog.LevelError)
	}
	if got := levelVar.Level(); got != slog.LevelError {
		t.Errorf("loadConfig left level = %v, want %v applied", got, slog.LevelError)
	}
	if !strings.Contains(buf.String(), "skipping unusable repo ref") {
		t.Errorf("loadConfig(LOG_LEVEL=error) did not emit the repo-ref warning before applying the level; logs:\n%s", buf.String())
	}
}

// TestRunCollect_partialSuccessStaysHealthy pins the health-marker contract:
// when one registry produces data and the other fails, the marker stays healthy.
func TestRunCollect_partialSuccessStaysHealthy(t *testing.T) {
	dh := &mainFakeSource{
		src:           registry.DockerHub,
		entries:       []registry.Entry{{Owner: "o", Repo: "app", Pulls: 1}},
		listingFailed: false,
	}
	gh := &mainFakeSource{src: registry.GHCR, listingFailed: true} // no entries, unhealthy
	cfg := &config.Config{
		DockerHubRepos: []registry.RepoRef{{Owner: "o", Repo: "app"}},
		GHCRRepos:      []registry.RepoRef{{Owner: "o", Repo: "pkg"}},
	}
	marker := &mainFakeMarker{}
	runCollect(t.Context(), cfg, []collect.Source{dh, gh}, &publication{marker: marker, m: obs.New(), ready: &webhttp.Ready{}})
	if !marker.Healthy() {
		t.Error("runCollect partial success left marker unhealthy, want healthy")
	}
}

func TestRunCollect_latchesReadinessAcrossFailedCycle(t *testing.T) {
	source := &mainFakeSource{
		src:           registry.DockerHub,
		entries:       []registry.Entry{{Owner: "o", Repo: "app", Pulls: 7}},
		listingFailed: false,
	}
	cfg := &config.Config{
		DockerHubRepos: []registry.RepoRef{{Owner: "o", Repo: "app"}},
	}
	m := obs.New()
	marker := &mainFakeMarker{}
	ready := &webhttp.Ready{}
	pub := &publication{marker: marker, m: m, ready: ready}

	runCollect(t.Context(), cfg, []collect.Source{source}, pub)
	if !marker.Healthy() {
		t.Error("runCollect first cycle left marker unhealthy, want healthy")
	}
	if !ready.Ready() {
		t.Error("runCollect first cycle left readiness false, want true")
	}

	source.entries = nil
	source.listingFailed = true
	runCollect(t.Context(), cfg, []collect.Source{source}, pub)

	if marker.Healthy() {
		t.Error("runCollect failed cycle left marker healthy, want unhealthy")
	}
	if !ready.Ready() {
		t.Error("runCollect failed cycle cleared latched readiness")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := rec.Body.String(); strings.Contains(body, `owner="o",registry="dockerhub",repo="app"`) {
		t.Errorf("runCollect failed cycle left prior image series in metrics:\n%s", body)
	}
}

// TestRunCollect_cancelledCyclePublishesNothing pins both publication guards:
// a cancelled cycle moves neither health signal and publishes no image label.
func TestRunCollect_cancelledCyclePublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &mainFakeSource{
		onCollect:     cancel,
		src:           registry.DockerHub,
		entries:       []registry.Entry{{Owner: "o", Repo: "app", Pulls: 7}},
		listingFailed: false,
	}
	cfg := &config.Config{DockerHubRepos: []registry.RepoRef{{Owner: "o", Repo: "app"}}}
	m := obs.New()
	marker := &mainFakeMarker{}
	ready := &webhttp.Ready{}

	runCollect(ctx, cfg, []collect.Source{source}, &publication{marker: marker, m: m, ready: ready})

	if marker.Healthy() {
		t.Error("cancelled cycle set the marker healthy, want untouched")
	}
	if ready.Ready() {
		t.Error("cancelled cycle set readiness, want untouched")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := rec.Body.String(); strings.Contains(body, `repo="app"`) {
		t.Errorf("cancelled cycle published an image series:\n%s", body)
	}
}

// TestRunCollect_publishesImageMetrics pins that one runCollect cycle flows
// each source's records into the {registry,owner,repo} gauge labels on the
// real /metrics output, the label set grafana-dashboard.json queries.
func TestRunCollect_publishesImageMetrics(t *testing.T) {
	dh := &mainFakeSource{
		src:           registry.DockerHub,
		entries:       []registry.Entry{{Owner: "cplieger", Repo: "subflux", Pulls: 1234}},
		listingFailed: false,
	}
	gh := &mainFakeSource{
		src:           registry.GHCR,
		entries:       []registry.Entry{{Owner: "cplieger", Repo: "vibekit", Pulls: 56}},
		listingFailed: false,
	}
	cfg := &config.Config{
		DockerHubRepos: []registry.RepoRef{{Owner: "cplieger", Repo: "subflux"}},
		GHCRRepos:      []registry.RepoRef{{Owner: "cplieger", Repo: "vibekit"}},
	}
	m := obs.New()
	marker := &mainFakeMarker{}
	runCollect(t.Context(), cfg, []collect.Source{dh, gh}, &publication{marker: marker, m: m, ready: &webhttp.Ready{}})
	if !marker.Healthy() {
		t.Error("runCollect image publication left marker unhealthy, want healthy")
	}

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	body := w.Body.String()

	want := []string{
		`registrystats_image_pulls_total{owner="cplieger",registry="dockerhub",repo="subflux"} 1234`,
		`registrystats_image_pulls_total{owner="cplieger",registry="ghcr",repo="vibekit"} 56`,
		`registrystats_collect_duration_seconds_count 1`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("metrics output missing %q\n got:\n%s", line, body)
		}
	}
}

// mainFakeMarker is a minimal healthSignal for asserting the health flag.
type mainFakeMarker struct{ healthy bool }

func (m *mainFakeMarker) Set(h bool)    { m.healthy = h }
func (m *mainFakeMarker) Healthy() bool { return m.healthy }

// mainBlockingMarker parks inside Set so a test can observe the
// publication critical section from outside while a cycle is in it.
type mainBlockingMarker struct {
	entered chan struct{}
	release chan struct{}
}

func (m *mainBlockingMarker) Set(bool) {
	close(m.entered)
	<-m.release
}

// TestRunCollect_holdsPubAcrossPublication pins the serialization
// preDrain depends on: a cycle publishes its outcome under the publication
// lock, so the shutdown readiness clear cannot land between the cycle's
// context check and ready.Set(true) and be overwritten by it.
func TestRunCollect_holdsPubAcrossPublication(t *testing.T) {
	source := &mainFakeSource{
		src:     registry.DockerHub,
		entries: []registry.Entry{{Owner: "o", Repo: "app", Pulls: 7}},
	}
	cfg := &config.Config{DockerHubRepos: []registry.RepoRef{{Owner: "o", Repo: "app"}}}
	ready := &webhttp.Ready{}
	marker := &mainBlockingMarker{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	pub := &publication{marker: marker, m: obs.New(), ready: ready}
	done := make(chan struct{})
	go func() {
		runCollect(t.Context(), cfg, []collect.Source{source}, pub)
		close(done)
	}()
	<-marker.entered

	if pub.mu.TryLock() {
		pub.mu.Unlock()
		t.Error("runCollect published without holding its publication lock; a shutdown readiness clear can interleave")
	}

	close(marker.release)
	<-done
	if !ready.Ready() {
		t.Error("runCollect cycle left readiness false, want true")
	}
}

// TestLogConfig_noReposLogsError: with zero repos configured, logConfig must
// emit the operator-facing "no repos configured" ERROR warning that the
// healthcheck will fail after the first collect. Swaps slog.Default to
// capture, so no t.Parallel.
func TestLogConfig_noReposLogsError(t *testing.T) {
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	logConfig(&config.Config{})

	if !strings.Contains(buf.String(), "no repos configured") {
		t.Errorf("logConfig with no repos did not emit the expected ERROR; logs:\n%s", buf.String())
	}
}

// TestActiveSources_preMintsConfiguredSourcesOnly pins the cold-start
// contract the shipped RegistryStatsSourceDegraded rule reads: both per-source
// collect counters carry a zero sample before the first collect. A source with
// no configured refs gets no series.
func TestActiveSources_preMintsConfiguredSourcesOnly(t *testing.T) {
	m := obs.New()

	cfg := &config.Config{DockerHubRepos: []registry.RepoRef{{Owner: "o", Repo: "app"}}}
	sources := []collect.Source{
		&mainFakeSource{src: registry.DockerHub},
		&mainFakeSource{src: registry.GHCR},
	}

	active, names := activeSources(cfg, sources)
	m.MintCollectSources(names)

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	body := w.Body.String()

	for _, line := range []string{
		`registrystats_collects_total{source="dockerhub"} 0`,
		`registrystats_collect_errors_total{source="dockerhub"} 0`,
	} {
		if !strings.Contains(body, line) {
			t.Errorf("pre-mint missing %q\n got:\n%s", line, body)
		}
	}
	if strings.Contains(body, `source="ghcr"`) {
		t.Errorf("unconfigured source ghcr has a series; want none\n got:\n%s", body)
	}

	runCollect(t.Context(), cfg, active, &publication{marker: &mainFakeMarker{}, m: m, ready: &webhttp.Ready{}})
	w = httptest.NewRecorder()
	m.Handler()(w, r)
	body = w.Body.String()
	if !strings.Contains(body, `registrystats_collects_total{source="dockerhub"} 1`) || strings.Contains(body, `source="ghcr"`) {
		t.Errorf("runCollect did not use the configured source set; got:\n%s", body)
	}
}
