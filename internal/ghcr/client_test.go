package ghcr

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
)

// capturingLogger returns a logger that records every record (Debug and
// up) into buf so format-drift ERROR/WARN lines can be asserted on.
func capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// noMarkerServer serves HTML with no "Total downloads" marker for every
// request, so parseDownloads returns errHTMLFormatChanged (a parse
// failure) for any package scrape.
func noMarkerServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><div>no marker here</div></html>`))
	}))
	return srv
}

// TestNewClient_nilLogger_doesNotPanicOnErrorPath verifies a nil logger
// falls back to a usable default: an error path that logs must not
// nil-panic. A failing scrape logs at WARN, exercising that path, and
// the cycle reports unhealthy.
func TestNewClient_nilLogger_doesNotPanicOnErrorPath(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), nil))
	_, _, healthy := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})
	if healthy {
		t.Error("Collect with an all-failing scrape = healthy true, want false")
	}
}

// TestNewClient_customLogger_isUsed confirms a supplied logger is the one
// actually used: a failing scrape's WARN must land in the supplied
// logger's buffer (a fallback-to-default would leave it empty).
func TestNewClient_customLogger_isUsed(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	var buf bytes.Buffer
	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	_, _, _ = c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	if !strings.Contains(buf.String(), "ghcr scrape failed") {
		t.Errorf("supplied logger captured no scrape-failure log; logs:\n%s", buf.String())
	}
}

// TestClient_Collect_pacesAtProductionDefaults drives Collect with the
// zero-value pacing fields against an in-memory test server inside a
// synctest bubble, so the real DefaultMinPacing / DefaultPacingJitter path
// runs on the synthetic clock instead of costing 2-5 s of wall time per
// package. It pins three things the old pacingDelay unit test could not
// reach, because it called the helper instead of letting a request pace:
// both zero-value fallbacks are applied (a zero jitter would panic
// rand.N(0), a zero minimum would collapse the gap), the delay leads the
// FIRST scrape as the collect comment claims, and every interval lands in
// [DefaultMinPacing, DefaultMinPacing+DefaultPacingJitter).
//
// httptest.NewTestServer is what makes this possible: its in-memory network
// is synctest-compatible and routes every request to the handler regardless
// of host, so the production github.com URLs reach it unrewritten.
func TestClient_Collect_pacesAtProductionDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stamps []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			stamps = append(stamps, time.Now())
			_, _ = w.Write([]byte(downloadsHTML("11")))
		}))

		// Pacing fields left at zero on purpose: they are the subject.
		c := NewClient(srv.Client(), Options{Logger: testsupport.QuietLogger()})
		refs := []registry.RepoRef{
			{Owner: "owner", Repo: "pkg1"},
			{Owner: "owner", Repo: "pkg2"},
			{Owner: "owner", Repo: "pkg3"},
		}

		start := time.Now()
		entries, attempted, healthy := c.Collect(t.Context(), refs)
		if attempted != len(refs) || !healthy || len(entries) != len(refs) {
			t.Fatalf("Collect = (%d entries, attempted %d, healthy %v), want (%d, %d, true)",
				len(entries), attempted, healthy, len(refs), len(refs))
		}
		if len(stamps) != len(refs) {
			t.Fatalf("handler saw %d requests, want %d", len(stamps), len(refs))
		}

		// Interval 0 is the leading delay before the first scrape; the rest
		// are the gaps between consecutive scrapes.
		const maxPacing = DefaultMinPacing + DefaultPacingJitter
		prev := start
		for i, at := range stamps {
			gap := at.Sub(prev)
			if gap < DefaultMinPacing || gap >= maxPacing {
				t.Errorf("pacing interval %d = %v, want [%v, %v)", i, gap, DefaultMinPacing, maxPacing)
			}
			prev = at
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
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
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
	c := NewClient(noMarkerServer(t).Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}}
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if strings.Contains(buf.String(), "owner listing yielded no packages") {
		t.Errorf("listing-empty ERROR logged with zero listing parse failures; logs:\n%s", buf.String())
	}
}

// TestCollect_listingParseFailure_logsDrift verifies a wildcard whose
// listing page has no package links (listingParseFailures=1) trips the
// listing-format-drift ERROR.
func TestCollect_listingParseFailure_logsDrift(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links here</html>`))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	_, _, _ = c.Collect(t.Context(), refs)

	if !strings.Contains(buf.String(), "owner listing yielded no packages") {
		t.Errorf("expected listing-empty ERROR for a parse-failing wildcard listing; logs:\n%s", buf.String())
	}
}

