package ghcr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
)

// packagePageURL is a representative production GHCR package-page URL:
// httptest.NewTestServer's in-memory client routes every request to the
// handler regardless of scheme, host or address and leaves Server.URL
// empty, so these tests pass the URL production would build.
const packagePageURL = "https://github.com/users/owner/packages/container/package/pkg"

// shortRetry returns httpx options with a 1 ms base delay so retry
// tests don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
}

// fastPacing returns Options with microsecond pacing/jitter so mock
// Collect tests don't sit on the production per-package delay.
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
		{"attribute value resembling title", `<span>Total downloads</span><h3 data-tip='title="1"' title="27800">27.8K</h3>`, 27800},
		{"distant whitespace", `<span>Total downloads</span>` + strings.Repeat(" ", 501) + `<h3 title="9">9</h3>`, 9},
		{"later titled element", `<span>Total downloads</span><h3 title="7">7</h3><span title="1">rank</span>`, 7},
		{"form feed before count", "<span>Total downloads</span>\f<h3 title=\"8\">8</h3>", 8},
		{"valueless attribute before the count", `<span>Total downloads</span><h3 hidden title="9">9</h3>`, 9},
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

func TestParseDownloads_FindsMarkerText(t *testing.T) {
	tests := []struct {
		name string
		html string
		want int64
	}{
		{
			name: "attribute value with greater-than byte",
			html: `<div data-label="before>Total downloads">noise</div><span>Total downloads</span><h3 title="7">7</h3>`,
			want: 7,
		},
		{
			name: "script less-than byte",
			html: `<script>if (a<b) {}</script><span>Total downloads</span><h3 title="8">8</h3>`,
			want: 8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDownloads(tt.html)
			if err != nil || got != tt.want {
				t.Errorf("parseDownloads(%q) = (%d, %v), want (%d, nil)", tt.html, got, err, tt.want)
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
		{"non-whitespace between marker and count", "<span>Total downloads</span>" + strings.Repeat("x", 501) + `<h3 title="999">999</h3>`},
		{"title bytes inside attribute value", `<span>Total downloads</span><h3 data-tip='title="1"'>27.8K</h3>`},
		{"title bytes after other text inside attribute value", `<span>Total downloads</span><h3 data-tip='x title="1"'>27.8K</h3>`},
		{"two title attributes", `<span>Total downloads</span><h3 title="1" title="2">2</h3>`},
		{"attribute name ending in title", `<span>Total downloads</span><h3 data-title="9">27.8K</h3>`},
		{"element name beginning with h3", `<span>Total downloads</span><h3x title="9">27.8K</h3x>`},
		{"two download-count markers", downloadsHTML("1") + downloadsHTML("2")},
		{"real marker and a raw-text marker", `<script>>Total downloads</script><h3 title="999">999</h3>` + downloadsHTML("7")},
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

func TestParseDownloads_CommentOnlyMarkerIsIgnored(t *testing.T) {
	const html = `<!-- <span>Total downloads</span><h3 title="999">999</h3> -->`
	count, err := parseDownloads(html)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("parseDownloads(comment-only marker) = (%d, %v), want errHTMLFormatChanged", count, err)
	}
}

func TestParseDownloads_CommentEndBeforeRealMarker(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{name: "ordinary_close", html: `<!-- <span>Total downloads</span> -->` + downloadsHTML("7")},
		{name: "bang_close", html: `<!-- <span>Total downloads</span> --!>` + downloadsHTML("7")},
		{name: "abrupt_close", html: `<!-->` + downloadsHTML("7")},
		{name: "abrupt_dash_close", html: `<!--->` + downloadsHTML("7")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count, err := parseDownloads(tt.html)
			if err != nil || count != 7 {
				t.Errorf("parseDownloads(%q) = (%d, %v), want (7, nil)", tt.html, count, err)
			}
		})
	}
}

func TestParseDownloads_CommentOverlapKeepsDocumentOrder(t *testing.T) {
	t.Run("one_real_marker", func(t *testing.T) {
		html := `<!--x<!--!>` + downloadsHTML("7")
		count, err := parseDownloads(html)
		if err != nil || count != 7 {
			t.Errorf("parseDownloads(comment overlap + one marker) = (%d, %v), want (7, nil)", count, err)
		}
	})

	t.Run("two_real_markers", func(t *testing.T) {
		html := downloadsHTML("1") + `<!--x<!--!>` + downloadsHTML("2")
		count, err := parseDownloads(html)
		if !errors.Is(err, errHTMLFormatChanged) {
			t.Errorf("parseDownloads(two markers around comment overlap) = (%d, %v), want errHTMLFormatChanged", count, err)
		}
	})
}

func TestParsePackageList_Valid(t *testing.T) {
	html := `<a href="/users/owner/packages/container/package/app1">app1</a>
<a href="/users/owner/packages/container/package/app2">app2</a>
<a href="/users/owner/packages/container/package/app1">app1-dup</a>`
	got, _, _ := parsePackageList(html, "owner", userOwner)
	want := []string{"app1", "app2", "app1"}
	if len(got) != len(want) {
		t.Fatalf("got %d packages, want %d (%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestParsePackageList_SingleQuotedHref(t *testing.T) {
	html := `<a href='/users/owner/packages/container/package/app'>app</a>`
	got, refused, _ := parsePackageList(html, "owner", userOwner)
	if !slices.Equal(got, []string{"app"}) || refused.Count != 0 {
		t.Errorf("parsePackageList(single-quoted href) = (%v, %+v), want ([app], no refusals)", got, refused)
	}
}

// TestParsePackageList_MultiplePerLine pins that multiple package links
// on the same HTML line are all extracted.
func TestParsePackageList_MultiplePerLine(t *testing.T) {
	html := `<a href="/users/o/packages/container/package/a">a</a><a href="/users/o/packages/container/package/b">b</a>`
	got, _, _ := parsePackageList(html, "o", userOwner)
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2 (%v)", len(got), got)
	}
}

func TestParsePackageList_DecodesNestedNames(t *testing.T) {
	for _, encoded := range []string{"helm-charts%2Fgrafana-operator", "helm-charts%2fgrafana-operator"} {
		html := `<a href="/users/owner/packages/container/package/` + encoded + `">package</a>`
		got, refused, _ := parsePackageList(html, "owner", userOwner)
		if !slices.Equal(got, []string{"helm-charts/grafana-operator"}) || refused.Count != 0 {
			t.Errorf("parsePackageList(%q) = (%v, %+v), want decoded nested name", encoded, got, refused)
		}
	}
}

func TestParsePackageList_BoundsWholeName(t *testing.T) {
	atBound := strings.Repeat("a", urlsafe.MaxSegmentBytes-len("owner"+"/"))
	overBound := atBound + "a"
	tests := []struct {
		name        string
		token       string
		want        []string
		wantRefused int
	}{
		{name: "at bound", token: atBound, want: []string{atBound}},
		{name: "over bound", token: overBound, wantRefused: 1},
		{name: "many short elements", token: strings.Repeat("a%2F", 126) + "a", wantRefused: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := `<a href="/users/owner/packages/container/package/` + tt.token + `">package</a>`
			got, refused, _ := parsePackageList(html, "owner", userOwner)
			if !slices.Equal(got, tt.want) || refused.Count != tt.wantRefused {
				t.Errorf("parsePackageList(%q) = (%v, %+v), want (%v, refused=%d)", tt.token, got, refused, tt.want, tt.wantRefused)
			}
		})
	}
}

func TestParsePackageList_RefusesUnsafeDecodedNames(t *testing.T) {
	for _, encoded := range []string{"..%2f..%2fetc%2fpasswd", "%2e%2e%2f%2e%2e", "%2e", "a//b", "/abs", "a%2F", "bad%00name", "%zz"} {
		html := `<a href="/users/owner/packages/container/package/` + encoded + `">package</a>`
		got, refused, _ := parsePackageList(html, "owner", userOwner)
		if len(got) != 0 || refused.Count != 1 {
			t.Errorf("parsePackageList(%q) = (%v, %+v), want one refusal", encoded, got, refused)
		}
	}
}

func TestParsePackageList_RefusesInvalidUTF8Name(t *testing.T) {
	const token = "%FF"
	html := `<a href="/users/owner/packages/container/package/` + token + `">package</a>`

	got, refused, _ := parsePackageList(html, "owner", userOwner)

	if len(got) != 0 || refused.Count != 1 {
		t.Errorf("parsePackageList(%q) = (%v, %+v), want one refusal", token, got, refused)
	}
}

// A candidate is the value of a real href attribute. An unsafe name inside one
// is refused and counted; bytes that merely LOOK like a package link — in a
// non-href attribute, or in an attribute whose name only ends in "href" — are
// not candidates at all, so they yield neither a name nor a refusal. A page
// made only of those reaches the caller as an empty listing, which
// emptyListingError reports as format drift.
func TestParsePackageList_ConsumesEachCandidate(t *testing.T) {
	prefix := linkPrefix(userOwner, "owner")
	tests := []struct {
		name        string
		html        string
		want        []string
		wantRefused int
	}{
		{
			name:        "unsafe candidate containing package prefix",
			html:        `<a href="` + prefix + `..` + prefix + `phantom">bad</a>`,
			wantRefused: 1,
		},
		{
			name:        "newline inside candidate",
			html:        `<a href="` + prefix + "na\nme" + `">bad</a>`,
			wantRefused: 1,
		},
		{
			name:        "non-link attribute candidates",
			html:        `<div data-targets="` + prefix + `a ` + prefix + `b">`,
			wantRefused: 0,
		},
		{
			name:        "attribute name ending in href",
			html:        `<div data-href="` + prefix + `a">`,
			wantRefused: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused, _ := parsePackageList(tt.html, "owner", userOwner)
			if !slices.Equal(got, tt.want) || refused.Count != tt.wantRefused {
				t.Errorf("parsePackageList(%q) = (%v, %+v), want (%v, refused=%d)", tt.html, got, refused, tt.want, tt.wantRefused)
			}
		})
	}
}

