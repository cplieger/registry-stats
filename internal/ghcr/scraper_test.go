package ghcr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
)

// packagePageURL is a representative production GHCR package-page URL for
// the fetchHTML tests. httptest.NewTestServer's in-memory client routes
// every request to the handler regardless of scheme, host or address — and
// leaves Server.URL empty — so these tests pass the URL production would
// build instead of a loopback address, which is closer to the real call.
const packagePageURL = "https://github.com/users/owner/packages/container/package/pkg"

// shortRetry returns httpx options with a 1 ms base delay so retry
// tests don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
}

// fastPacing returns Options with microsecond pacing/jitter so mock
// Collect tests don't sit on the 2-5 s production per-package delay
// baked into DefaultMinPacing / DefaultPacingJitter. Non-mock tests
// that intentionally exercise default pacing should keep passing
// Options{} (zero value falls back to the DefaultPacing* constants).
func fastPacing(retry []httpx.GetOption, logger *slog.Logger) Options {
	return Options{
		MinPacing:    time.Microsecond,
		PacingJitter: time.Microsecond,
		RetryOpts:    retry,
		Logger:       logger,
	}
}

// downloadsHTML builds a minimal page containing a "Total downloads"
// marker plus a title="N" attribute that parseDownloads can extract.
func downloadsHTML(count string) string {
	return `<span>Total downloads</span><h3 title="` + count + `">` + count + `</h3>`
}

