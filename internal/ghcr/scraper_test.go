package ghcr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

func lastPageHTML() string {
	return `<div class="` + lastPageMarker + `">Next</div>`
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
		{name: "plain element text", html: downloadsHTML("9"), want: 9},
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

// TestParseDownloads_UsesAssociatedCountElement pins that the count is the
// marker's own element rather than another titled element nearby.
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
	got, refused := parsePackageList(html, "owner", userOwner)
	if !slices.Equal(got, []string{"app"}) || refused.Count != 0 {
		t.Errorf("parsePackageList(single-quoted href) = (%v, %+v), want ([app], no refusals)", got, refused)
	}
}

// TestParsePackageList_MultiplePerLine pins that multiple package links
// on the same HTML line are all extracted.
func TestParsePackageList_MultiplePerLine(t *testing.T) {
	html := `<a href="/users/o/packages/container/package/a">a</a><a href="/users/o/packages/container/package/b">b</a>`
	got, _ := parsePackageList(html, "o", userOwner)
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2 (%v)", len(got), got)
	}
}

func TestParsePackageList_DecodesNestedNames(t *testing.T) {
	for _, encoded := range []string{"helm-charts%2Fgrafana-operator", "helm-charts%2fgrafana-operator"} {
		html := `<a href="/users/owner/packages/container/package/` + encoded + `">package</a>`
		got, refused := parsePackageList(html, "owner", userOwner)
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
			got, refused := parsePackageList(html, "owner", userOwner)
			if !slices.Equal(got, tt.want) || refused.Count != tt.wantRefused {
				t.Errorf("parsePackageList(%q) = (%v, %+v), want (%v, refused=%d)", tt.token, got, refused, tt.want, tt.wantRefused)
			}
		})
	}
}

func TestParsePackageList_RefusesUnsafeDecodedNames(t *testing.T) {
	for _, encoded := range []string{"..%2f..%2fetc%2fpasswd", "%2e%2e%2f%2e%2e", "%2e", "a//b", "/abs", "a%2F", "bad%00name", "%zz"} {
		html := `<a href="/users/owner/packages/container/package/` + encoded + `">package</a>`
		got, refused := parsePackageList(html, "owner", userOwner)
		if len(got) != 0 || refused.Count != 1 {
			t.Errorf("parsePackageList(%q) = (%v, %+v), want one refusal", encoded, got, refused)
		}
	}
}

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
			wantRefused: 2,
		},
		{
			name:        "attribute name ending in href",
			html:        `<div data-href="` + prefix + `a">`,
			wantRefused: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, refused := parsePackageList(tt.html, "owner", userOwner)
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
			got, refused := parsePackageList(tt.html, "nvidia", userOwner)
			if !slices.Equal(got, []string{"CUDA"}) || refused.Count != 0 {
				t.Errorf("parsePackageList = (%v, %+v), want registered package casing preserved", got, refused)
			}
		})
	}
}

func TestParsePackageList_BoundsRefusalSample(t *testing.T) {
	raw := strings.Repeat("a", 256)
	html := `<a href="/users/owner/packages/container/package/` + raw + `">package</a>`
	got, refused := parsePackageList(html, "owner", userOwner)
	if len(got) != 0 || refused.Count != 1 {
		t.Fatalf("parsePackageList(overlong name) = (%v, %+v), want one refusal", got, refused)
	}
	if refused.Sample != strings.Repeat("a", maxRefusalSampleBytes) {
		t.Errorf("refused.Sample = %q (%d bytes), want first %d bytes", refused.Sample, len(refused.Sample), maxRefusalSampleBytes)
	}
}

func TestParsePackageList_RefusalSampleDoesNotRetainPage(t *testing.T) {
	html := `<a href="/users/owner/packages/container/package/%zz">bad</a>` + strings.Repeat("x", 512<<10)
	_, refused := parsePackageList(html, "owner", userOwner)
	if refused.Sample != "%zz" {
		t.Fatalf("parsePackageList refusal sample = %q, want %%zz", refused.Sample)
	}

	pageStart := uintptr(unsafe.Pointer(unsafe.StringData(html)))
	sampleStart := uintptr(unsafe.Pointer(unsafe.StringData(refused.Sample)))
	if sampleStart >= pageStart && sampleStart < pageStart+uintptr(len(html)) {
		t.Error("parsePackageList refusal sample shares the listing page's backing array")
	}

	candidateStart := strings.Index(html, "%zz")
	direct := (refusals{}).refuse(html[candidateStart:])
	directStart := uintptr(unsafe.Pointer(unsafe.StringData(direct.Sample)))
	if directStart >= pageStart && directStart < pageStart+uintptr(len(html)) {
		t.Error("refusals.refuse sample shares the candidate page's backing array")
	}
}

