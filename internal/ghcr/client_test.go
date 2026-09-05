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

// TestNewClient_customLogger_isUsed confirms a supplied logger is the one
// actually used: a failing scrape's WARN must land in the supplied
// logger's buffer (a fallback-to-default would leave it empty).
func TestNewClient_customLogger_isUsed(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	var defaultBuf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(capturingLogger(&defaultBuf))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	var injectedBuf bytes.Buffer
	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&injectedBuf)))
	_, _, _, _ = c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	if logs := defaultBuf.String(); logs != "" {
		t.Errorf("default logger captured records from Collect(retryable failure):\n%s", logs)
	}
	logs := injectedBuf.String()
	if !strings.Contains(logs, `level=DEBUG msg="http retries exhausted"`) {
		t.Errorf("supplied logger captured no DEBUG retry-exhausted log; logs:\n%s", logs)
	}
	if got := strings.Count(logs, `level=WARN msg="ghcr scrape failed"`); got != 1 {
		t.Errorf("supplied logger captured %d ghcr scrape-failure WARNs, want 1; logs:\n%s", got, logs)
	}
}

func TestClient_ScrapePackage_BoundsErrorLog(t *testing.T) {
	var buf bytes.Buffer
	invalidCount := strings.Repeat("x", 4096)
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML(invalidCount)))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	_, _, _, _ = c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

	logs := buf.String()
	if !strings.Contains(logs, "ghcr scrape failed") {
		t.Errorf("Collect(invalid count) emitted no scrape-failure log; logs:\n%s", logs)
	}
	if len(logs) > 1024 {
		t.Errorf("Collect(4096-byte invalid count) emitted %d log bytes, want bounded output", len(logs))
	}
}

// TestClient_Collect_pacesAtProductionDefaults drives Collect with the
// zero-value pacing fields against an in-memory test server inside a
// synctest bubble, so the real DefaultMinPacing / DefaultPacingJitter path
// runs on the synthetic clock rather than costing real wall time per
// package. It pins that both zero-value fallbacks apply, the first request
// issues without advancing the clock, and every later interval lands in
// [DefaultMinPacing, DefaultMinPacing+DefaultPacingJitter).
//
// httptest.NewTestServer's in-memory network is synctest-compatible and
// routes every request to the handler regardless of host, so the
// production github.com URLs reach it unrewritten.
func TestClient_Collect_pacesAtProductionDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stamps []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stamps = append(stamps, time.Now())
			switch r.URL.Path {
			case "/owner":
				_, _ = w.Write([]byte(`<div data-total-pages="1"></div>` +
					packageLink(userOwner, "owner", "pkg1") +
					packageLink(userOwner, "owner", "pkg2")))
			case "/users/owner/packages/container/package/pkg1", "/users/owner/packages/container/package/pkg2":
				_, _ = w.Write([]byte(downloadsHTML("11")))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))

		// Pacing fields left at zero on purpose: they are the subject.
		c := NewClient(srv.Client(), Options{Logger: testsupport.QuietLogger()})
		refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}

		start := time.Now()
		entries, fetched, attempted, listingFailed := c.Collect(t.Context(), refs)
		if fetched != 2 || attempted != 2 || listingFailed || len(entries) != 2 {
			t.Fatalf("Collect(wildcard) = (%d entries, fetched %d, attempted %d, listingFailed %v), want (2, 2, 2, false)",
				len(entries), fetched, attempted, listingFailed)
		}
		if len(stamps) != 3 {
			t.Fatalf("handler saw %d requests, want 3 (one listing and two packages)", len(stamps))
		}
		if !stamps[0].Equal(start) {
			t.Errorf("first request issued at %v, want no clock advance from %v", stamps[0], start)
		}

		const maxPacing = DefaultMinPacing + DefaultPacingJitter
		for i := 1; i < len(stamps); i++ {
			gap := stamps[i].Sub(stamps[i-1])
			if gap < DefaultMinPacing || gap >= maxPacing {
				t.Errorf("pacing interval %d = %v, want [%v, %v)", i, gap, DefaultMinPacing, maxPacing)
			}
		}
	})
}

