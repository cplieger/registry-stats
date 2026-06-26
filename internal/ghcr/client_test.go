package ghcr

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

// capturingLogger returns a logger that records every record (Debug and
// up) into buf so format-drift ERROR/WARN lines can be asserted on.
func capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// noMarkerServer serves HTML with no "Total downloads" marker for every
// request, so ParseDownloads returns ErrHTMLFormatChanged (a parse
// failure) for any package scrape.
func noMarkerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><div>no marker here</div></html>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNewClient_nilLogger_doesNotPanicOnErrorPath verifies a nil logger
// falls back to a usable default: an error path that logs must not
// nil-panic. A failing scrape logs at WARN, exercising that path, and
// the cycle reports unhealthy.
func TestNewClient_nilLogger_doesNotPanicOnErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), nil)
	_, _, healthy := c.Collect(t.Context(), []model.RepoRef{{Owner: "owner", Repo: "pkg1"}})
	if healthy {
		t.Error("Collect with an all-failing scrape = healthy true, want false")
	}
}

// TestNewClient_customLogger_isUsed confirms a supplied logger is the one
// actually used: a failing scrape's WARN must land in the supplied
// logger's buffer (a fallback-to-default would leave it empty).
func TestNewClient_customLogger_isUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), capturingLogger(&buf))
	_, _, _ = c.Collect(t.Context(), []model.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	if !strings.Contains(buf.String(), "ghcr scrape failed") {
		t.Errorf("supplied logger captured no scrape-failure log; logs:\n%s", buf.String())
	}
}

// TestClient_pacingDelay_appliesDefaults verifies the zero-value pacing
// fields fall back to the package defaults. The jitter default is
// load-bearing: without it rand.Int64N(0) would panic. Testing the
// extracted pacingDelay directly avoids sleeping the real 2s delay.
func TestClient_pacingDelay_appliesDefaults(t *testing.T) {
	t.Run("zero_min_uses_default", func(t *testing.T) {
		c := NewClient(http.DefaultClient, shortRetry(),
			Options{MinPacing: 0, PacingJitter: time.Nanosecond}, testsupport.QuietLogger())
		if d := c.pacingDelay(); d < DefaultMinPacing {
			t.Errorf("pacingDelay(MinPacing=0) = %v, want >= DefaultMinPacing %v", d, DefaultMinPacing)
		}
	})
	t.Run("zero_jitter_no_panic", func(t *testing.T) {
		c := NewClient(http.DefaultClient, shortRetry(), Options{}, testsupport.QuietLogger())
		if d := c.pacingDelay(); d < DefaultMinPacing {
			t.Errorf("pacingDelay(Options{}) = %v, want >= DefaultMinPacing %v", d, DefaultMinPacing)
		}
	})
}

// TestCollect_attemptedCountsScrapes verifies attempted reflects one
// count per scrape attempt: two successful scrapes => attempted == 2.
func TestCollect_attemptedCountsScrapes(t *testing.T) {
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

// TestCollect_singleParseFailure_logsMajorityDrift verifies a lone scrape
// that misses the "Total downloads" marker (parseFailures=1, total=1)
// trips the majority-format-drift ERROR, since 1*2 > 1.
func TestCollect_singleParseFailure_logsMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient(mockClient(noMarkerServer(t)), shortRetry(), fastPacing(), capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if !strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("expected majority-format-drift ERROR for 1/1 parse failure; logs:\n%s", buf.String())
	}
}

// TestCollect_noListingFailure_silent verifies explicit refs (no wildcard
// listing) never trip the listing-format-drift ERROR: listingParseFailures
// stays 0, so the listing-format log must not fire.
func TestCollect_noListingFailure_silent(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if strings.Contains(buf.String(), "listing HTML format may have changed") {
		t.Errorf("listing-format ERROR logged with zero listing parse failures; logs:\n%s", buf.String())
	}
}

// TestCollect_listingParseFailure_logsDrift verifies a wildcard whose
// listing page has no package links (listingParseFailures=1) trips the
// listing-format-drift ERROR.
func TestCollect_listingParseFailure_logsDrift(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links here</html>`))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if !strings.Contains(buf.String(), "listing HTML format may have changed") {
		t.Errorf("expected listing-format ERROR for a parse-failing wildcard listing; logs:\n%s", buf.String())
	}
}

// TestCollect_noScrapes_noMajorityDrift verifies that when a wildcard
// listing fails to parse (total stays 0 while parseFailures carries the
// listing failure), the per-scrape majority ERROR stays silent: its
// total>0 guard is false, so only the listing-format ERROR fires.
func TestCollect_noScrapes_noMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links</html>`))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 0 {
		t.Fatalf("precondition: attempted = %d, want 0 (no packages scraped)", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at total==0; logs:\n%s", buf.String())
	}
}

// TestCollect_allParseFailures_logsMajorityDrift verifies two refs that
// both miss the marker (total=2, parseFailures=2) trip the majority ERROR
// and report unhealthy.
func TestCollect_allParseFailures_logsMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient(mockClient(noMarkerServer(t)), shortRetry(), fastPacing(), capturingLogger(&buf))
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

// TestCollect_halfParseFailures_noMajorityDrift verifies exactly half the
// scrapes failing to parse (total=2, parseFailures=1) does NOT trip the
// majority ERROR: 1*2 > 2 is false (half is not a majority).
func TestCollect_halfParseFailures_noMajorityDrift(t *testing.T) {
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

	c := NewClient(mockClient(srv), shortRetry(), fastPacing(), capturingLogger(&buf))
	refs := []model.RepoRef{{Owner: "owner", Repo: "good"}, {Owner: "owner", Repo: "bad"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at exactly half parse failures; logs:\n%s", buf.String())
	}
}

// TestCollect_listingFailuresExcludedFromHealth verifies listing failures
// are excluded from the per-package health ratio: a failing wildcard
// listing plus one succeeding explicit scrape stays healthy, because
// pkgFailures = failures - listingFailures = 0.
func TestCollect_listingFailuresExcludedFromHealth(t *testing.T) {
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

// TestCollect_minorityPackageFailures_healthy verifies a minority of
// package failures keeps the cycle healthy: one of two explicit scrapes
// fails (pkgFailures=1 < total=2), so the cycle stays healthy.
func TestCollect_minorityPackageFailures_healthy(t *testing.T) {
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