// TestCollect_noScrapes_noMajorityDrift verifies that when a wildcard
// listing fails to parse (total stays 0 while parseFailures carries the
// listing failure), the per-scrape majority ERROR stays silent: its
// total>0 guard is false, so only the listing-empty ERROR fires.
func TestCollect_noScrapes_noMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links</html>`))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
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
	c := NewClient(noMarkerServer(t).Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
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
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "good"}, {Owner: "owner", Repo: "bad"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at exactly half parse failures; logs:\n%s", buf.String())
	}
}

// TestCollect_whollyFailedListing_isUnhealthy verifies a wildcard listing
// that yielded nothing flips the cycle unhealthy even when an explicit
// scrape succeeded: an unknown number of packages went uncollected, and no
// exported series can report its own absence, so collect_errors_total has
// to move. Listing failures are still out of the per-package RATIO — they
// are a separate arm of the verdict, not a subtraction.
func TestCollect_whollyFailedListing_isUnhealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("99")))
	})
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}, {Owner: "owner", Repo: "pkg1"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if healthy {
		t.Error("Collect healthy = true, want false (the wildcard owner listing wholly failed)")
	}
	if len(entries) != 1 || entries[0].Pulls != 99 {
		t.Fatalf("entries = %+v, want exactly one entry with Pulls=99", entries)
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
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "ok"}, {Owner: "owner", Repo: "fail"}}
	_, attempted, healthy := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if !healthy {
		t.Error("Collect healthy = false, want true (1 of 2 failures is a minority)")
	}
}

// TestCollect_listingParseFailureWithSuccessfulScrape_noMajorityDrift pins the
// pkgParseFailures arithmetic (parseFailures - listingParseFailures): a wildcard
// whose listing page parse-fails (listingParseFailures=1) alongside a successful
// explicit scrape (total=1) must fire ONLY the listing-empty ERROR, never the
// per-package majority ERROR. The lone parse failure belongs to the listing,
// which has its own dedicated signal, so pkgParseFailures is 0 and the
// per-package majority check (pkgParseFailures*2 > total) stays silent.
func TestCollect_listingParseFailureWithSuccessfulScrape_noMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links here</html>`))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("99")))
	})
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}, {Owner: "owner", Repo: "pkg1"}}
	entries, attempted, healthy := c.Collect(t.Context(), refs)

	if attempted != 1 {
		t.Fatalf("precondition: attempted = %d, want 1 (only the explicit pkg1 was scraped)", attempted)
	}
	if healthy {
		t.Errorf("healthy = true, want false (the wildcard owner listing yielded no packages)")
	}
	if len(entries) != 1 || entries[0].Pulls != 99 {
		t.Fatalf("entries = %+v, want exactly one entry with Pulls=99", entries)
	}
	logs := buf.String()
	if !strings.Contains(logs, "owner listing yielded no packages") {
		t.Errorf("expected listing-empty ERROR for the parse-failing wildcard listing; logs:\n%s", logs)
	}
	if strings.Contains(logs, "majority of scrapes hit format errors") {
		t.Errorf("per-package majority ERROR must not fire when the only parse failure is the listing (pkgParseFailures=0); logs:\n%s", logs)
	}
}

// TestCycleHealthy unit-tests the pure cycle verdict in isolation from the
// HTTP-mock Collect pipeline. A cycle is healthy when no wildcard owner
// listing wholly failed and package failures are at most half of the
// scrapes (pkgFailures*2 <= total). The rows pin every boundary outcome:
// empty, a wholly-failed listing with nothing scraped, exactly half, a
// strict majority that is not a total outage, a full outage, and a clear
// minority.
func TestCycleHealthy(t *testing.T) {
	tests := []struct {
		name                string
		pkgFailures         int
		total               int
		listingWhollyFailed bool
		want                bool
	}{
		{name: "no failures no scrapes is healthy", want: true},
		{name: "no failures with scrapes is healthy", total: 5, want: true},
		{name: "a wholly failed listing is unhealthy", listingWhollyFailed: true},
		{name: "a wholly failed listing is unhealthy even beside successful scrapes", total: 5, listingWhollyFailed: true},
		{name: "sole package failure is a total outage", pkgFailures: 1, total: 1},
		{name: "exactly half failures stays healthy", pkgFailures: 1, total: 2, want: true},
		{name: "majority but not total is unhealthy", pkgFailures: 2, total: 3},
		{name: "all packages failed is unhealthy", pkgFailures: 3, total: 3},
		{name: "clear minority stays healthy", pkgFailures: 1, total: 4, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cycleHealthy(tt.pkgFailures, tt.total, tt.listingWhollyFailed); got != tt.want {
				t.Errorf("cycleHealthy(pkgFailures=%d, total=%d, listingWhollyFailed=%v) = %v, want %v",
					tt.pkgFailures, tt.total, tt.listingWhollyFailed, got, tt.want)
			}
		})
	}
}