func TestCollect_noScrapes_noMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>no package links</html>`))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	_, _, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 0 {
		t.Fatalf("precondition: attempted = %d, want 0 (no packages scraped)", attempted)
	}
	if strings.Contains(buf.String(), "majority of scrapes hit format errors") {
		t.Errorf("majority ERROR logged at total==0; logs:\n%s", buf.String())
	}
}

func TestCollect_allParseFailures_logsMajorityDrift(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient(noMarkerServer(t).Client(), fastPacing(shortRetry(), capturingLogger(&buf)))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
	_, _, attempted, listingFailed := c.Collect(t.Context(), refs)

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
	_, _, attempted, _ := c.Collect(t.Context(), refs)

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
	entries, _, _, listingFailed := c.Collect(t.Context(), refs)

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
	_, fetched, attempted, listingFailed := c.Collect(t.Context(), refs)

	if fetched != 1 || attempted != 2 {
		t.Fatalf("Collect with one successful and one failed scrape = (fetched %d, attempted %d), want (1, 2)", fetched, attempted)
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
	entries, _, attempted, listingFailed := c.Collect(t.Context(), refs)

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
	entries, _, attempted, listingFailed := c.Collect(t.Context(), refs)

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
// path: an already-cancelled ctx reaches the first scrape and returns through
// the cancelled-result arm with the results gathered so far, the attempted
// count and the cycle verdict, rather than blocking or panicking. MinPacing is
// an hour so an accidental leading wait would hang; no HTTP request completes
// because the cancelled context reaches the transport immediately.
func TestCollect_ContextCancelledDuringPacing(t *testing.T) {
	c := NewClient(http.DefaultClient,
		Options{MinPacing: time.Hour, PacingJitter: time.Nanosecond, RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	entries, _, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}})

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

func TestCollect_cancelledMidCycle_isNotAFailure(t *testing.T) {
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

		// Between the immediate first scrape and the second at t+1h.
		time.AfterFunc(30*time.Minute, cancel)
		entries, _, attempted, listingFailed := c.Collect(ctx, refs)

		if attempted != 1 || len(entries) != 1 {
			t.Fatalf("Collect(2 refs, cancelled in the second pacing wait) = (%d entries, attempted %d), want (1, 1)",
				len(entries), attempted)
		}
		if listingFailed {
			t.Error("Collect listingFailed = true, want false for cancellation")
		}
		if logs := buf.String(); strings.Contains(logs, "level=WARN") {
			t.Errorf("Collect(2 refs, cancelled mid-cycle) logged a source warning; logs:\n%s", logs)
		}
	})
}

func TestCollect_cancelledInPacingWait_isNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		requests := 0
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			if requests > 1 {
				t.Error("Collect issued the second request, want the stop to land in its pacing wait")
				return
			}
			_, _ = w.Write([]byte(downloadsHTML("5")))
		}))

		c := NewClient(srv.Client(), Options{
			MinPacing:    time.Hour,
			PacingJitter: time.Nanosecond,
			RetryOpts:    shortRetry(),
			Logger:       capturingLogger(&buf),
		})

		time.AfterFunc(time.Minute, cancel)
		refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}, {Owner: "owner", Repo: "pkg2"}}
		entries, _, attempted, listingFailed := c.Collect(ctx, refs)

		if attempted != 1 || len(entries) != 1 {
			t.Errorf("Collect(cancelled in the second pacing wait) = (%d entries, attempted %d), want (1, 1)",
				len(entries), attempted)
		}
		if listingFailed {
			t.Error("Collect listingFailed = true, want false for cancellation")
		}
		if logs := buf.String(); strings.Contains(logs, "level=WARN") {
			t.Errorf("Collect(cancelled in the second pacing wait) logged a source warning; logs:\n%s", logs)
		}
	})
}

