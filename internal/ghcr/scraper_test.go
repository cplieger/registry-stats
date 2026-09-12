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
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/config"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
)

// packagePageURL is a representative production GHCR package-page URL:
// httptest.NewTestServer's in-memory client routes every request to the
// handler regardless of scheme, host or address and leaves Server.URL
// empty, so these tests pass the URL production would build.
const packagePageURL = "https://github.com/users/owner/packages/container/package/pkg"

// downloadsHTML builds a minimal page containing a "Total downloads"
// marker plus a title="N" attribute that parseDownloads can extract.
func downloadsHTML(count string) string {
	return `<span>Total downloads</span><h3 title="` + count + `">` + count + `</h3>`
}

func TestClient_Source(t *testing.T) {
	c := NewClient(http.DefaultClient, Options{Logger: slog.New(slog.DiscardHandler)})
	if got := c.Source().String(); got != "ghcr" {
		t.Errorf("Source().String() = %q, want ghcr", got)
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
		{"attribute name ending in title", `<span>Total downloads</span><h3 data-title="9">27.8K</h3>`},
		{"element name beginning with h3", `<span>Total downloads</span><h3x title="9">27.8K</h3x>`},
		{"two download-count markers", downloadsHTML("1") + downloadsHTML("2")},
		{"marker end tag carrying an attribute", `<span>Total downloads</span foo="><h3 title='9'>">`},
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

func TestStatedPackages_NumberAgreement(t *testing.T) {
	tests := map[string]struct {
		html   string
		want   int
		wantOK bool
	}{
		"singular":  {html: `</svg>1 package</h3>`, want: 1, wantOK: true},
		"plural":    {html: `</svg>24 packages</h3>`, want: 24, wantOK: true},
		"ambiguous": {html: `</svg>24 packages</h3><span>3 packages</span>`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := statedPackages(tt.html)
			if ok != tt.wantOK || (tt.wantOK && got != tt.want) {
				t.Errorf("statedPackages(%q) = (%d, %t), want (%d, %t)", tt.html, got, ok, tt.want, tt.wantOK)
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

func TestParsePackageList_RequiresExactHrefAttribute(t *testing.T) {
	prefix := linkPrefix(userOwner, "owner")
	tests := map[string]struct {
		html string
		want []string
	}{
		"href":       {html: `<a href="` + prefix + `real">`, want: []string{"real"}},
		"data-href":  {html: `<a data-href="` + prefix + `ghost">`},
		"xlink:href": {html: `<a xlink:href="` + prefix + `ghost">`},
		"ahref":      {html: `<a ahref="` + prefix + `ghost">`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, refused := parsePackageList(tt.html, "owner", userOwner)
			if !slices.Equal(got, tt.want) || refused.Count != 0 {
				t.Errorf("parsePackageList(%q) = (%v, %+v), want (%v, no refusals)", tt.html, got, refused, tt.want)
			}
		})
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

func TestParsePackageList_RefusesInvalidUTF8Name(t *testing.T) {
	const token = "%FF"
	html := `<a href="/users/owner/packages/container/package/` + token + `">package</a>`

	got, refused := parsePackageList(html, "owner", userOwner)

	if len(got) != 0 || refused.Count != 1 {
		t.Errorf("parsePackageList(%q) = (%v, %+v), want one refusal", token, got, refused)
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
			_, refused := parsePackageList(html, "owner", userOwner)
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
	names, refused := parsePackageList(html, "owner", userOwner)
	if !slices.Equal(names, []string{"app"}) || refused.Count != 0 {
		t.Fatalf("parsePackageList = (%v, %+v), want ([app], no refusals)", names, refused)
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
	_, refused := parsePackageList(html, "owner", userOwner)
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

	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})
	html, err := c.fetchHTML(t.Context(), &pacer{}, packagePageURL)
	if err != nil {
		t.Fatalf("fetchHTML: %v", err)
	}
	if html != "<html>test</html>" {
		t.Errorf("html = %q, want <html>test</html>", html)
	}
}

func TestFetchHTML_BodyPastCapIsFormatChange(t *testing.T) {
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), ghcrBodyCap+1))
	}), slog.New(slog.DiscardHandler))

	html, err := c.fetchHTML(t.Context(), &pacer{}, packagePageURL)
	if !errors.Is(err, errHTMLFormatChanged) {
		t.Errorf("fetchHTML(body past ghcrBodyCap) error = %v, want errHTMLFormatChanged", err)
	}
	if html != "" {
		t.Errorf("fetchHTML(body past ghcrBodyCap) = %d bytes, want 0", len(html))
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
	c := NewClient(client, Options{Logger: slog.New(slog.DiscardHandler)})
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
	synctest.Test(t, testCollectCanonicalizedExplicitRefDeduplicatesWildcard)
}

func testCollectCanonicalizedExplicitRefDeduplicatesWildcard(t *testing.T) {
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
	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})

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

func TestScrapePackage_EscapesNestedName(t *testing.T) {
	var path string
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(downloadsHTML("7")))
	}))
	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})

	entry, err := c.scrapePackage(t.Context(), &pacer{}, registry.RepoRef{
		Owner: "owner",
		Repo:  "helm-charts/grafana-operator",
	})
	if err != nil {
		t.Fatalf("scrapePackage: %v", err)
	}
	if entry.Pulls != 7 {
		t.Errorf("scrapePackage Pulls = %d, want 7", entry.Pulls)
	}
	if path != "/users/owner/packages/container/package/helm-charts%2Fgrafana-operator" {
		t.Errorf("scrapePackage path = %q, want nested name escaped once", path)
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

// packageLink renders one package link in the form a listing page of kind
// carries, so a fixture matches the prefix the parser matches.
func packageLink(kind ownerKind, owner, name string) string {
	return `<a href="` + linkPrefix(kind, owner) + name + `">` + name + `</a>`
}

func listingClient(t *testing.T, h http.Handler, logger *slog.Logger) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	return NewClient(srv.Client(), Options{Logger: logger})
}

