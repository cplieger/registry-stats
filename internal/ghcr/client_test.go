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
// nil-panic. A failing scrape logs at WARN, exercising that path.
func TestNewClient_nilLogger_doesNotPanicOnErrorPath(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), nil))
	_, _, listingFailed := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})
	if listingFailed {
		t.Error("Collect with an all-failing scrape = listingFailed true, want false")
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
// runs on the synthetic clock rather than costing real wall time per
// package. It pins that both zero-value fallbacks apply, the delay leads
// the FIRST scrape, and every interval lands in
// [DefaultMinPacing, DefaultMinPacing+DefaultPacingJitter).
//
// httptest.NewTestServer's in-memory network is synctest-compatible and
// routes every request to the handler regardless of host, so the
// production github.com URLs reach it unrewritten.
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
		entries, attempted, listingFailed := c.Collect(t.Context(), refs)
		if attempted != len(refs) || listingFailed || len(entries) != len(refs) {
			t.Fatalf("Collect = (%d entries, attempted %d, listingFailed %v), want (%d, %d, false)",
				len(entries), attempted, listingFailed, len(refs), len(refs))
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
// without classifying the explicit-package failures as a listing failure.
func TestCollect_allParseFailures_logsMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient(noMarkerServer(t).Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
	_, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if listingFailed {
		t.Fatalf("precondition: listingFailed = true, want false (every scrape failed)")
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

// TestCollect_whollyFailedListing verifies that a wildcard listing failure
// is reported independently of a successful explicit scrape.
func TestCollect_whollyFailedListing(t *testing.T) {
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
	entries, _, listingFailed := c.Collect(t.Context(), refs)

	if !listingFailed {
		t.Error("Collect listingFailed = false, want true for a wholly failed wildcard listing")
	}
	if len(entries) != 1 || entries[0].Pulls != 99 {
		t.Fatalf("entries = %+v, want exactly one entry with Pulls=99", entries)
	}
}

// TestCollect_packageFailuresDoNotSetListingFailed verifies that failures
// scraping explicit packages do not masquerade as a wildcard-listing outage.
func TestCollect_packageFailuresDoNotSetListingFailed(t *testing.T) {
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
	_, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Fatalf("precondition: attempted = %d, want 2", attempted)
	}
	if listingFailed {
		t.Error("Collect listingFailed = true, want false for an explicit-package failure")
	}
}

func TestCollect_clientTimeoutDoesNotSetListingFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/fast", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("9")))
	})
	srv := httptest.NewTestServer(t, mux)

	client := srv.Client()
	client.Timeout = 5 * time.Millisecond
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "slow"}, {Owner: "owner", Repo: "fast"}}
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Errorf("Collect after a client timeout attempted = %d, want 2", attempted)
	}
	if listingFailed {
		t.Error("Collect after a client timeout listingFailed = true, want false")
	}
	if len(entries) != 1 || entries[0].Repo != "fast" {
		t.Errorf("Collect after a client timeout entries = %+v, want only owner/fast", entries)
	}
}

// TestCollect_listingParseFailureWithSuccessfulScrape_noMajorityDrift pins
// that a wholly failed wildcard listing gets its owner-scoped ERROR without
// counting as a package scrape failure. The successful explicit scrape keeps
// the per-package majority ERROR silent.
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
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 1 {
		t.Fatalf("precondition: attempted = %d, want 1 (only the explicit pkg1 was scraped)", attempted)
	}
	if !listingFailed {
		t.Error("listingFailed = false, want true for a wildcard listing that yielded no packages")
	}
	if len(entries) != 1 || entries[0].Pulls != 99 {
		t.Fatalf("entries = %+v, want exactly one entry with Pulls=99", entries)
	}
	logs := buf.String()
	if !strings.Contains(logs, "ghcr package listing failed") {
		t.Errorf("expected owner-scoped ERROR for the parse-failing wildcard listing; logs:\n%s", logs)
	}
	if strings.Contains(logs, "majority of scrapes hit format errors") {
		t.Errorf("per-package majority ERROR must not fire when the only parse failure is the listing (pkgParseFailures=0); logs:\n%s", logs)
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

	entries, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	if attempted != 0 {
		t.Errorf("Collect attempted = %d, want 0 (ctx cancelled before the first scrape)", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("Collect entries = %+v, want none (cancelled before any scrape completed)", entries)
	}
	if listingFailed {
		t.Error("Collect listingFailed = true, want false for cancellation")
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
		entries, attempted, listingFailed := c.Collect(ctx, refs)

		if attempted != 1 || len(entries) != 1 {
			t.Fatalf("Collect(2 refs, cancelled in the second pacing wait) = (%d entries, attempted %d), want (1, 1)",
				len(entries), attempted)
		}
		if listingFailed {
			t.Error("Collect listingFailed = true, want false for cancellation")
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
// attempted, listingFailed stays false so collect_errors_total does not move
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
		entries, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

		if attempted != 0 || len(entries) != 0 {
			t.Errorf("Collect(cancelled in the pacing wait) = (%d entries, attempted %d), want (0, 0)",
				len(entries), attempted)
		}
		if listingFailed {
			t.Error("Collect listingFailed = true, want false for cancellation")
		}
		if logs := buf.String(); !strings.Contains(logs, "ghcr collection interrupted by context cancellation") {
			t.Errorf("Collect(cancelled in the pacing wait) logged no interruption; logs:\n%s", logs)
		}
	})
}