func TestParsePackageList_FoldsRegisteredOwnerCasing(t *testing.T) {
	prefix := "/users/NVIDIA/packages/container/package/"
	tests := []struct {
		name string
		html string
	}{
		{"registered casing", `<a href="` + prefix + `CUDA">package</a>`},
		{"other owner before package", `<a href="/users/other/packages/container/package/wrong">wrong</a><a href="` + prefix + `CUDA">package</a>`},
		{"unicode before package", strings.Repeat("Ⱥ", 200) + `<a href="` + prefix + `CUDA">package</a>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused, _ := parsePackageList(tt.html, "nvidia", userOwner)
			if !slices.Equal(got, []string{"CUDA"}) || refused.Count != 0 {
				t.Errorf("parsePackageList = (%v, %+v), want registered package casing preserved", got, refused)
			}
		})
	}
}

func TestClient_ScrapePackageList_UnreadablePackageAttributeFailsClosed(t *testing.T) {
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		html := `<span>2 packages</span><a title=x href="/users/owner/packages/container/package/lost">lost</a>` +
			packageLink(userOwner, "owner", "good")
		_, _ = w.Write([]byte(html))
	}), testsupport.QuietLogger())

	names, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("scrapePackageList unreadable package attribute error = %v, want errHTMLFormatChanged", err)
	}
	if len(names) != 0 || refused.Count != 0 {
		t.Errorf("scrapePackageList unreadable package attribute = (%v, %+v), want the inconsistent page discarded alongside the format error", names, refused)
	}
}

func TestParsePackageList_BoundsRefusalSample(t *testing.T) {
	raw := strings.Repeat("a", 256)
	html := `<a href="/users/owner/packages/container/package/` + raw + `">package</a>`
	got, refused, _ := parsePackageList(html, "owner", userOwner)
	if len(got) != 0 || refused.Count != 1 {
		t.Fatalf("parsePackageList(overlong name) = (%v, %+v), want one refusal", got, refused)
	}
	if refused.Sample != strings.Repeat("a", maxRefusalSampleBytes) {
		t.Errorf("refused.Sample = %q (%d bytes), want first %d bytes", refused.Sample, len(refused.Sample), maxRefusalSampleBytes)
	}
}

func TestParsePackageList_RefusalSampleDoesNotRetainPage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"under the sample cap", "%zz", "%zz"},
		{"truncated to the sample cap", strings.Repeat("a", 256), strings.Repeat("a", maxRefusalSampleBytes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := `<a href="/users/owner/packages/container/package/` + tt.raw + `">bad</a>` +
				strings.Repeat("x", 512<<10)
			_, refused, _ := parsePackageList(html, "owner", userOwner)
			if refused.Sample != tt.want {
				t.Fatalf("parsePackageList refusal sample = %q, want %q", refused.Sample, tt.want)
			}

			pageStart := uintptr(unsafe.Pointer(unsafe.StringData(html)))
			sampleStart := uintptr(unsafe.Pointer(unsafe.StringData(refused.Sample)))
			if sampleStart >= pageStart && sampleStart < pageStart+uintptr(len(html)) {
				t.Error("parsePackageList refusal sample shares the listing page's backing array")
			}
		})
	}
}

func TestParsePackageList_NameDoesNotRetainPage(t *testing.T) {
	html := packageLink(userOwner, "owner", "app") + strings.Repeat("x", 512<<10)
	names, refused, err := parsePackageList(html, "owner", userOwner)
	if err != nil || !slices.Equal(names, []string{"app"}) || refused.Count != 0 {
		t.Fatalf("parsePackageList = (%v, %+v, %v), want ([app], no refusals, no error)", names, refused, err)
	}

	pageStart := uintptr(unsafe.Pointer(unsafe.StringData(html)))
	nameStart := uintptr(unsafe.Pointer(unsafe.StringData(names[0])))
	if nameStart >= pageStart && nameStart < pageStart+uintptr(len(html)) {
		t.Error("parsePackageList package name shares the listing page's backing array")
	}
}

func TestParsePackageList_KeepsFirstInformativeSample(t *testing.T) {
	prefix := linkPrefix(userOwner, "owner")
	html := `<a href="` + prefix + `"></a><a href="` + prefix + `%zz">bad</a>`
	_, refused, _ := parsePackageList(html, "owner", userOwner)
	if refused.Count != 2 || refused.Sample != "%zz" {
		t.Errorf("parsePackageList refusals = %+v, want count 2 and sample %%zz", refused)
	}
}

func TestFetchHTML_SendsRequestHeaders(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		if r.Header.Get("Accept") != "text/html" {
			t.Errorf("Accept = %q, want text/html", r.Header.Get("Accept"))
		}
		w.Write([]byte("<html>test</html>"))
	}))

	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))
	html, err := c.fetchHTML(t.Context(), &pacer{delay: c.pacingDelay}, packagePageURL)
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
// reflows that whitespace, so it is not format drift.
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
// owner/repo pair and scraped download count.
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
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	listingFailed := collection.ListingFailed

	if listingFailed {
		t.Error("listingFailed = true, want false")
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

func TestCollect_CanonicalizedExplicitRefDeduplicatesWildcard(t *testing.T) {
	for _, key := range []string{"DOCKERHUB_REPOS", "POLL_INTERVAL_HOURS", "LISTEN_ADDR", "LOG_LEVEL"} {
		t.Setenv(key, "")
	}
	t.Setenv("GHCR_REPOS", "owner/*,owner/App,owner/Extra")
	cfg, _ := config.Load()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(packageLink(userOwner, "owner", "App")))
			return
		}
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/app", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("7")))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/extra", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("11")))
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))

	collection := c.Collect(t.Context(), cfg.GHCRRepos)
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed
	if listingFailed || attempted != 2 || len(entries) != 2 {
		t.Fatalf("Collect(canonicalized wildcard, duplicate explicit ref, and new explicit ref) = (%d entries, attempted %d, listingFailed %v), want (2, 2, false)", len(entries), attempted, listingFailed)
	}
	pulls := map[string]int64{}
	for _, entry := range entries {
		pulls[entry.Repo] = entry.Pulls
	}
	want := map[string]int64{"app": 7, "extra": 11}
	if !maps.Equal(pulls, want) {
		t.Errorf("Collect package pulls = %v, want %v", pulls, want)
	}
}

func TestScrapeDownloads_EscapesNestedName(t *testing.T) {
	var path string
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))

	count, err := c.scrapeDownloads(t.Context(), &pacer{delay: c.pacingDelay}, registry.RepoRef{
		Owner: "owner",
		Repo:  "helm-charts/grafana-operator",
	})
	if err != nil {
		t.Fatalf("scrapeDownloads: %v", err)
	}
	if count != 7 {
		t.Errorf("scrapeDownloads count = %d, want 7", count)
	}
	if path != "/users/owner/packages/container/package/helm-charts%2Fgrafana-operator" {
		t.Errorf("scrapeDownloads path = %q, want nested name escaped once", path)
	}
}

// TestFetchHTML_AppBodyCapWinsOverCallerOption verifies that a GHCR page
// larger than ghcrBodyCap is surfaced as errHTMLFormatChanged (a markup/format
// signal) even when the caller supplies a larger cap. httpx v2 returns a typed
// *ResponseTooLargeError on overflow (v1 silently truncated).
func TestFetchHTML_AppBodyCapWinsOverCallerOption(t *testing.T) {
	oversize := strings.Repeat("x", ghcrBodyCap+1)
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversize))
	}))

	c := NewClient(srv.Client(), fastPacing(append(shortRetry(), httpx.WithMaxBodyBytes(8<<20)), testsupport.QuietLogger()))
	body, err := c.fetchHTML(t.Context(), &pacer{delay: c.pacingDelay}, packagePageURL)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("fetchHTML over-cap error = %v, want errHTMLFormatChanged", err)
	}
	if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); !ok {
		t.Errorf("error = %v, want it to wrap *httpx.ResponseTooLargeError", err)
	}
	if body != "" {
		t.Errorf("fetchHTML over-cap body = %q, want empty", body)
	}
}

// TestParsePackageList_SkipsMalformedAndEmptyNames pins that an empty name
// inside a real href is refused without aborting the walk, so a later valid
// link on the same line is still parsed.
func TestParsePackageList_SkipsMalformedAndEmptyNames(t *testing.T) {
	tests := []struct {
		name        string
		html        string
		owner       string
		want        []string
		wantRefused int
	}{
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
			got, refused, _ := parsePackageList(tt.html, tt.owner, userOwner)
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

// packageLink renders one package link in the form a listing page of kind
// carries, so a fixture matches the prefix the parser matches.
func packageLink(kind ownerKind, owner, name string) string {
	return `<a href="` + linkPrefix(kind, owner) + name + `">` + name + `</a>`
}

// listingClient returns a Client whose listing reads are paced in microseconds.
func listingClient(t *testing.T, h http.Handler, logger *slog.Logger) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	return NewClient(srv.Client(), fastPacing(shortRetry(), logger))
}

// TestClient_ScrapePackageList_PaginatesOwnerListing pins the page loop: an
// owner listing longer than one page is read until a page adds no package name,
// and the names are unioned in listing order.
func TestClient_ScrapePackageList_PaginatesOwnerListing(t *testing.T) {
	tests := []struct {
		name      string
		pages     map[string]string
		want      []string
		wantPages int
	}{
		{
			name: "stops on a page with no package link",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a"),
				"2": packageLink(userOwner, "owner", "b"),
				"3": `<html>no package links</html>`,
			},
			want:      []string{"a", "b"},
			wantPages: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			asked := map[string]int{}
			ecosystems := map[string]int{}
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				asked[page]++
				ecosystems[r.URL.Query().Get("ecosystem")]++
				if strings.HasPrefix(r.URL.Path, "/orgs/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(tt.pages[page]))
			}), capturingLogger(&buf))

			got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
			if err != nil {
				t.Fatalf("scrapePackageList: %v", err)
			}
			if refused.Count != 0 {
				t.Errorf("refusals = %+v, want none", refused)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("scrapePackageList = %v, want %v (every page, in listing order)", got, tt.want)
			}
			logs := buf.String()
			if strings.Contains(logs, "hit page cap") || strings.Contains(logs, "partially failed") {
				t.Errorf("a listing that ended normally warned; logs:\n%s", logs)
			}
			warning := `level=WARN msg="ghcr listing page states no usable package count; completeness unchecked" owner=owner page=`
			if got := strings.Count(logs, warning); got != tt.wantPages {
				t.Errorf("listing without usable package counts emitted %d completeness WARNs, want %d; logs:\n%s", got, tt.wantPages, logs)
			}
			if !strings.Contains(logs, fmt.Sprintf("%s%d", warning, tt.wantPages)) {
				t.Errorf("listing completeness WARNs omitted final page %d; logs:\n%s", tt.wantPages, logs)
			}
			if asked["1"] != 2 {
				t.Errorf("page 1 requested %d times, want 2 (one account-kind probe and one user listing)", asked["1"])
			}
			if len(asked) != tt.wantPages {
				t.Errorf("listing requested %d distinct pages, want %d", len(asked), tt.wantPages)
			}
			wantEcosystems := map[string]int{"container": tt.wantPages + 1}
			if !maps.Equal(ecosystems, wantEcosystems) {
				t.Errorf("ecosystems %v, want %v", ecosystems, wantEcosystems)
			}
		})
	}
}

func TestClient_ScrapePackageList_BoundsPageCandidates(t *testing.T) {
	for _, count := range []int{maxListingCandidates, maxListingCandidates + 1} {
		t.Run(fmt.Sprintf("candidates_%d", count), func(t *testing.T) {
			var html strings.Builder
			for i := range count {
				html.WriteString(packageLink(userOwner, "owner", fmt.Sprintf("p%d", i)))
			}
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					_, _ = w.Write([]byte(html.String()))
					return
				}
				_, _ = w.Write([]byte(`<span>0 packages</span>`))
			}), testsupport.QuietLogger())

			got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
			if count > maxListingCandidates {
				if !errors.Is(err, errHTMLFormatChanged) || len(got) != 0 {
					t.Fatalf("scrapePackageList(%d candidates) = (%d names, %v), want no names and errHTMLFormatChanged", count, len(got), err)
				}
				want := fmt.Sprintf("listing page 1 carried %d package candidates", count)
				if !strings.Contains(err.Error(), want) {
					t.Errorf("scrapePackageList error = %q, want %q", err, want)
				}
				return
			}
			if err != nil || len(got) != maxListingCandidates {
				t.Errorf("scrapePackageList(%d candidates) = (%d names, %v), want %d names", count, len(got), err, maxListingCandidates)
			}
		})
	}
}

func TestClient_ScrapePackageList_RefusedOnlyPageFailsClosed(t *testing.T) {
	pages := 0
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(packageLink(userOwner, "owner", "good")))
			return
		}
		_, _ = w.Write([]byte(`<a href="/users/owner/packages/container/package/%zz">bad</a>`))
	}), testsupport.QuietLogger())

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList error = %v, want errHTMLFormatChanged", err)
	}
	if !slices.Equal(got, []string{"good"}) || refused.Count != 1 || pages != 3 {
		t.Errorf("scrapePackageList = (%v, %+v) after %d requests, want partial name and one refusal after an org probe plus 2 user pages", got, refused, pages)
	}
}

// TestClient_ScrapePackageList_RepeatedPageIsPartial pins the stalled-walk
// arm: a later page that re-serves only already-collected names is a partial
// listing, so the names already read come back with errHTMLFormatChanged and
// the literal alerts/logql.yaml keys on.
func TestClient_ScrapePackageList_RepeatedPageIsPartial(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a") + packageLink(userOwner, "owner", "b")))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList error = %v, want errHTMLFormatChanged for a page that re-served collected names", err)
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("scrapePackageList = %v, want the first page's names kept alongside the error", got)
	}
	if logs := buf.String(); !strings.Contains(logs, `level=ERROR msg="ghcr owner listing partially failed"`) {
		t.Errorf("a repeated page did not log the partial-listing record at ERROR; logs:\n%s", logs)
	}
}

// TestClient_ScrapePackageList_PageCapWarnsOnTruncation pins the WARN an
// operator's alert keys on: with names still arriving when the bound bites,
// the listing is truncated and reports the configured cap.
func TestClient_ScrapePackageList_PageCapWarnsOnTruncation(t *testing.T) {
	var buf bytes.Buffer
	pages := 0
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if strings.HasPrefix(r.URL.Path, "/orgs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "p"+r.URL.Query().Get("page"))))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if len(got) != maxListingPages || pages != maxListingPages+1 {
		t.Errorf("scrapePackageList = %v after %d requests, want %d names from one org probe and %d user pages", got, pages, maxListingPages, maxListingPages)
	}
	logs := buf.String()
	wantPages := fmt.Sprintf("max_pages=%d", maxListingPages)
	if !strings.Contains(logs, "hit page cap") || !strings.Contains(logs, wantPages) {
		t.Errorf("a truncated listing did not warn with the page cap, %s; logs:\n%s", wantPages, logs)
	}
}

// TestClient_ScrapePackageList_LaterPageFailureIsPartial pins the other
// literal alerts/logql.yaml keys on: a page after the first failing is a PARTIAL
// listing, so the names already read come back with the error, and the
// owner is not reported as wholly failed.
func TestClient_ScrapePackageList_LaterPageFailureIsPartial(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a")))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want the page-2 failure reported")
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("scrapePackageList = %v, want the page-1 name kept alongside the error", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "listing partially failed") {
		t.Errorf("a partial listing did not warn with the `listing partially failed` literal; logs:\n%s", logs)
	}

	_, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if whollyFailed {
		t.Error("buildPackageList reported a wholly failed listing, want partial (page 1 yielded a name)")
	}
}

func TestClient_Collect_cancelledListingIsNotListingFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a")))
			return
		}
		cancel()
		<-r.Context().Done()
	}), capturingLogger(&buf))

	collection := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed
	if len(entries) != 0 || attempted != 0 || listingFailed {
		t.Errorf("Collect(cancelled listing) = (%d entries, attempted %d, listingFailed %v), want (0, 0, false)",
			len(entries), attempted, listingFailed)
	}
	logs := buf.String()
	if strings.Contains(logs, "ghcr owner listing partially failed") {
		t.Errorf("cancelled listing emitted the partial-listing alert record; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "ghcr package listing cancelled") {
		t.Errorf("cancelled listing emitted no cancellation record; logs:\n%s", logs)
	}
}

func TestClient_ScrapePackageList_ReturnsUserFormRefusals(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(packageLink(userOwner, "owner", "bad/nested") +
				packageLink(userOwner, "owner", "svc")))
			return
		}
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if !slices.Equal(got, []string{"svc"}) {
		t.Errorf("scrapePackageList = %v, want [svc]", got)
	}
	if refused.Count != 1 || refused.Sample != "bad/nested" {
		t.Errorf("scrapePackageList refusals = %+v, want one refusal sampled as bad/nested", refused)
	}
}

func TestClient_ScrapePackageList_OrganizationLaterPageFailureIsPartial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(packageLink(orgOwner, "owner", "svc")))
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want organization page-2 failure")
	}
	if !slices.Equal(got, []string{"svc"}) || refused.Count != 0 {
		t.Errorf("scrapePackageList = (%v, %+v), want organization page-1 name alongside error", got, refused)
	}
}

func TestClient_Collect_OrganizationLaterPageNotFoundIsPartial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(packageLink(orgOwner, "owner", "svc")))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/svc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("17")))
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))

	collection := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed
	if attempted != 1 || listingFailed {
		t.Errorf("Collect with organization page-2 404 = (attempted %d, listingFailed %v), want (1, false)", attempted, listingFailed)
	}
	if len(entries) != 1 {
		t.Fatalf("Collect with organization page-2 404 returned %d entries, want 1", len(entries))
	}
	if entries[0].Owner != "owner" || entries[0].Repo != "svc" || entries[0].Pulls != 17 {
		t.Errorf("Collect with organization page-2 404 entry = %+v, want owner/svc with 17 pulls", entries[0])
	}
}

// TestClient_ScrapePackageList_OrganizationFirstPageFailureReturnsError pins
// scrapePackageList's organization-error arm.
func TestClient_ScrapePackageList_OrganizationFirstPageFailureReturnsError(t *testing.T) {
	orgCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
		orgCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	_, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	statusErr, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || statusErr.Code != http.StatusInternalServerError {
		t.Fatalf("scrapePackageList error = %v, want organization page-1 HTTP status 500", err)
	}
	if orgCalls == 0 {
		t.Error("scrapePackageList made no organization-form request")
	}
}

func TestClient_ScrapePackageList_UserNamesWinOverOrganizationProbeFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(packageLink(userOwner, "owner", "svc")))
		case "2":
			_, _ = w.Write([]byte(`<span>0 packages</span>`))
		default:
			t.Errorf("user listing requested page %q, want page 1 or 2", r.URL.Query().Get("page"))
		}
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err != nil {
		t.Fatalf("scrapePackageList(organization 500, valid user listing) error = %v, want nil", err)
	}
	if !slices.Equal(got, []string{"svc"}) || refused.Count != 0 {
		t.Errorf("scrapePackageList(organization 500, valid user listing) = (%v, %+v), want ([svc], no refusals)", got, refused)
	}
}

func TestClient_ScrapePackageList_ConfirmedEmptyNamesOnlyActualCause(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "nobody")
	if !errors.Is(err, errEmptyListing) || errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList = (%v, %v), want only errEmptyListing", got, err)
	}
	if !strings.Contains(err.Error(), "the listing states 0") {
		t.Errorf("scrapePackageList error = %q, want confirmed zero-package diagnostic", err)
	}
}

func TestClient_Collect_ConfirmedEmptyWildcardIsNotFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	var buf bytes.Buffer
	c := listingClient(t, mux, capturingLogger(&buf))

	collection := c.Collect(t.Context(), []registry.RepoRef{{Owner: "nobody", Repo: "*"}})
	if len(collection.Entries) != 0 || collection.Fetched != 0 || collection.Attempted != 0 || collection.ListingFailed {
		t.Errorf("Collect(confirmed-empty wildcard) = %+v, want an empty successful collection", collection)
	}
	logs := buf.String()
	const warn = `level=WARN msg="ghcr owner has no public container packages" owner=nobody`
	if got := strings.Count(logs, warn); got != 1 {
		t.Errorf("Collect(confirmed-empty wildcard) WARN count = %d, want 1; logs:\n%s", got, logs)
	}
	if strings.Contains(logs, `level=ERROR msg="ghcr package listing failed"`) {
		t.Errorf("Collect(confirmed-empty wildcard) entered the ERROR alert stream; logs:\n%s", logs)
	}
}

// TestClient_ScrapePackageList_EmptyFirstPageNamesBothCauses pins the
// diagnostic for the one state that stays ambiguous after the kind probe: a
// user with no container packages and a user page whose link markup changed
// are indistinguishable, so the error names both.
func TestClient_ScrapePackageList_EmptyFirstPageNamesBothCauses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // not an organization: this owner is a user
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "nobody")
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList = (%v, %v), want errHTMLFormatChanged", got, err)
	}
	for _, want := range []string{"check the owner name", "the listing markup changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("scrapePackageList error = %q, want it to name %q", err, want)
		}
	}
}

func TestClient_ExpandWildcard_ReportsOnlyParseDrift(t *testing.T) {
	tests := []struct {
		name       string
		html       string
		wantReport bool
	}{
		{name: "empty listing", html: `<html>no package links</html>`},
		{name: "parse drift", html: `<a href="/users/owner/packages/container/package/%zz">bad</a>`, wantReport: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.html))
			}), capturingLogger(&buf))

			_, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
			if !whollyFailed {
				t.Fatal("buildPackageList listingFailed = false, want true")
			}
			gotReport := strings.Contains(buf.String(), "report_at=")
			if gotReport != tt.wantReport {
				t.Errorf("buildPackageList logs report_at = %v, want %v; logs:\n%s", gotReport, tt.wantReport, buf.String())
			}
		})
	}
}

func TestClient_ExpandWildcard_TransportFailureWarns(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}), capturingLogger(&buf))

	_, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if !whollyFailed {
		t.Fatal("buildPackageList listingFailed = false, want true for a transport failure")
	}

	logs := buf.String()
	const warnRecord = `level=WARN msg="ghcr package listing failed"`
	if got := strings.Count(logs, warnRecord); got != 1 {
		t.Errorf("transport-failure WARN count = %d, want 1; logs:\n%s", got, logs)
	}
	if strings.Contains(logs, `level=ERROR msg="ghcr package listing failed"`) {
		t.Errorf("transport failure entered the ERROR alert stream; logs:\n%s", logs)
	}
}

// TestClient_ExpandWildcard_BoundsRefusedNameSample pins that the refused
// name count plus one sample is bounded at the emit site: the sample is
// raw bytes off a scraped page, so an unterminated attribute must not
// reach the log stream megabytes wide.
func TestClient_ExpandWildcard_BoundsRefusedNameSample(t *testing.T) {
	var buf bytes.Buffer
	huge := strings.Repeat("%", 4096)
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`<a href="` + linkPrefix(userOwner, "owner") + huge + `">bad</a>` +
				packageLink(userOwner, "owner", "good")))
			return
		}
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	}), capturingLogger(&buf))

	packages, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if whollyFailed || len(packages) != 1 || packages[0].Repo != "good" {
		t.Fatalf("buildPackageList = (%+v, whollyFailed=%v), want just owner/good", packages, whollyFailed)
	}

	logs := buf.String()
	const warnRecord = `level=WARN msg="ghcr listing refused package candidates" owner=owner refused=1 sample=`
	if got := strings.Count(logs, warnRecord); got != 1 {
		t.Errorf("refused-name WARN count = %d, want 1; logs:\n%s", got, logs)
	}
	if len(logs) > 1024 {
		t.Errorf("one refused 4096-byte name produced a %d-byte log; want the sample bounded", len(logs))
	}
}

func TestParsePackageList_QuotedGreaterThanPreservesLaterAttributes(t *testing.T) {
	html := `<a data-x="a>b" href="/users/owner/packages/container/package/real">r</a>`
	got, refused, err := parsePackageList(html, "owner", userOwner)
	if err != nil || !slices.Equal(got, []string{"real"}) || refused.Count != 0 {
		t.Errorf("parsePackageList(%q) = (%v, %+v, %v), want ([real], no refusals, nil)", html, got, refused, err)
	}
}

func TestParsePackageList_UppercaseTagWithQuotedGreaterThan(t *testing.T) {
	html := `<A data-note="1 > 0" HREF="/users/owner/packages/container/package/upper">`
	got, refused, err := parsePackageList(html, "owner", userOwner)
	if err != nil || !slices.Equal(got, []string{"upper"}) || refused.Count != 0 {
		t.Errorf("parsePackageList(%q) = (%v, %+v, %v), want ([upper], no refusals, nil)", html, got, refused, err)
	}
}

// Commented-out markup is not markup: a terminated comment's body is
// skipped whole, so a package link inside one is invisible and the canary's
// bare quotes never open an attribute value.
func TestStartTagWalk_IgnoresCommentedMarkup(t *testing.T) {
	canary := `<!-- '"` + "`" + ` --><!-- </textarea></xmp> -->`
	html := canary +
		`<!-- ` + packageLink(userOwner, "owner", "ghost") + ` -->` +
		packageLink(userOwner, "owner", "app1")
	got, refused, _ := parsePackageList(html, "owner", userOwner)
	if !slices.Equal(got, []string{"app1"}) || refused.Count != 0 {
		t.Errorf("parsePackageList(commented markup + link) = (%v, %+v), want ([app1], no refusals)", got, refused)
	}
}

func TestStartTagWalk_RecognizesCommentEnds(t *testing.T) {
	real := packageLink(userOwner, "owner", "real")
	tests := []struct {
		name string
		html string
	}{
		{name: "bang_close", html: `<!-- ` + packageLink(userOwner, "owner", "ghost") + ` --!>` + real},
		{name: "abrupt_close", html: `<!-->` + real + `-->`},
		{name: "abrupt_dash_close", html: `<!--->` + real + `-->`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused, err := parsePackageList(tt.html, "owner", userOwner)
			if err != nil || !slices.Equal(got, []string{"real"}) || refused.Count != 0 {
				t.Errorf("parsePackageList(%q) = (%v, %+v, %v), want ([real], no refusals, nil)", tt.html, got, refused, err)
			}
		})
	}
}

func TestStartTagWalk_IgnoresNonStartTagMarkup(t *testing.T) {
	real := packageLink(userOwner, "owner", "real")
	tests := []struct {
		name string
		html string
	}{
		{name: "declaration", html: `<![CDATA[` + packageLink(userOwner, "owner", "ghost") + `]]>` + real},
		{name: "processing_instruction", html: `<?ignored ` + packageLink(userOwner, "owner", "ghost") + `?>` + real},
		{name: "end_tag", html: `</a ` + packageLink(userOwner, "owner", "ghost") + `>` + real},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused, err := parsePackageList(tt.html, "owner", userOwner)
			if err != nil || !slices.Equal(got, []string{"real"}) || refused.Count != 0 {
				t.Errorf("parsePackageList(%q) = (%v, %+v, %v), want ([real], no refusals, nil)", tt.html, got, refused, err)
			}
		})
	}
}