func inSynctest(f func(*testing.T)) func(*testing.T) {
	return func(t *testing.T) { synctest.Test(t, f) }
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
		t.Run(tt.name, inSynctest(func(t *testing.T) {
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

			got, refused, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
		}))
	}
}

func TestAlertRulesCoverWildcardListingConditions(t *testing.T) {
	contents, err := os.ReadFile("../../alerts/logql.yaml")
	if err != nil {
		t.Fatalf("read alert rules: %v", err)
	}

	const rulePrefix = "      - alert: "
	for _, tt := range []struct {
		rule    string
		message string
	}{
		{
			rule:    "RegistryStatsConfigRejected",
			message: "ghcr owner has no public container packages",
		},
		{
			rule:    "RegistryStatsCollectionIncomplete",
			message: "ghcr listing page states no usable package count",
		},
	} {
		t.Run(tt.rule, func(t *testing.T) {
			_, afterRule, ok := strings.Cut(string(contents), rulePrefix+tt.rule)
			if !ok {
				t.Errorf("%s does not contain alert rule %q", "../../alerts/logql.yaml", tt.rule)
				return
			}
			block, _, _ := strings.Cut(afterRule, "\n"+rulePrefix)
			if !strings.Contains(block, tt.message) {
				t.Errorf("alert rule %q does not match message %q", tt.rule, tt.message)
			}
		})
	}
}

func TestClient_ScrapePackageList_BoundsPageCandidates(t *testing.T) {
	for _, count := range []int{maxListingCandidates, maxListingCandidates + 1} {
		t.Run(fmt.Sprintf("candidates_%d", count), inSynctest(func(t *testing.T) {
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
			}), slog.New(slog.DiscardHandler))

			got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
		}))
	}
}

func TestClient_ScrapePackageList_RefusedOnlyPageFailsClosed(t *testing.T) {
	synctest.Test(t, testClientScrapePackageListRefusedOnlyPageFailsClosed)
}