func TestParsePackageList_KeepsFirstInformativeSample(t *testing.T) {
	prefix := linkPrefix(userOwner, "owner")
	html := `<a href="` + prefix + `"></a><a href="` + prefix + `%zz">bad</a>`
	_, refused := parsePackageList(html, "owner", userOwner)
	if refused.Count != 2 || refused.Sample != "%zz" {
		t.Errorf("parsePackageList refusals = %+v, want count 2 and sample %%zz", refused)
	}

	merged := (refusals{Count: 1}).merge(refusals{Count: 1, Sample: "%zz"})
	if merged.Count != 2 || merged.Sample != "%zz" {
		t.Errorf("refusals.merge = %+v, want count 2 and sample %%zz", merged)
	}
}

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
	_, err := c.fetchHTML(ctx, &pacer{delay: c.pacingDelay}, "https://example.com")
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

// TestFetchHTML_SendsBrowserHeaders verifies that fetchHTML installs
// the browser-like User-Agent / Accept / Accept-Language triplet
// GitHub requires for anonymous GHCR pages.
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
	entries, _, listingFailed := c.Collect(t.Context(), refs)

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

// TestCollect_WildcardMock exercises the wildcard branch end-to-end:
// owner listing returns two packages, each package page returns a
// download count.
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
	entries, _, listingFailed := c.Collect(ctx, refs)

	if listingFailed {
		t.Error("listingFailed = true, want false")
	}
	if len(entries) != 2 {
		t.Fatalf("Collect returned %d entries, want 2", len(entries))
	}
}

func TestCollect_CanonicalizedExplicitRefDeduplicatesWildcard(t *testing.T) {
	for _, key := range []string{"DOCKERHUB_REPOS", "POLL_INTERVAL_HOURS", "LISTEN_ADDR", "LOG_LEVEL"} {
		t.Setenv(key, "")
	}
	t.Setenv("GHCR_REPOS", "owner/*,owner/App")
	cfg, _ := config.Load()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "app") + lastPageHTML()))
	})
	mux.HandleFunc("GET /users/owner/packages/container/package/app", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(downloadsHTML("7")))
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))

	entries, attempted, listingFailed := c.Collect(t.Context(), cfg.GHCRRepos)
	if listingFailed || attempted != 1 || len(entries) != 1 {
		t.Errorf("Collect(canonicalized wildcard and explicit ref) = (%d entries, attempted %d, listingFailed %v), want (1, 1, false)", len(entries), attempted, listingFailed)
	}
}

func TestScrapeDownloads_EscapesNestedName(t *testing.T) {
	var path string
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))
	c := NewClient(srv.Client(), fastPacing(shortRetry(), testsupport.QuietLogger()))

	count, err := c.scrapeDownloads(t.Context(), &pacer{delay: c.pacingDelay}, "owner", "helm-charts/grafana-operator")
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

// TestCollect_AllFailuresAreNotListingFailure verifies that package-scrape
// failures do not set the source-private wildcard-listing fact.
func TestCollect_AllFailuresAreNotListingFailure(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	client := srv.Client()
	c := NewClient(client, fastPacing(shortRetry(), testsupport.QuietLogger()))
	refs := []registry.RepoRef{{Owner: "owner", Repo: "pkg1"}}
	entries, _, listingFailed := c.Collect(t.Context(), refs)

	if listingFailed {
		t.Error("expected listingFailed=false when all scrapes fail")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries (failures skipped), got %d", len(entries))
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
	_, err := c.fetchHTML(t.Context(), &pacer{delay: c.pacingDelay}, packagePageURL)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("fetchHTML over-cap error = %v, want errHTMLFormatChanged", err)
	}
	if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); !ok {
		t.Errorf("error = %v, want it to wrap *httpx.ResponseTooLargeError", err)
	}
}

// TestParsePackageList_SkipsMalformedAndEmptyNames covers two malformed
// GHCR package-link candidates: a prefix with no closing delimiter ends
// the scan with one refusal, while an empty name is refused without
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
			name:        "prefix with no closing delimiter yields no packages",
			html:        `<a href="/users/owner/packages/container/package/app1`,
			owner:       "owner",
			wantRefused: 1,
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