func TestClient_Name(t *testing.T) {
	c := NewClient(http.DefaultClient, Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	if got := c.Source().String(); got != "ghcr" {
		t.Errorf("Name() = %q, want ghcr", got)
	}
}

func TestParseDownloads_Valid(t *testing.T) {
	tests := []struct {
		name string
		html string
		want int64
	}{
		{"zero", downloadsHTML("0"), 0},
		{"small", downloadsHTML("42"), 42},
		{"large", downloadsHTML("999999999"), 999999999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDownloads(tt.html)
			if err != nil {
				t.Fatalf("parseDownloads: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseDownloads = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseDownloads_FormatChanged(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{"no Total downloads", "<div>nothing</div>"},
		{"no title attribute", "<span>Total downloads</span>\n<h3>176K</h3>"},
		{"non-numeric title", `<span>Total downloads</span><h3 title="abc">N/A</h3>`},
		{"truncated at marker", "<span>Total downloads</span>"},
		{"negative count", `<span>Total downloads</span><h3 title="-5">-5</h3>`},
		{"title unclosed", `<span>Total downloads</span><h3 title="12345>`},
		{"an element between the marker and the count", `<span>Total downloads</span><div class="foo">bar</div><h3 title="176000">176K</h3>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDownloads(tt.html)
			if !errors.Is(err, errHTMLFormatChanged) {
				t.Errorf("err = %v, want errHTMLFormatChanged", err)
			}
		})
	}
}

func TestParseDownloads_TitleBeyondMaxDistance(t *testing.T) {
	// The count element must start within maxTitleDistance (500 bytes) of
	// the marker; padding pushes it past the window, so nothing follows the
	// marker element and the parse fails closed.
	padding := strings.Repeat("x", 501)
	html := "<span>Total downloads</span>" + padding + `<h3 title="999">999</h3>`
	_, err := parseDownloads(html)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("err = %v, want errHTMLFormatChanged", err)
	}
}

// TestParseDownloads_UsesAssociatedCountElement pins that the count is the
// marker's OWN element rather than whatever titled element happens to be
// nearby: a titled element between the marker and the count, of any tag,
// makes the association ambiguous, so the sample is dropped behind the
// format sentinel instead of publishing a number that may belong to
// something else. The live shape passes, which is what keeps the strict
// rule honest against the page GitHub actually ships.
func TestParseDownloads_UsesAssociatedCountElement(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		want    int64
		wantErr bool
	}{
		{
			name:    "unrelated titled span before the count",
			html:    `<span>Total downloads</span><span title="1">rank</span><h3 title="27880">27.8K</h3>`,
			wantErr: true,
		},
		{
			name:    "unrelated titled h3 before the count",
			html:    `<span>Total downloads</span><h3 title="1">rank</h3><h3 title="27880">27.8K</h3>`,
			wantErr: true,
		},
		{
			name: "live shape",
			html: `<span>Total downloads</span> <h3 title="27880">27.8K</h3>`,
			want: 27880,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDownloads(tt.html)
			if tt.wantErr {
				if !errors.Is(err, errHTMLFormatChanged) {
					t.Fatalf("parseDownloads(%q) = (%d, %v), want errHTMLFormatChanged", tt.html, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDownloads(%q) error = %v", tt.html, err)
			}
			if got != tt.want {
				t.Errorf("parseDownloads(%q) = %d, want %d", tt.html, got, tt.want)
			}
		})
	}
}

func TestParsePackageList_Valid(t *testing.T) {
	html := `<a href="/users/owner/packages/container/package/app1">app1</a>
<a href="/users/owner/packages/container/package/app2">app2</a>
<a href="/users/owner/packages/container/package/app1">app1-dup</a>`
	got, _ := parsePackageList(html, "owner", userOwner)
	want := []string{"app1", "app2"}
	if len(got) != len(want) {
		t.Fatalf("got %d packages, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestParsePackageList_MultiplePerLine(t *testing.T) {
	// Multiple package links on the same line should all be extracted.
	html := `<a href="/users/o/packages/container/package/a">a</a><a href="/users/o/packages/container/package/b">b</a>`
	got, _ := parsePackageList(html, "o", userOwner)
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2 (%v)", len(got), got)
	}
}

// TestParsePackageList_Empty pins that the parse core reports zero names
// and nothing else: whether an empty page means an empty owner or markup
// drift depends on which page it is, which only the page loop knows.
func TestParsePackageList_Empty(t *testing.T) {
	got, refused := parsePackageList("<html>nothing here</html>", "owner", userOwner)
	if len(got) != 0 || refused.Count != 0 {
		t.Errorf("parsePackageList(empty page) = (%v, %+v), want no names and no refusals", got, refused)
	}
}

func TestFetchHTML_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := NewClient(http.DefaultClient, fastPacing(shortRetry(), testsupport.QuietLogger()))
	_, err := c.fetchHTML(ctx, "https://example.com")
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

// --- Migrated from main_test.go in chain step 4 ---

// TestFetchHTML_SendsBrowserHeaders verifies that fetchHTML installs
// the browser-like User-Agent / Accept / Accept-Language triplet
// GitHub requires for anonymous GHCR pages. Migrated from the legacy
// TestFetchGitHubHTMLSuccess which asserted the same headers against
// main.go's fetchGitHubHTML forwarder.
func TestFetchHTML_SendsBrowserHeaders(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("expected User-Agent header")
		}
		if r.Header.Get("Accept") != "text/html" {
			t.Errorf("Accept = %q, want text/html", r.Header.Get("Accept"))
		}
		if r.Header.Get("Accept-Language") == "" {
			t.Error("expected Accept-Language header")
		}
		w.Write([]byte("<html>test</html>"))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	html, err := c.fetchHTML(t.Context(), packagePageURL)
	if err != nil {
		t.Fatalf("fetchHTML: %v", err)
	}
	if html != "<html>test</html>" {
		t.Errorf("html = %q, want <html>test</html>", html)
	}
}

// TestParseDownloads_ContentBeforeTitle pins the offset calculation
// across a line boundary: the count element may sit on the line after the
// marker, indented, with whitespace before its title attribute. GitHub
// reflows that whitespace, so it is not format drift. Whatever else
// precedes the title inside the start tag (class, id, data-*) is the same
// byte scan, so one case covers them.
func TestParseDownloads_ContentBeforeTitle(t *testing.T) {
	tests := []struct {
		name string
		html string
		want int64
	}{
		{
			name: "whitespace before title",
			html: "<span>Total downloads</span>\n   <h3   title=\"7\">7</h3>",
			want: 7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := parseDownloads(tt.html)
			if err != nil {
				t.Fatalf("parseDownloads: %v", err)
			}
			if count != tt.want {
				t.Errorf("parseDownloads = %d, want %d", count, tt.want)
			}
		})
	}
}

// TestCollect_ExplicitMock exercises *Client.Collect against a mock
// server for a single explicit ref, asserting the returned entry's
// owner/repo pair and scraped download count plus healthy=true.
// Migrated from TestCollectGHCRExplicitMock in main_test.go and
// previously driven through the free-function ghcr.Collect shim.
func TestCollect_ExplicitMock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/mypkg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><span>Total downloads</span>
<h3 title="4567">4.6K</h3></html>`))
	})
	srv := httptest.NewTestServer(t, mux)

	client := srv.Client()
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "mypkg"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if !healthy {
		t.Error("expected healthy=true")
	}
	if len(entries) != 1 {
		t.Fatalf("Collect returned %d entries, want 1", len(entries))
	}
	if entries[0].Owner != "owner" || entries[0].Repo != "mypkg" {
		t.Errorf("entry ref = %s/%s, want owner/mypkg", entries[0].Owner, entries[0].Repo)
	}
	if entries[0].Pulls != 4567 {
		t.Errorf("Pulls = %d, want 4567", entries[0].Pulls)
	}
}

// TestCollect_WildcardMock exercises the wildcard branch end-to-end:
// owner listing returns two packages, each package page returns a
// download count. Migrated from TestCollectGHCRWildcardMock.
func TestCollect_WildcardMock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html>
<a href="/users/owner/packages/container/package/pkg1">pkg1</a>
<a href="/users/owner/packages/container/package/pkg2">pkg2</a>
</html>`))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><span>Total downloads</span>
<h3 title="100">100</h3></html>`))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/pkg2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><span>Total downloads</span>
<h3 title="200">200</h3></html>`))
	})
	srv := httptest.NewTestServer(t, mux)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	client := srv.Client()
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	entries, _, healthy := c.Collect(ctx, refs)

	if !healthy {
		t.Error("expected healthy=true")
	}
	if len(entries) != 2 {
		t.Fatalf("Collect returned %d entries, want 2", len(entries))
	}
}

// TestCollect_AllFailUnhealthy verifies that when every package scrape
// fails with a non-parse error (e.g. 500), the returned healthy flag
// is false and no zero-count entries are appended (a zero entry would
// inject a false zero into the exposed gauge; the per-day delta is
// computed downstream by Prometheus/Mimir). Migrated from
// TestCollectGHCRAllFailUnhealthy.
func TestCollect_AllFailUnhealthy(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	client := srv.Client()
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if healthy {
		t.Error("expected healthy=false when all scrapes fail")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries (failures skipped), got %d", len(entries))
	}
}

// TestCollect_AllParseFailures verifies that when every package scrape
// returns HTML that misses the download marker, Collect reports
// healthy=false and appends no zero-count entries. Migrated from
// TestCollectGHCRAllParseFailures; also pins the "majority parse
// failures" format-drift warning path.
func TestCollect_AllParseFailures(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><div>no download info here</div></html>`))
	}))

	client := srv.Client()
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if healthy {
		t.Error("expected healthy=false when all parse failures")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries (parse failures skipped), got %d", len(entries))
	}
}

