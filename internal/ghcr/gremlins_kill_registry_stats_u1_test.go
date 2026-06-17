package ghcr

// Tests added by mutant-killing unit registry-stats-u1. They target
// surviving gremlins mutants in client.go (and confirm equivalence for a
// few in scraper.go via reasoning, not via a test). Helpers from
// scraper_test.go in this same package (mockClient, shortRetry,
// fastPacing, downloadsHTML) are reused directly; every identifier
// defined here is prefixed gk_registry_stats_u1_ to avoid collisions.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// gk_registry_stats_u1_capturingLogger returns a slog.Logger that writes
// every record (Debug and up) into buf so format-drift ERROR lines can be
// asserted on. The format-drift logs under test are emitted at Error
// level, which Debug-level capture always includes.
func gk_registry_stats_u1_capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- client.go:49:12 CONDITIONALS_NEGATION (`if logger == nil`) ---

func TestGkRegistryStatsU1_NewClientNilLoggerGetsDefault(t *testing.T) {
	// Original `if logger == nil { logger = slog.Default() }` replaces a
	// nil arg with the default. The negation `if logger != nil` would
	// leave c.logger nil for a nil arg.
	c := NewClient(http.DefaultClient, shortRetry(), Options{}, nil)
	if c.logger == nil {
		t.Error("NewClient(logger=nil).logger = nil, want non-nil slog.Default()")
	}
}

func TestGkRegistryStatsU1_NewClientCustomLoggerPreserved(t *testing.T) {
	// Other side of the negation: a non-nil custom logger must be kept.
	// The negated `if logger != nil { logger = slog.Default() }` would
	// overwrite it with the default (a different pointer).
	want := slog.New(slog.DiscardHandler)
	c := NewClient(http.DefaultClient, shortRetry(), Options{}, want)
	if c.logger != want {
		t.Errorf("NewClient(custom).logger = %p, want the supplied logger %p", c.logger, want)
	}
}

// --- client.go:117:16 CONDITIONALS_BOUNDARY + NEGATION (`pacingMin <= 0`) ---

func TestGkRegistryStatsU1_CollectZeroMinPacingUsesDefault(t *testing.T) {
	// `if pacingMin <= 0 { pacingMin = DefaultMinPacing }`. With
	// MinPacing == 0 the original selects the 2s default, so a single
	// paced scrape blocks ~2s. Both the boundary mutation (`< 0`, false at
	// 0) and the negation mutation (`> 0`, false at 0) leave pacingMin at
	// 0, collapsing the delay to ~0. PacingJitter=1ns forces jitter to 0
	// (rand.Int64N(1) == 0) so the elapsed time is the pacing alone and
	// the assertion is deterministic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("5")))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(),
		Options{MinPacing: 0, PacingJitter: time.Nanosecond}, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}

	start := time.Now()
	_, _, _ = c.Collect(t.Context(), refs)
	elapsed := time.Since(start)

	if elapsed < time.Second {
		t.Errorf("Collect(MinPacing=0) elapsed = %v, want >= 1s (DefaultMinPacing 2s pacing)", elapsed)
	}
}

// --- client.go:141:8 INCREMENT_DECREMENT (`total++`) ---

func TestGkRegistryStatsU1_CollectAttemptedCountsScrapes(t *testing.T) {
	// total++ runs once per scrape attempt and is returned as `attempted`.
	// Two successful scrapes => attempted == 2; `total--` would yield -2.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("10")))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg2", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("20")))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Errorf("Collect attempted = %d, want 2", attempted)
	}
}

// --- client.go:151:18 INCREMENT_DECREMENT (`parseFailures++`) ---

func TestGkRegistryStatsU1_CollectParseFailureDrivesMajorityLog(t *testing.T) {
	// A single scrape that misses the "Total downloads" marker yields
	// parseFailures=1, total=1, so `parseFailures*2 (=2) > total (=1)` is
	// true and the majority-format-drift ERROR is logged. `parseFailures--`
	// gives -1 => `-2 > 1` is false => no log.
	var buf bytes.Buffer
	c := NewClient(mockClient(gk_registry_stats_u1_noMarkerServer(t)), shortRetry(), fastPacing(),
		gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if !strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("expected majority-format-drift ERROR for 1/1 parse failure; logs:\n%s", buf.String())
	}
}

// gk_registry_stats_u1_noMarkerServer serves HTML with no "Total
// downloads" marker for every request, so ParseDownloads returns
// ErrHTMLFormatChanged (a parse failure) for any package scrape.
func gk_registry_stats_u1_noMarkerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><div>no marker here</div></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- client.go:171:26 CONDITIONALS_BOUNDARY + NEGATION (`listingParseFailures > 0`) ---

func TestGkRegistryStatsU1_CollectNoListingFailureNoListingLog(t *testing.T) {
	// Explicit refs only => listingParseFailures == 0, so the original
	// `if listingParseFailures > 0` is false and the listing-format-drift
	// ERROR is NOT logged. The boundary (`>= 0`) and negation (`<= 0`)
	// mutations are both true at 0 and would log it spuriously.
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if strings.Contains(buf.String(), "listing HTML format may have changed") {
		t.Errorf("listing-format ERROR logged with zero listing parse failures; logs:\n%s", buf.String())
	}
}

