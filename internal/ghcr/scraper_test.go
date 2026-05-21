package ghcr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"registry-stats/internal/api"
	"registry-stats/internal/httpx"
	"registry-stats/internal/model"
	"registry-stats/internal/testsupport"
)

// Compile-time assertion kept here so a change that narrows
// api.RegistrySource past what *Client satisfies trips the build in
// the test binary before reaching any other consumer.
var _ api.RegistrySource = (*Client)(nil)

func mockClient(srv *httptest.Server) *http.Client {
	return testsupport.MockClient(srv)
}

// shortRetry returns httpx.Options with a 1 ms base delay so retry
// tests don't wait a full second between attempts.
func shortRetry() httpx.Options {
	return httpx.Options{BaseDelay: time.Millisecond}
}

// fastPacing returns Options with microsecond pacing/jitter so mock
// Collect tests don't sit on the 2-5 s production per-package delay
// baked into DefaultPacingMin / DefaultPacingJitter. Non-mock tests
// that intentionally exercise default pacing should keep passing
// Options{} (zero value falls back to the DefaultPacing* constants).
func fastPacing() Options {
	return Options{
		PacingMin:    time.Microsecond,
		PacingJitter: time.Microsecond,
	}
}

// downloadsHTML builds a minimal page containing a "Total downloads"
// marker plus a title="N" attribute that ParseDownloads can extract.
func downloadsHTML(count string) string {
	return `<span>Total downloads</span><h3 title="` + count + `">` + count + `</h3>`
}

func TestClient_Name(t *testing.T) {
	c := NewClient(http.DefaultClient, shortRetry(), Options{}, testsupport.QuietLogger())
	if got := c.Name(); got != "ghcr" {
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
		{
			name: "content before title on same line",
			html: `<span>Total downloads</span><div class="foo">bar</div><h3 title="176000">176K</h3>`,
			want: 176000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDownloads(tt.html)
			if err != nil {
				t.Fatalf("ParseDownloads: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseDownloads = %d, want %d", got, tt.want)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDownloads(tt.html)
			if !errors.Is(err, ErrHTMLFormatChanged) {
				t.Errorf("err = %v, want ErrHTMLFormatChanged", err)
			}
		})
	}
}

func TestParseDownloads_TitleBeyondMaxDistance(t *testing.T) {
	// The maxTitleDistance cap (500 chars) truncates the search window.
	// A title attribute placed beyond that distance should trigger
	// ErrHTMLFormatChanged because the truncated rest no longer contains it.
	padding := strings.Repeat("x", 501)
	html := "<span>Total downloads</span>" + padding + `<h3 title="999">999</h3>`
	_, err := ParseDownloads(html)
	if !errors.Is(err, ErrHTMLFormatChanged) {
		t.Errorf("err = %v, want ErrHTMLFormatChanged", err)
	}
}

func TestParsePackageList_Valid(t *testing.T) {
	html := `<a href="/users/owner/packages/container/package/app1">app1</a>
<a href="/users/owner/packages/container/package/app2">app2</a>
<a href="/users/owner/packages/container/package/app1">app1-dup</a>`
	got, err := ParsePackageList(html, "owner")
	if err != nil {
		t.Fatalf("ParsePackageList: %v", err)
	}
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
	got, err := ParsePackageList(html, "o")
	if err != nil {
		t.Fatalf("ParsePackageList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2 (%v)", len(got), got)
	}
}

func TestParsePackageList_UnsafeNames(t *testing.T) {
	// Unsafe names (path traversal attempts) must be filtered out.
	html := `<a href="/users/owner/packages/container/package/good">good</a>
<a href="/users/owner/packages/container/package/bad%2Fapp">bad</a>
<a href="/users/owner/packages/container/package/also-good">also</a>`
	got, err := ParsePackageList(html, "owner")
	if err != nil {
		t.Fatalf("ParsePackageList: %v", err)
	}
	// bad%2Fapp contains % which is not in the safe-segment set.
	for _, name := range got {
		if strings.Contains(name, "%") {
			t.Errorf("unsafe name %q leaked through filter", name)
		}
	}
}

func TestParsePackageList_Empty(t *testing.T) {
	_, err := ParsePackageList("<html>nothing here</html>", "owner")
	if !errors.Is(err, ErrHTMLFormatChanged) {
		t.Errorf("err = %v, want ErrHTMLFormatChanged", err)
	}
}

func TestFetchHTML_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html>test</html>"))
	}))
	defer srv.Close()

	html, err := fetchHTML(t.Context(), srv.Client(), srv.URL, shortRetry())
	if err != nil {
		t.Fatalf("fetchHTML: %v", err)
	}
	if html != "<html>test</html>" {
		t.Errorf("got %q, want <html>test</html>", html)
	}
}

func TestFetchHTML_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := fetchHTML(t.Context(), srv.Client(), srv.URL, shortRetry())
	if err == nil {
		t.Error("expected error for 403 response")
	}
}