func testClientScrapePackageListRefusedOnlyPageFailsClosed(t *testing.T) {
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
	}), slog.New(slog.DiscardHandler))

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
	synctest.Test(t, testClientScrapePackageListRepeatedPageIsPartial)
}

func testClientScrapePackageListRepeatedPageIsPartial(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a") + packageLink(userOwner, "owner", "b")))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
	synctest.Test(t, testClientScrapePackageListPageCapWarnsOnTruncation)
}

func testClientScrapePackageListPageCapWarnsOnTruncation(t *testing.T) {
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

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
	synctest.Test(t, testClientScrapePackageListLaterPageFailureIsPartial)
}

func testClientScrapePackageListLaterPageFailureIsPartial(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(packageLink(userOwner, "owner", "a")))
	}), capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want the page-2 failure reported")
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("scrapePackageList = %v, want the page-1 name kept alongside the error", got)
	}
	if logs := buf.String(); !strings.Contains(logs, "listing partially failed") {
		t.Errorf("a partial listing did not warn with the `listing partially failed` literal; logs:\n%s", logs)
	}

	_, whollyFailed := c.buildPackageList(t.Context(), &pacer{}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	if whollyFailed {
		t.Error("buildPackageList reported a wholly failed listing, want partial (page 1 yielded a name)")
	}
}

func TestClient_Collect_cancelledListingIsNotListingFailure(t *testing.T) {
	synctest.Test(t, testClientCollectCancelledListingIsNotListingFailure)
}

func testClientCollectCancelledListingIsNotListingFailure(t *testing.T) {
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
	synctest.Test(t, testClientScrapePackageListReturnsUserFormRefusals)
}

func testClientScrapePackageListReturnsUserFormRefusals(t *testing.T) {
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
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
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
	synctest.Test(t, testClientScrapePackageListOrganizationLaterPageFailureIsPartial)
}

func testClientScrapePackageListOrganizationLaterPageFailureIsPartial(t *testing.T) {
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
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
	if err == nil {
		t.Fatal("scrapePackageList error = nil, want organization page-2 failure")
	}
	if !slices.Equal(got, []string{"svc"}) || refused.Count != 0 {
		t.Errorf("scrapePackageList = (%v, %+v), want organization page-1 name alongside error", got, refused)
	}
}

func TestClient_Collect_OrganizationLaterPageNotFoundIsPartial(t *testing.T) {
	synctest.Test(t, testClientCollectOrganizationLaterPageNotFoundIsPartial)
}

func testClientCollectOrganizationLaterPageNotFoundIsPartial(t *testing.T) {
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
	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})

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
	synctest.Test(t, testClientScrapePackageListOrganizationFirstPageFailureReturnsError)
}

func testClientScrapePackageListOrganizationFirstPageFailureReturnsError(t *testing.T) {
	orgCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /owner", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/owner/packages", func(w http.ResponseWriter, _ *http.Request) {
		orgCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	_, _, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
	statusErr, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || statusErr.Code != http.StatusInternalServerError {
		t.Fatalf("scrapePackageList error = %v, want organization page-1 HTTP status 500", err)
	}
	if orgCalls == 0 {
		t.Error("scrapePackageList made no organization-form request")
	}
}

func TestClient_ScrapePackageList_UserNamesWinOverOrganizationProbeFailure(t *testing.T) {
	synctest.Test(t, testClientScrapePackageListUserNamesWinOverOrganizationProbeFailure)
}

func testClientScrapePackageListUserNamesWinOverOrganizationProbeFailure(t *testing.T) {
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
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	got, refused, err := c.scrapePackageList(t.Context(), &pacer{}, "owner")
	if err != nil {
		t.Fatalf("scrapePackageList(organization 500, valid user listing) error = %v, want nil", err)
	}
	if !slices.Equal(got, []string{"svc"}) || refused.Count != 0 {
		t.Errorf("scrapePackageList(organization 500, valid user listing) = (%v, %+v), want ([svc], no refusals)", got, refused)
	}
}

func TestClient_ScrapePackageList_ConfirmedEmptyOrganizationSkipsUserProbe(t *testing.T) {
	synctest.Test(t, testClientScrapePackageListConfirmedEmptyOrganizationSkipsUserProbe)
}

func testClientScrapePackageListConfirmedEmptyOrganizationSkipsUserProbe(t *testing.T) {
	var orgCalls, userCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		orgCalls++
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		userCalls++
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	var buf bytes.Buffer
	c := listingClient(t, mux, capturingLogger(&buf))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "nobody")
	if len(got) != 0 || !errors.Is(err, errEmptyListing) || errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList(confirmed-empty organization) = (%v, %v), want ([], errEmptyListing)", got, err)
	}
	if orgCalls != 1 || userCalls != 0 {
		t.Errorf("scrapePackageList(confirmed-empty organization) requests = (organization %d, user %d), want (1, 0)", orgCalls, userCalls)
	}
	if logs := buf.String(); strings.Contains(logs, "completeness unchecked") {
		t.Errorf("scrapePackageList(confirmed-empty organization) logged a completeness warning:\n%s", logs)
	}
}