func TestParsePackageList_StrayQuoteDoesNotExtendTag(t *testing.T) {
	tests := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "later_link",
			html: `<a class="c" "x>` + packageLink(userOwner, "owner", "app1"),
			want: []string{"app1"},
		},
		{
			name: "commented_link",
			html: `<a class="c" "x><!-- ` + packageLink(userOwner, "owner", "ghost") + ` --><b ">`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused, err := parsePackageList(tt.html, "owner", userOwner)
			if err != nil || !slices.Equal(got, tt.want) || refused.Count != 0 {
				t.Errorf("parsePackageList(%q) = (%v, %+v, %v), want (%v, no refusals, nil)", tt.html, got, refused, err, tt.want)
			}
		})
	}
}

// TestParsePackageList_UnterminatedConstructIsFormatDrift pins that the first
// unterminated comment ends the listing before links in its body can be emitted.
func TestParsePackageList_UnterminatedConstructIsFormatDrift(t *testing.T) {
	link := packageLink(userOwner, "owner", "real")

	got, refused, err := parsePackageList("<!--"+link, "owner", userOwner)
	if len(got) != 0 || !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("parsePackageList(unterminated comment + link) = (%v, %v), want ([], errHTMLFormatChanged)", got, err)
	}
	if refused.Count != 0 {
		t.Errorf("parsePackageList(unterminated comment + link) refusals = %+v, want no partial result", refused)
	}

	got, refused, err = parsePackageList(strings.Repeat("<!--", 9)+link, "owner", userOwner)
	if len(got) != 0 || !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("parsePackageList(9 unterminated comments + link) = (%v, %v), want ([], errHTMLFormatChanged)", got, err)
	}
	if refused.Count != 0 {
		t.Errorf("parsePackageList(9 unterminated comments + link) refusals = %+v, want no partial result", refused)
	}
}