// TestFetchHTML_OverCap_IsFormatChanged verifies that a GHCR page larger than
// ghcrBodyCap is surfaced as errHTMLFormatChanged (a markup/format signal) so
// it feeds the majority-format-drift escalation, rather than bubbling up as a
// generic transport error. httpx v2 returns a typed *ResponseTooLargeError on
// overflow (v1 silently truncated).
func TestFetchHTML_OverCap_IsFormatChanged(t *testing.T) {
	oversize := strings.Repeat("x", ghcrBodyCap+1)
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversize))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	_, err := c.fetchHTML(t.Context(), packagePageURL)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("fetchHTML over-cap error = %v, want errHTMLFormatChanged", err)
	}
	// The typed httpx error stays unwrappable for callers that want the limit.
	if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); !ok {
		t.Errorf("error = %v, want it to wrap *httpx.ResponseTooLargeError", err)
	}
}

// TestParsePackageList_SkipsMalformedAndEmptyNames covers scanLine's two
// defensive branches on malformed GHCR listing HTML: a package-link prefix
// with no closing delimiter ends the scan with no name, and a prefix
// immediately followed by a delimiter (an empty name) is refused without
// aborting the scan, so a later valid link on the same line is still parsed.
func TestParsePackageList_SkipsMalformedAndEmptyNames(t *testing.T) {
	tests := []struct {
		name        string
		html        string
		owner       string
		want        []string
		wantRefused int
	}{
		{
			name:  "prefix with no closing delimiter yields no packages",
			html:  `<a href="/users/owner/packages/container/package/app1`,
			owner: "owner",
		},
		{
			name:        "empty name is refused and a later valid link still parses",
			html:        `<a href="/users/owner/packages/container/package/"></a><a href="/users/owner/packages/container/package/real">real</a>`,
			owner:       "owner",
			want:        []string{"real"},
			wantRefused: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused := parsePackageList(tt.html, tt.owner, userOwner)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d packages %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if got[i] != w {
					t.Errorf("[%d] = %q, want %q", i, got[i], w)
				}
			}
			if refused.Count != tt.wantRefused {
				t.Errorf("refused.Count = %d, want %d", refused.Count, tt.wantRefused)
			}
		})
	}
}