func TestClient_ScrapePackageList_ConfirmedEmptyNamesOnlyActualCause(t *testing.T) {
	synctest.Test(t, testClientScrapePackageListConfirmedEmptyNamesOnlyActualCause)
}

func testClientScrapePackageListConfirmedEmptyNamesOnlyActualCause(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<span>0 packages</span>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "nobody")
	if !errors.Is(err, errEmptyListing) || errors.Is(err, errHTMLFormatChanged) {
		t.Fatalf("scrapePackageList = (%v, %v), want only errEmptyListing", got, err)
	}
	if !strings.Contains(err.Error(), "the listing states 0") {
		t.Errorf("scrapePackageList error = %q, want confirmed zero-package diagnostic", err)
	}
}

func TestClient_Collect_ConfirmedEmptyWildcardIsNotFailure(t *testing.T) {
	synctest.Test(t, testClientCollectConfirmedEmptyWildcardIsNotFailure)
}

func testClientCollectConfirmedEmptyWildcardIsNotFailure(t *testing.T) {
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
	synctest.Test(t, testClientScrapePackageListEmptyFirstPageNamesBothCauses)
}

func testClientScrapePackageListEmptyFirstPageNamesBothCauses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /nobody", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>a profile page, no package links</html>`))
	})
	mux.HandleFunc("GET /orgs/nobody/packages", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // not an organization: this owner is a user
	})
	c := listingClient(t, mux, slog.New(slog.DiscardHandler))

	got, _, err := c.scrapePackageList(t.Context(), &pacer{}, "nobody")
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
		t.Run(tt.name, inSynctest(func(t *testing.T) {
			var buf bytes.Buffer
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.html))
			}), capturingLogger(&buf))

			_, whollyFailed := c.buildPackageList(t.Context(), &pacer{}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
			if !whollyFailed {
				t.Fatal("buildPackageList listingFailed = false, want true")
			}
			gotReport := strings.Contains(buf.String(), "report_at=")
			if gotReport != tt.wantReport {
				t.Errorf("buildPackageList logs report_at = %v, want %v; logs:\n%s", gotReport, tt.wantReport, buf.String())
			}
		}))
	}
}

func TestClient_ExpandWildcard_TransportFailureWarns(t *testing.T) {
	synctest.Test(t, testClientExpandWildcardTransportFailureWarns)
}

func testClientExpandWildcardTransportFailureWarns(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}), capturingLogger(&buf))

	_, whollyFailed := c.buildPackageList(t.Context(), &pacer{}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
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
	synctest.Test(t, testClientExpandWildcardBoundsRefusedNameSample)
}

func testClientExpandWildcardBoundsRefusedNameSample(t *testing.T) {
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

	packages, whollyFailed := c.buildPackageList(t.Context(), &pacer{}, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
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