func TestParsePackageList_UnterminatedConstructReportsOffset(t *testing.T) {
	_, _, err := parsePackageList("hello <!--x", "owner", userOwner)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("parsePackageList(unterminated construct at byte 6) error = %v, want errHTMLFormatChanged", err)
	}
	if got := err.Error(); !strings.Contains(got, "byte 6") {
		t.Errorf("parsePackageList(unterminated construct at byte 6) error = %q, want construct offset", got)
	}
}

// TestParsePackageList_UnterminatedTagIsFormatDrift pins the same failure mode
// for a start tag whose quoted attribute value has no terminator.
func TestParsePackageList_UnterminatedTagIsFormatDrift(t *testing.T) {
	got, refused, err := parsePackageList(`<a x="`, "owner", userOwner)
	if len(got) != 0 || !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("parsePackageList(unterminated tag) = (%v, %v), want ([], errHTMLFormatChanged)", got, err)
	}
	if refused.Count != 0 {
		t.Errorf("parsePackageList(unterminated tag) refusals = %+v, want no partial result", refused)
	}

	got, _, err = parsePackageList(strings.Repeat(`<a x="`, 9), "owner", userOwner)
	if len(got) != 0 || !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("parsePackageList(9 malformed tags) = (%v, %v), want ([], errHTMLFormatChanged)", got, err)
	}
}
