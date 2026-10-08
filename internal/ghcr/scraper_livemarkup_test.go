package ghcr

import (
	"compress/gzip"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveMarkup reads a gzipped GitHub response from the URLs listingURL,
// scrapePackage and ReadDetail build. live-listing (cplieger's listing, its
// authenticity_token masked) and live-package (the registry-stats page, nothing
// masked) were captured 2026-09-09, live-versions (plex-exporter's tagged
// versions, html-safe-nonce, visitor-payload and visitor-hmac masked) 2026-10-07.
// To recapture, mask whichever of those four values a page carries, and gzip.
// cplieger is a user, so the orgs listing answers 404 and these bytes exercise
// the user-form fallback.
func liveMarkup(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".html.gz")
	if err != nil {
		t.Fatalf("open captured %s: %v", name, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip captured %s: %v", name, err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read captured %s: %v", name, err)
	}
	return string(body)
}

// TestParsePackageList_ReadsTheServedListing pins the listing reader against GitHub's
// own bytes. The counts are what the page states about itself, so a change here is
// either a real markup change upstream or a regression in the reader.
func TestParsePackageList_ReadsTheServedListing(t *testing.T) {
	html := liveMarkup(t, "live-listing")

	names, refused := parsePackageList(html, "cplieger", userOwner)
	if refused.Count != 0 {
		t.Errorf("parsePackageList(served listing) refused %d candidates (%q), want 0",
			refused.Count, refused.Sample)
	}
	if len(names) != 24 {
		t.Errorf("parsePackageList(served listing) = %d names, want 24: %v", len(names), names)
	}
	// A name the walk must find, so a refactor that silently returns the wrong SET
	// fails here and not only on the count.
	if !slices.Contains(names, "registry-stats") {
		t.Errorf("parsePackageList(served listing) names = %v, want it to contain registry-stats", names)
	}

	stated, ok := statedPackages(html)
	if !ok {
		t.Fatal("statedPackages(served listing) ok = false, want true — the printed count is what proves a walk complete")
	}
	if stated != len(names) {
		t.Errorf("statedPackages(served listing) = %d, want it to agree with the %d names read", stated, len(names))
	}
}

// TestParseDownloads_ReadsTheServedPackagePage pins the download-count reader. The
// exact figure moves whenever anything pulls the image, so this asserts the shape the
// parser must recover rather than a frozen number.
func TestParseDownloads_ReadsTheServedPackagePage(t *testing.T) {
	got, err := parseDownloads(liveMarkup(t, "live-package"))
	if err != nil {
		t.Fatalf("parseDownloads(served package page) error = %v, want nil", err)
	}
	if got != 31254 {
		t.Errorf("parseDownloads(served package page) = %d, want 31254 (the count in the captured bytes)", got)
	}
}

// TestParsePublished_ReadsTheServedPackagePage pins the "Last published" reader
// against the time the captured page states.
func TestParsePublished_ReadsTheServedPackagePage(t *testing.T) {
	got, err := parsePublished(liveMarkup(t, "live-package"))
	if err != nil {
		t.Fatalf("parsePublished(served package page) error = %v, want nil", err)
	}
	if want := time.Date(2026, 9, 9, 14, 14, 12, 0, time.UTC); !got.Equal(want) {
		t.Errorf("parsePublished(served package page) = %v, want %v", got, want)
	}
}

// TestParseVersionCounts_ReadsTheServedVersionsPage pins the version-count reader on
// the largest tracked package, whose counts GitHub prints with thousands separators.
func TestParseVersionCounts_ReadsTheServedVersionsPage(t *testing.T) {
	html := liveMarkup(t, "live-versions")
	if len(html) >= ghcrBodyCap {
		t.Errorf("served versions page is %d bytes, want it under the %d-byte body cap", len(html), ghcrBodyCap)
	}
	tagged, untagged, err := parseVersionCounts(html)
	if err != nil || tagged != 1002 || untagged != 2215 {
		t.Errorf("parseVersionCounts(served versions page) = (%d, %d, %v), want (1002, 2215, nil)", tagged, untagged, err)
	}
}

func TestServedMarkupCarriesNoExoticConstructs(t *testing.T) {
	listing, pkg, versions := liveMarkup(t, "live-listing"), liveMarkup(t, "live-package"), liveMarkup(t, "live-versions")
	for _, tc := range []struct {
		construct string
		handledBy string
	}{
		{"--!>", "unsupported comment-close markup"},
		{"<!-->", "unsupported comment-close markup"},
		{"<!--->", "unsupported comment-close markup"},
		{"<![CDATA[", "unsupported CDATA markup"},
		{"<?", "unsupported processing-instruction markup"},
	} {
		t.Run(tc.construct, func(t *testing.T) {
			for page, html := range map[string]string{"listing": listing, "package": pkg, "versions": versions} {
				if n := strings.Count(html, tc.construct); n != 0 {
					t.Errorf("served %s page carries %q %d time(s); %s is exercised by real input after all",
						page, tc.construct, n, tc.handledBy)
				}
			}
		})
	}
}

func TestServedMarkupRawTextBodiesCarryNoLessThanBytes(t *testing.T) {
	pages := map[string]string{
		"listing":  liveMarkup(t, "live-listing"),
		"package":  liveMarkup(t, "live-package"),
		"versions": liveMarkup(t, "live-versions"),
	}
	for page, html := range pages {
		for _, element := range []string{"script", "style", "textarea", "title"} {
			openToken := "<" + element
			closeToken := "</" + element + ">"
			for cursor := 0; ; {
				open := strings.Index(html[cursor:], openToken)
				if open < 0 {
					break
				}
				open += cursor
				bodyStart := strings.IndexByte(html[open:], '>')
				if bodyStart < 0 {
					t.Fatalf("served %s page <%s> start tag has no closing '>'", page, element)
				}
				bodyStart += open + 1
				bodyEnd := strings.Index(html[bodyStart:], closeToken)
				if bodyEnd < 0 {
					t.Fatalf("served %s page <%s> has no %s", page, element, closeToken)
				}
				bodyEnd += bodyStart
				if strings.Contains(html[bodyStart:bodyEnd], "<") {
					t.Errorf("served %s page <%s> body contains '<'; this assertion records the served shape, and TestParsePackageList_ReadsTheServedListing holds the listing behavior when it changes", page, element)
				}
				cursor = bodyEnd + len(closeToken)
			}
		}
	}
}