// TestCollect_ContextCancelledDuringPacing pins Collect's graceful-shutdown
// path: an already-cancelled ctx is caught at the top of the package loop, so
// it returns immediately with the results gathered so far, the attempted
// count and the cycle verdict, rather than blocking on a pacing wait or
// panicking. MinPacing is an hour so nothing can complete first, making the
// branch deterministic without a real sleep; no HTTP request is issued
// because cancellation precedes the first scrape (buildPackageList makes no
// network call for an explicit ref).
func TestCollect_ContextCancelledDuringPacing(t *testing.T) {
	c := NewClient(http.DefaultClient,
		Options{MinPacing: time.Hour, PacingJitter: time.Nanosecond, RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	entries, attempted, healthy := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	if attempted != 0 {
		t.Errorf("Collect attempted = %d, want 0 (ctx cancelled before the first scrape)", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("Collect entries = %+v, want none (cancelled before any scrape completed)", entries)
	}
	if !healthy {
		t.Error("Collect healthy = false, want true (no package failures recorded before cancellation)")
	}
}

// TestCollect_cancelledMidCycle_logsUnscrapedRemainder pins the count the
// interrupted-collection log reports for the packages a shutdown skipped:
// the packages never reached, not the whole list. It is what tells an
// operator how much of the cycle a SIGTERM cost, so it has to shrink as
// the cycle progresses rather than restate the list length.
//
// The stop is scheduled to land inside the SECOND package's pacing wait,
// which is the widest window a signal can arrive in, so one of the two
// packages is already collected and one is never reached. A package a stop
// interrupted is neither attempted nor failed, so attempted stays at the
// one that completed.
//
// synctest keeps it deterministic and free: the hour-long pacing costs no
// wall time on the synthetic clock, and the cancellation fires at a fixed
// point on it rather than racing a response.
func TestCollect_cancelledMidCycle_logsUnscrapedRemainder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(downloadsHTML("5")))
		}))

		c := NewClient(srv.Client(), Options{
			MinPacing:    time.Hour,
			PacingJitter: time.Nanosecond,
			RetryOpts:    shortRetry(),
			Logger:       capturingLogger(&buf),
		})
		refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}

		// Between the first scrape (paced to t+1h) and the second (t+2h).
		time.AfterFunc(90*time.Minute, cancel)
		entries, attempted, healthy := c.Collect(ctx, refs)

		if attempted != 1 || len(entries) != 1 {
			t.Fatalf("Collect(2 refs, cancelled in the second pacing wait) = (%d entries, attempted %d), want (1, 1)",
				len(entries), attempted)
		}
		if !healthy {
			t.Error("Collect healthy = false, want true (a stop is not a package failure)")
		}
		logs := buf.String()
		if !strings.Contains(logs, "ghcr collection interrupted by context cancellation") {
			t.Fatalf("Collect(2 refs, cancelled mid-cycle) logged no interruption; logs:\n%s", logs)
		}
		if !strings.Contains(logs, "remaining=1") {
			t.Errorf("Collect(2 refs, 1 attempted) interruption log = %q, want it to carry remaining=1", logs)
		}
	})
}

// TestCollect_cancelledInPacingWait_isNotAFailure pins that the widest
// window a SIGTERM can land in — the pacing wait before a scrape — is
// reported as a stop rather than as a GHCR failure: nothing is counted
// attempted, the cycle stays healthy so collect_errors_total does not move
// on a redeploy, and the interruption WARN still names what was skipped.
func TestCollect_cancelledInPacingWait_isNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("Collect issued a request, want the stop to land in the pacing wait first")
		}))

		c := NewClient(srv.Client(), Options{
			MinPacing:    time.Hour,
			PacingJitter: time.Nanosecond,
			RetryOpts:    shortRetry(),
			Logger:       capturingLogger(&buf),
		})

		time.AfterFunc(time.Minute, cancel)
		entries, attempted, healthy := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

		if attempted != 0 || len(entries) != 0 {
			t.Errorf("Collect(cancelled in the pacing wait) = (%d entries, attempted %d), want (0, 0)",
				len(entries), attempted)
		}
		if !healthy {
			t.Error("Collect healthy = false, want true (a stop is not a package failure)")
		}
		if logs := buf.String(); !strings.Contains(logs, "ghcr collection interrupted by context cancellation") {
			t.Errorf("Collect(cancelled in the pacing wait) logged no interruption; logs:\n%s", logs)
		}
	})
}