// TestBuildPackageListSharedKeyEncodingPreventsDuplicates guards that the
// wildcard and explicit-ref dedup passes write into the SAME `seen` map, so
// an explicit ref already found by the wildcard is not returned, scraped
// and exported a second time.
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
	packages, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, refs)
	if whollyFailed {
		t.Fatal("listing failed, want successful deduplicated package list")
	}

	counts := make(map[registry.RepoRef]int, len(packages))
	for _, p := range packages {
		counts[p]++
	}
	for ref, n := range counts {
		if n != 1 {
			t.Errorf("package %s/%s returned %d times, want exactly 1", ref.Owner, ref.Repo, n)
		}
	}
	if len(packages) != 3 {
		t.Errorf("len(packages) = %d, want 3 (app1, app2, app3); got %+v", len(packages), packages)
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
// owner listing longer than one page is read to its end and the names are
// unioned in listing order, and the read stops clean — no WARN — when a page
// carries GitHub's own last-page marker or adds no new name.
func TestClient_ScrapePackageList_PaginatesOwnerListing(t *testing.T) {
	tests := []struct {
		name      string
		pages     map[string]string
		want      []string
		wantPages int
	}{
		{
			name: "stops on the positive last-page marker",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a") + packageLink(userOwner, "owner", "b"),
				"2": packageLink(userOwner, "owner", "c") + `<div class="next_page disabled">Next</div>`,
			},
			want:      []string{"a", "b", "c"},
			wantPages: 2,
		},
		{
			name: "stops on reversed last-page class tokens",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a") + `<div class="disabled paginate next_page">Next</div>`,
			},
			want:      []string{"a"},
			wantPages: 1,
		},
		{
			name: "ordinary text does not stop pagination",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a") + `<p>next_page disabled</p>`,
				"2": packageLink(userOwner, "owner", "b") + `<div class="next_page disabled">Next</div>`,
			},
			want:      []string{"a", "b"},
			wantPages: 2,
		},
		{
			name: "unrelated attribute does not stop pagination",
			pages: map[string]string{
				"1": packageLink(userOwner, "owner", "a") + `<div data-state="next_page disabled"></div>`,
				"2": packageLink(userOwner, "owner", "b") + `<div class="next_page disabled">Next</div>`,
			},
			want:      []string{"a", "b"},
			wantPages: 2,
		},
		{
			name: "stops on a page that adds no name",
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
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				asked[page]++
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
			if logs := buf.String(); strings.Contains(logs, "hit page cap") || strings.Contains(logs, "partially failed") {
				t.Errorf("a listing that ended normally warned; logs:\n%s", logs)
			}
			if asked["1"] != 1 {
				t.Errorf("page 1 requested %d times, want 1 (the redirecting form would re-fetch it)", asked["1"])
			}
			if len(asked) != tt.wantPages {
				t.Errorf("listing requested %d pages, want %d", len(asked), tt.wantPages)
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
			html.WriteString(lastPageHTML())
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(html.String()))
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
	if !slices.Equal(got, []string{"good"}) || refused.Count != 1 || pages != 2 {
		t.Errorf("scrapePackageList = (%v, %+v) after %d pages, want partial name and one refusal after 2 pages", got, refused, pages)
	}
}

// TestClient_ScrapePackageList_PageCapWarnsOnTruncation pins the WARN an
// operator's alert keys on: with names still arriving when the bound bites,
// the listing is truncated and warns with the literal alerts/logql.yaml matches.
func TestClient_ScrapePackageList_PageCapWarnsOnTruncation(t *testing.T) {
	var buf bytes.Buffer
	pages := 0
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "p"+r.URL.Query().Get("page"))))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if len(got) != maxListingPages || pages != maxListingPages {
		t.Errorf("scrapePackageList = %v after %d pages, want %d names from %d pages", got, pages, maxListingPages, maxListingPages)
	}
	logs := buf.String()
	wantPages := fmt.Sprintf("max_pages=%d", maxListingPages)
	if !strings.Contains(logs, "hit page cap") || !strings.Contains(logs, wantPages) {
		t.Errorf("a truncated listing did not warn with the `hit page cap` literal and %s; logs:\n%s", wantPages, logs)
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

	entries, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
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

// TestClient_ScrapePackageList_ReadsOrganizationForm pins the owner-kind
// probe: an organization's profile page carries no package links, so the
// listing is re-read in the /orgs form, whose links carry their own prefix.
func TestClient_ScrapePackageList_ReadsOrganizationForm(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /myorg", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/myorg/packages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<a href="/orgs/myorg/packages/container/package/svc">svc</a>`))
	})
	c := listingClient(t, mux, testsupport.QuietLogger())

	got, _, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "myorg")
	if err != nil {
		t.Fatalf("scrapePackageList: %v", err)
	}
	if !slices.Equal(got, []string{"svc"}) {
		t.Errorf("scrapePackageList = %v, want the organization's packages", got)
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

	entries, attempted, listingFailed := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})
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

func TestClient_ScrapePackageList_OrganizationFirstPageFailureKeepsUserSummary(t *testing.T) {
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

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{delay: c.pacingDelay}, "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want organization page-1 failure")
	}
	if len(got) != 0 || refused.Count != 0 || orgCalls == 0 {
		t.Errorf("scrapePackageList = (%v, %+v) after %d organization calls, want empty user-form summary", got, refused, orgCalls)
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

// TestClient_ExpandWildcard_BoundsRefusedNameSample pins that the refused
// name count plus one sample is bounded at the emit site: the sample is
// raw bytes off a scraped page, so an unterminated attribute must not
// reach the log stream megabytes wide.
func TestClient_ExpandWildcard_BoundsRefusedNameSample(t *testing.T) {
	var buf bytes.Buffer
	huge := strings.Repeat("%", 4096)
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<a href="` + linkPrefix(userOwner, "owner") + huge + `">bad</a>` +
			packageLink(userOwner, "owner", "good") + lastPageHTML()))
	}), capturingLogger(&buf))

	packages, whollyFailed := c.buildPackageList(t.Context(), &pacer{delay: c.pacingDelay}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
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