func TestFetchHTML_InvalidURL(t *testing.T) {
	_, err := fetchHTML(t.Context(), http.DefaultClient, "://invalid", shortRetry())
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestFetchHTML_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fetchHTML(ctx, http.DefaultClient, "https://example.com", shortRetry())
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestScrapeDownloads_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(downloadsHTML("98765")))
	}))
	defer srv.Close()
	client := mockClient(srv)

	got, err := scrapeDownloads(t.Context(), client, "owner", "mypkg", shortRetry())
	if err != nil {
		t.Fatalf("scrapeDownloads: %v", err)
	}
	if got != 98765 {
		t.Errorf("scrapeDownloads = %d, want 98765", got)
	}
}

func TestScrapePackageList_FetchError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := scrapePackageList(ctx, http.DefaultClient, "testowner", shortRetry())
	if err == nil {
		t.Error("expected error when fetch fails")
	}
}

func TestScrapeDownloads_FetchError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := scrapeDownloads(ctx, http.DefaultClient, "owner", "pkg", shortRetry())
	if err == nil {
		t.Error("expected error when fetch fails")
	}
}

func TestBuildPackageList_Explicit(t *testing.T) {
	refs := []model.RepoRef{
		{Owner: "o", Repo: "a"},
		{Owner: "o", Repo: "b"},
	}
	packages, listFail, parseFail := buildPackageList(t.Context(), http.DefaultClient, refs, shortRetry())
	if listFail != 0 || parseFail != 0 {
		t.Errorf("expected no listing failures, got listFail=%d parseFail=%d", listFail, parseFail)
	}
	if len(packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(packages))
	}
}

func TestBuildPackageList_WildcardDedup(t *testing.T) {
	// ParsePackageList + dedup: an explicit ref that matches an
	// already-expanded wildcard entry should not duplicate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/owner/packages") && !strings.Contains(r.URL.Path, "/container/") {
			w.Write([]byte(`<a href="/users/owner/packages/container/package/app1">app1</a>
<a href="/users/owner/packages/container/package/app2">app2</a>`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := mockClient(srv)

	refs := []model.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "app1"}, // duplicate of wildcard result
		{Owner: "owner", Repo: "app3"}, // genuinely new
	}
	packages, listFail, parseFail := buildPackageList(t.Context(), client, refs, shortRetry())
	if listFail != 0 || parseFail != 0 {
		t.Fatalf("listing failures: listFail=%d parseFail=%d", listFail, parseFail)
	}
	// app1, app2 (wildcard), app3 (explicit new). app1 explicit is skipped.
	wantNames := map[string]bool{"owner/app1": false, "owner/app2": false, "owner/app3": false}
	for _, p := range packages {
		key := p.Owner + "/" + p.Repo
		if _, ok := wantNames[key]; !ok {
			t.Errorf("unexpected package %q", key)
		}
		wantNames[key] = true
	}
	for key, seen := range wantNames {
		if !seen {
			t.Errorf("missing package %q", key)
		}
	}
}

func TestBuildPackageList_WildcardListingError(t *testing.T) {
	// A wildcard listing that fails to fetch should bump listFail but
	// leave explicit refs flowing through.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := mockClient(srv)

	refs := []model.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "explicit"},
	}
	packages, listFail, _ := buildPackageList(t.Context(), client, refs, shortRetry())
	if listFail != 1 {
		t.Errorf("listFail = %d, want 1", listFail)
	}
	// The explicit ref still shows up.
	found := false
	for _, p := range packages {
		if p.Owner == "owner" && p.Repo == "explicit" {
			found = true
		}
	}
	if !found {
		t.Error("explicit ref missing after wildcard listing error")
	}
}

func TestParseDownloads_NeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		html := rapid.String().Draw(t, "html")
		_, _ = ParseDownloads(html) // must not panic
	})
}

func TestParsePackageList_NeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		html := rapid.String().Draw(t, "html")
		owner := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "owner")
		_, _ = ParsePackageList(html, owner) // must not panic
	})
}

func TestErrHTMLFormatChanged_Sentinel(t *testing.T) {
	// ErrHTMLFormatChanged must be the root for errors.Is matching on
	// wrapped errors returned from ParseDownloads.
	_, err := ParseDownloads(`<span>Total downloads</span><h3 title="abc">x</h3>`)
	if !errors.Is(err, ErrHTMLFormatChanged) {
		t.Errorf("errors.Is(..., ErrHTMLFormatChanged) = false for wrapped parse error")
	}
}

// --- Migrated from main_test.go in chain step 4 ---

// TestFetchHTML_SendsBrowserHeaders verifies that fetchHTML installs
// the browser-like User-Agent / Accept / Accept-Language triplet
// GitHub requires for anonymous GHCR pages. Migrated from the legacy
// TestFetchGitHubHTMLSuccess which asserted the same headers against
// main.go's fetchGitHubHTML forwarder.
func TestFetchHTML_SendsBrowserHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	html, err := fetchHTML(t.Context(), srv.Client(), srv.URL, shortRetry())
	if err != nil {
		t.Fatalf("fetchHTML: %v", err)
	}
	if html != "<html>test</html>" {
		t.Errorf("html = %q, want <html>test</html>", html)
	}
}