// TestBuildPackageListSharedKeyEncodingPreventsDuplicates guards the invariant
// that makes the two dedup sites correct: the wildcard pass and the
// explicit-ref pass write into ONE `seen` map, so they must encode a pair
// identically. If only one of them were changed, the explicit ref would no
// longer match its wildcard twin and the same package would be returned twice,
// scraped twice and exported twice.
//
// Set membership cannot see a duplicate, so this test counts occurrences per
// ref, which is the part that fails if the two encodings ever drift apart.
func TestBuildPackageListSharedKeyEncodingPreventsDuplicates(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/owner" && r.URL.Query().Get("tab") == "packages" {
			_, _ = w.Write([]byte(`<a href="/users/owner/packages/container/package/app1">app1</a>
<a href="/users/owner/packages/container/package/app2">app2</a>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	refs := []registry.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "app1"}, // already found by the wildcard
		{Owner: "owner", Repo: "app2"}, // already found by the wildcard
		{Owner: "owner", Repo: "app3"}, // genuinely new
	}
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	packages, whollyFailed, parseFail := c.buildPackageList(t.Context(), refs)
	if whollyFailed || parseFail != 0 {
		t.Fatalf("listing failures: whollyFailed=%v parseFail=%d", whollyFailed, parseFail)
	}

	counts := make(map[registry.RepoRef]int, len(packages))
	for _, p := range packages {
		counts[p]++
	}
	for ref, n := range counts {
		if n != 1 {
			t.Errorf("package %s/%s returned %d times, want exactly 1 (the wildcard and explicit passes disagree on the dedup key encoding)",
				ref.Owner, ref.Repo, n)
		}
	}
	if len(packages) != 3 {
		t.Errorf("len(packages) = %d, want 3 (app1, app2, app3); got %+v", len(packages), packages)
	}
}

// packageLink renders one package link of the form a listing page of kind
// carries, so a fixture cannot drift from the prefix the parser matches.
func packageLink(kind ownerKind, owner, name string) string {
	return `<a href="` + linkPrefix(kind, owner) + name + `">` + name + `</a>`
}

// listingClient returns a Client whose listing reads are capped at pageCap
// pages and paced in microseconds.
func listingClient(t *testing.T, h http.Handler, logger *slog.Logger, pageCap int) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	c := NewClient(srv.Client(), fastPacing(shortRetry(), logger))
	c.pageCap = pageCap
	return c
}

// TestClient_ScrapePackageList_PaginatesOwnerListing pins the page loop: an
// owner listing longer than one page is read to its end and the names are
// unioned in listing order, and the read stops clean — no WARN — when a page
// carries GitHub's own last-page marker or adds no new name.
func TestClient_ScrapePackageList_PaginatesOwnerListing(t *testing.T) {
	tests := []struct {
		name  string
		pages map[string]string
		want  []string
	}{
		{
			name: "stops on the positive last-page marker",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a") + packageLink(userOwner, "owner", "b"),
				"2": packageLink(userOwner, "owner", "c") + `<div class="next_page disabled">Next</div>`,
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "stops on a page that adds no name",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a"),
				"2": packageLink(userOwner, "owner", "b"),
				"3": `<html>no package links</html>`,
			},
			want: []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			asked := map[string]int{}
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				asked[page]++
				_, _ = w.Write([]byte(tt.pages[page]))
			}), capturingLogger(&buf), 5)

			got, refused, err := c.scrapePackageList(t.Context(), "owner")
			if err != nil {
				t.Fatalf("scrapePackageList: %v", err)
			}
			if refused.Count != 0 {
				t.Errorf("refusals = %+v, want none", refused)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("scrapePackageList = %v, want %v (every page, in listing order)", got, tt.want)
			}
			if logs := buf.String(); strings.Contains(logs, "hit page cap") || strings.Contains(logs, "partially failed") {
				t.Errorf("a listing that ended normally warned; logs:\n%s", logs)
			}
			if asked["1"] != 1 {
				t.Errorf("page 1 requested %d times, want 1 (the redirecting form would re-fetch it)", asked["1"])
			}
		})
	}
}

// TestClient_ScrapePackageList_PageCapWarnsOnTruncation pins the WARN an
// operator's alert keys on: with names still arriving when the bound bites,
// the listing is truncated and says so with the literal alerts.yaml matches.
func TestClient_ScrapePackageList_PageCapWarnsOnTruncation(t *testing.T) {
	var buf bytes.Buffer
	pages := 0
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "p"+r.URL.Query().Get("page"))))
	}), capturingLogger(&buf), 2)

	got, _, err := c.scrapePackageList(t.Context(), "owner")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if len(got) != 2 || pages != 2 {
		t.Errorf("scrapePackageList = %v after %d pages, want 2 names from 2 pages (pageCap=2)", got, pages)
	}
	logs := buf.String()
	if !strings.Contains(logs, "hit page cap") || !strings.Contains(logs, "max_pages=2") {
		t.Errorf("a truncated listing did not warn with the `hit page cap` literal and max_pages; logs:\n%s", logs)
	}
}

// TestClient_ScrapePackageList_LaterPageFailureIsPartial pins the other
// literal alerts.yaml keys on: a page after the first failing is a PARTIAL
// listing, so the names already read come back with the error rather than
// being discarded, and the owner is not reported as wholly failed.
func TestClient_ScrapePackageList_LaterPageFailureIsPartial(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a")))
	}), capturingLogger(&buf), 5)

	got, _, err := c.scrapePackageList(t.Context(), "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want the page-2 failure reported")
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("scrapePackageList = %v, want the page-1 name kept alongside the error", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "listing partially failed") {
		t.Errorf("a partial listing did not warn with the `listing partially failed` literal; logs:\n%s", logs)
	}

	_, whollyFailed, _ := c.buildPackageList(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if whollyFailed {
		t.Error("buildPackageList reported a wholly failed listing, want partial (page 1 yielded a name)")
	}
}

// TestClient_ScrapePackageList_ReadsOrganizationForm pins the owner-kind
// probe: an organization's profile page carries no package links, so the
// listing is re-read in the /orgs form, whose links carry their own prefix.
func TestClient_ScrapePackageList_ReadsOrganizationForm(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /myorg", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/myorg/packages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(packageLink(orgOwner, "myorg", "svc")))
	})
	c := listingClient(t, mux, testsupport.QuietLogger(), 5)

	got, _, err := c.scrapePackageList(t.Context(), "myorg")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if !slices.Equal(got, []string{"svc"}) {
		t.Errorf("scrapePackageList = %v, want the organization's packages", got)
	}
}

// TestClient_ScrapePackageList_EmptyFirstPageNamesBothCauses pins the
// diagnostic for the one state that stays ambiguous after the kind probe: a
// user with no container packages and a user page whose link markup changed
// are indistinguishable, so the error names both, the checkable one first.
func TestClient_ScrapePackageList_EmptyFirstPageNamesBothCauses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // not an organization: this owner is a user
	})
	c := listingClient(t, mux, testsupport.QuietLogger(), 5)

	got, _, err := c.scrapePackageList(t.Context(), "nobody")
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList = (%v, %v), want errHTMLFormatChanged", got, err)
	}
	for _, want := range []string{"check the owner name", "the listing markup changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("scrapePackageList error = %q, want it to name %q", err, want)
		}
	}
}

// TestClient_ExpandWildcard_BoundsRefusedNameSample pins the one answer an
// operator has to "why is my package missing": the count of names the
// charset gate refused plus one sample, bounded at the emit site because
// the sample is raw bytes off a scraped page — a single unterminated
// attribute can otherwise reach the log stream megabytes wide.
func TestClient_ExpandWildcard_BoundsRefusedNameSample(t *testing.T) {
	var buf bytes.Buffer
	huge := strings.Repeat("%", 4096)
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<a href="` + linkPrefix(userOwner, "owner") + huge + `">bad</a>` +
			packageLink(userOwner, "owner", "good")))
	}), capturingLogger(&buf), 1)

	packages, whollyFailed, _ := c.buildPackageList(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if whollyFailed || len(packages) != 1 || packages[0].Repo != "good" {
		t.Fatalf("buildPackageList = (%+v, whollyFailed=%v), want just owner/good", packages, whollyFailed)
	}

	logs := buf.String()
	if !strings.Contains(logs, "refused=1") {
		t.Errorf("the refused-name record does not carry the count; logs:\n%s", logs)
	}
	if len(logs) > 1024 {
		t.Errorf("one refused 4096-byte name produced a %d-byte log; want the sample bounded", len(logs))
	}
}
