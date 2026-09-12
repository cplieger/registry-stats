package ghcr

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestParseDownloads_reflowAndSurroundingNoise pins whitespace reflow and
// fail-closed handling of non-whitespace elements after the marker.
func TestParseDownloads_reflowAndSurroundingNoise(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.Int64Range(0, 1<<40).Draw(t, "count")
		gap := rapid.SliceOfN(rapid.SampledFrom([]string{" ", "\t", "\r", "\n"}), 0, 8).Draw(t, "gap")
		// Prefix fragments exclude the marker because parseDownloads anchors on its first occurrence.
		prefix := rapid.SliceOfN(rapid.SampledFrom([]string{
			"<div></div>",
			"<!-- noise -->",
			`<h3 title="999">999</h3>`,
			"<span>unrelated</span>",
		}), 0, 8).Draw(t, "prefix")
		want := strconv.FormatInt(count, 10)
		html := strings.Join(prefix, "") + "<span>Total downloads</span>" + strings.Join(gap, "") + `<h3 title="` + want + `">` + want + `</h3>`

		got, err := parseDownloads(html)
		if err != nil || got != count {
			t.Fatalf("parseDownloads(reflowed count %d) = (%d, %v), want (%d, nil)", count, got, err, count)
		}

		// Interposed elements exclude h3 because the first h3 is a legitimate count candidate.
		interposed := rapid.SampledFrom([]string{
			"<div></div>",
			"<span>noise</span>",
			"<!-- noise -->",
			"<p>noise</p>",
		}).Draw(t, "interposed")
		_, err = parseDownloads("<span>Total downloads</span>" + interposed + `<h3 title="` + want + `">` + want + `</h3>`)
		if !errors.Is(err, errHTMLFormatChanged) {
			t.Fatalf("parseDownloads(interposed %q) error = %v, want errHTMLFormatChanged", interposed, err)
		}
	})
}

// TestParsePackageList_interleavedMarkupIsInvariant pins that unrelated
// markup between package links changes neither names nor refusal count.
func TestParsePackageList_interleavedMarkupIsInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(2, 8).Draw(t, "count")
		noise := rapid.SliceOfN(rapid.SampledFrom([]string{
			`<span data-owner="/users/other">noise</span>`,
			`<div>/users/not-a-link</div>`,
			`<!-- /users/comment -->`,
		}), count, count).Draw(t, "noise")

		var plain strings.Builder
		var interleaved strings.Builder
		for i := range count {
			name := "pkg" + strconv.Itoa(i)
			link := `<a href="/users/owner/packages/container/package/` + name + `">` + name + `</a>`
			plain.WriteString(link)
			interleaved.WriteString(link)
			interleaved.WriteString(noise[i])
		}

		wantNames, wantRefused := parsePackageList(plain.String(), "owner", userOwner)
		gotNames, gotRefused := parsePackageList(interleaved.String(), "owner", userOwner)
		if !slices.Equal(gotNames, wantNames) || gotRefused.Count != wantRefused.Count {
			t.Fatalf("parsePackageList(interleaved markup) = (%v, %d refusals), want (%v, %d refusals)",
				gotNames, gotRefused.Count, wantNames, wantRefused.Count)
		}
	})
}