func TestGkRegistryStatsU1_CollectListingParseFailureLogsDrift(t *testing.T) {
	// A wildcard whose listing page has no package links yields
	// listingParseFailures == 1, so `1 > 0` is true and the listing-format
	// ERROR is logged. The negation `1 <= 0` would skip it.
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links here</html>`))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if !strings.Contains(buf.String(), "listing HTML format may have changed") {
		t.Errorf("expected listing-format ERROR for a parse-failing wildcard listing; logs:\n%s", buf.String())
	}
}

// --- client.go:180:11 CONDITIONALS_BOUNDARY + NEGATION (`total > 0`) ---

func TestGkRegistryStatsU1_CollectNoScrapesNoMajorityLog(t *testing.T) {
	// A parse-failing wildcard listing leaves total == 0 while
	// parseFailures == 1 (carried over from listingParseFailures via
	// `parseFailures += listingParseFailures`). The original short-circuits
	// on `total > 0` (false) so the "majority of scrapes" ERROR is NOT
	// logged. The boundary (`total >= 0`) and negation (`total <= 0`)
	// mutations make the first clause true at total==0, then
	// `1*2 (=2) > 0` logs spuriously.
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links</html>`))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 0 {
		t.Fatalf("precondition: attempted = %d, want 0 (no packages scraped)", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at total==0; logs:\n%s", buf.String())
	}
}

// --- client.go:180:31 ARITHMETIC_BASE (`parseFailures*2`) + 180:34 NEGATION ---

func TestGkRegistryStatsU1_CollectAllParseFailLogsMajority(t *testing.T) {
	// Two refs that both miss the marker: total=2, parseFailures=2, so
	// `2*2 (=4) > 2` is true and the majority ERROR is logged.
	//   180:31 `parseFailures/2` => `1 > 2` (false) => no log.
	//   180:34 negation `4 <= 2` (false) => no log.
	var buf bytes.Buffer
	c := NewClient(mockClient(gk_registry_stats_u1_noMarkerServer(t)), shortRetry(), fastPacing(),
		gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
	_, attempted, healthy := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if healthy {
		t.Fatalf("precondition: healthy = true, want false (every scrape failed)")
	}
	if !strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("expected majority ERROR for 2/2 parse failures; logs:\n%s", buf.String())
	}
}

// --- client.go:180:34 CONDITIONALS_BOUNDARY (`parseFailures*2 > total`) ---

func TestGkRegistryStatsU1_CollectHalfParseFailNoMajority(t *testing.T) {
	// One of two scrapes misses the marker: total=2, parseFailures=1, so
	// `1*2 (=2) > 2` is false (exactly half is not a majority) and no ERROR
	// is logged. The boundary mutation `2 >= 2` (and the negation `2 <= 2`)
	// would log spuriously.
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/good", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("42")))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/bad", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no marker</html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), gk_registry_stats_u1_capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "good"}, {Owner: "owner", Repo: "bad"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at exactly half parse failures; logs:\n%s", buf.String())
	}
}

// --- client.go:190:26 ARITHMETIC_BASE + INVERT_NEGATIVES (`failures - listingFailures`) ---

func TestGkRegistryStatsU1_CollectSubtractsListingFailures(t *testing.T) {
	// `failures` already includes listingFailures (added earlier), so
	// `pkgFailures = failures - listingFailures` isolates package-level
	// failures. A failing wildcard listing (listingFailures=1, also folded
	// into failures) plus one succeeding explicit scrape gives failures=1,
	// listingFailures=1 => pkgFailures=0 => healthy=true. Flipping the
	// subtraction to addition gives pkgFailures=2 => healthy=false.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("99")))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}, {Owner: "owner", Repo: "pkg1"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if !healthy {
		t.Error("Collect healthy = false, want true (listing failures excluded from package health)")
	}
	if len(entries) != 1 || entries[0].DownloadCount != 99 {
		t.Fatalf("entries = %+v, want exactly one entry with DownloadCount=99", entries)
	}
}

// --- client.go:191:39 CONDITIONALS_NEGATION (`total > 0`, second OR clause) ---

func TestGkRegistryStatsU1_CollectMinorityPackageFailuresHealthy(t *testing.T) {
	// healthy = pkgFailures==0 || (total > 0 && pkgFailures < total).
	// One of two explicit scrapes fails => pkgFailures=1 < total=2, so the
	// second clause keeps healthy=true. Negating `total > 0` to
	// `total <= 0` makes that clause false at total=2 => healthy=false.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("3")))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "ok"}, {Owner: "owner", Repo: "fail"}}
	_, attempted, healthy := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if !healthy {
		t.Error("Collect healthy = false, want true (1 of 2 failures is a minority)")
	}
}
