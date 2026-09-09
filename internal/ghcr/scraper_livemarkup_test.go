package ghcr

import (
	"compress/gzip"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// The readers in this package are the app's only untrusted-input boundary, and every
// other test in it drives markup this repo wrote. That leaves one question unanswered:
// does the code still read what GitHub actually serves, and does anything in it read
// markup GitHub never serves?
//
// These fixtures are real responses, captured 2026-09-09 from the two URLs
// listingURL and scrapeDownloads build:
//
//	https://github.com/cplieger?tab=packages&ecosystem=container&page=1
//	https://github.com/cplieger/registry-stats/pkgs/container/registry-stats
//
// Stored gzipped because they are a quarter-megabyte each and the bytes are the point;
// one value is masked, narrowly and per field, and nothing else is touched: the
// listing's `authenticity_token` carried a live anonymous-session CSRF value, which no
// reader here looks at. To recapture, fetch both URLs with a browser User-Agent, mask
// that one attribute value, and gzip.
//
// The organization form of the listing is deliberately NOT captured: cplieger is a
// user, so `https://github.com/orgs/cplieger/packages` answers 404 and the production
// path falls back to the user form. That fallback is what these bytes exercise.
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
// either a real markup change upstream or a regression in the walk.
func TestParsePackageList_ReadsTheServedListing(t *testing.T) {
	html := liveMarkup(t, "live-listing")

	names, refused, err := parsePackageList(html, "cplieger", userOwner)
	if err != nil {
		t.Fatalf("parsePackageList(served listing) error = %v, want nil", err)
	}
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

// TestServedMarkupCarriesNoExoticConstructs is the other half, and the half no
// ordinary test can state: it asserts what GitHub does NOT serve. Every construct
// below has dedicated handling in this package, and none of it appears in a
// quarter-megabyte of real markup from either page.
//
// This is not a request to delete that handling on sight — a reader whose whole job is
// to break loudly on a markup change may legitimately recognise a construct it has
// never seen. It is here so the next reader can tell the two apart with a command
// instead of an argument, and so a claim that some arm is load-bearing has to name the
// input that reaches it.
func TestServedMarkupCarriesNoExoticConstructs(t *testing.T) {
	listing, pkg := liveMarkup(t, "live-listing"), liveMarkup(t, "live-package")
	for _, tc := range []struct {
		construct string
		handledBy string
	}{
		{"--!>", "commentEnd's bang-close arm"},
		{"<!-->", "commentEnd's abrupt-close arm"},
		{"<!--->", "commentEnd's abrupt-close arm"},
		{"<![CDATA[", "nextStartTag's declaration skip"},
		{"<?", "nextStartTag's processing-instruction skip"},
	} {
		t.Run(tc.construct, func(t *testing.T) {
			for page, html := range map[string]string{"listing": listing, "package": pkg} {
				if n := strings.Count(html, tc.construct); n != 0 {
					t.Errorf("served %s page carries %q %d time(s); %s is exercised by real input after all",
						page, tc.construct, n, tc.handledBy)
				}
			}
		})
	}
}