// TestParseDownloads_ContentBeforeTitle expands the single
// "content before title" case in TestParseDownloads_Valid with a
// table migrated from the mutation-hunt test in main_test.go. Pins
// the offset calculation for title attributes preceded by class,
// id/style, whitespace, or data-* attributes on the same line.
func TestParseDownloads_ContentBeforeTitle(t *testing.T) {
	tests := []struct {
		name string
		html string
		want int64
	}{
		{
			name: "class before title",
			html: "<span>Total downloads</span>\n<h3 class=\"text-bold\" title=\"42\">42</h3>",
			want: 42,
		},
		{
			name: "id and style before title",
			html: "<span>Total downloads</span>\n<h3 id=\"count\" style=\"color:red\" title=\"999\">999</h3>",
			want: 999,
		},
		{
			name: "whitespace before title",
			html: "<span>Total downloads</span>\n   <h3   title=\"7\">7</h3>",
			want: 7,
		},
		{
			name: "data attribute before title",
			html: "<span>Total downloads</span>\n<h3 data-value=\"x\" title=\"12345\">12.3K</h3>",
			want: 12345,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := ParseDownloads(tt.html)
			if err != nil {
				t.Fatalf("ParseDownloads: %v", err)
			}
			if count != tt.want {
				t.Errorf("ParseDownloads = %d, want %d", count, tt.want)
			}
		})
	}
}

// TestCollect_ExplicitMock exercises *Client.Collect against a mock
// server for a single explicit ref, asserting the returned entry's
// Name and DownloadCount plus healthy=true. Migrated from
// TestCollectGHCRExplicitMock in main_test.go and previously driven
// through the free-function ghcr.Collect shim.
func TestCollect_ExplicitMock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages/container/package/mypkg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><span>Total downloads</span>
<h3 title="4567">4.6K</h3></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := mockClient(srv)
	c := NewClient(client, shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "mypkg"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if !healthy {
		t.Error("expected healthy=true")
	}
	if len(entries) != 1 {
		t.Fatalf("Collect returned %d entries, want 1", len(entries))
	}
	if entries[0].Name != "owner/mypkg" {
		t.Errorf("Name = %q, want %q", entries[0].Name, "owner/mypkg")
	}
	if entries[0].DownloadCount != 4567 {
		t.Errorf("DownloadCount = %d, want 4567", entries[0].DownloadCount)
	}
}

// TestCollect_WildcardMock exercises the wildcard branch end-to-end:
// owner listing returns two packages, each package page returns a
// download count. Migrated from TestCollectGHCRWildcardMock.
func TestCollect_WildcardMock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	client := mockClient(srv)
	c := NewClient(client, shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
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
// poison the daily-delta series). Migrated from
// TestCollectGHCRAllFailUnhealthy.
func TestCollect_AllFailUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := mockClient(srv)
	c := NewClient(client, shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><div>no download info here</div></html>`))
	}))
	defer srv.Close()

	client := mockClient(srv)
	c := NewClient(client, shortRetry(), fastPacing(), testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	entries, _, healthy := c.Collect(t.Context(), refs)

	if healthy {
		t.Error("expected healthy=false when all parse failures")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries (parse failures skipped), got %d", len(entries))
	}
}

// --- Fuzz targets (stdlib) ---

func FuzzParseDownloads(f *testing.F) {
	// Seed corpus from existing test fixtures.
	f.Add(`<span>Total downloads</span><h3 title="0">0</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="42">42</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="999999999">999999999</h3>`)
	f.Add(`<span>Total downloads</span><div class="foo">bar</div><h3 title="176000">176K</h3>`)
	f.Add("<div>nothing</div>")
	f.Add(`<span>Total downloads</span><h3 title="abc">N/A</h3>`)
	f.Add("")
	f.Fuzz(func(t *testing.T, html string) {
		count, err := ParseDownloads(html)
		if err == nil && count < 0 {
			t.Errorf("ParseDownloads returned negative count %d without error", count)
		}
	})
}

func FuzzParsePackageList(f *testing.F) {
	// Seed corpus from existing test fixtures.
	f.Add(`<a href="/users/owner/packages/container/package/app1">app1</a>`, "owner")
	f.Add(`<a href="/users/o/packages/container/package/a">a</a><a href="/users/o/packages/container/package/b">b</a>`, "o")
	f.Add("<html>nothing here</html>", "owner")
	f.Add("", "owner")
	f.Add(`<a href="/users/owner/packages/container/package/good">good</a>`, "owner")
	f.Fuzz(func(t *testing.T, html, owner string) {
		pkgs, err := ParsePackageList(html, owner)
		if err == nil && len(pkgs) == 0 {
			// ParsePackageList returns ErrHTMLFormatChanged when no packages found,
			// so err==nil with empty result should not happen.
			t.Errorf("ParsePackageList returned nil error with 0 packages")
		}
	})
}
