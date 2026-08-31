package ghcr

import (
	"testing"

	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
)

// FuzzParseDownloads drives the production download-count parser with
// arbitrary HTML. Invariant: it never returns a negative count without
// an error — parseDownloads rejects negative counts as format drift, so
// a non-negative count is the only successful outcome.
func FuzzParseDownloads(f *testing.F) {
	f.Add(`<span>Total downloads</span><h3 title="0">0</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="42">42</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="999999999">999999999</h3>`)
	f.Add(`<span>Total downloads</span><div class="foo">bar</div><h3 title="176000">176K</h3>`)
	f.Add(`<span>Total downloads</span><span title="1">rank</span><h3 title="27880">27.8K</h3>`)
	f.Add("<div>nothing</div>")
	f.Add(`<span>Total downloads</span><h3 title="abc">N/A</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="-5">-5</h3>`)
	f.Add(`<span>Total downloads</span><h3 title="12345>`)
	f.Add("")
	f.Fuzz(func(t *testing.T, html string) {
		count, err := parseDownloads(html)
		if err == nil && count < 0 {
			t.Errorf("parseDownloads(%q) = %d with nil error, want non-negative", html, count)
		}
	})
}

// FuzzParsePackageList drives the production package-list parser.
// Invariants: (1) every returned name passes IsSafeURLSegment, so a
// crafted page cannot smuggle a path-traversal name into downstream URL
// construction; (2) a reported refusal sample is itself a refused name,
// so the bounded diagnostic cannot leak a name that was accepted.
func FuzzParsePackageList(f *testing.F) {
	f.Add(`<a href="/users/owner/packages/container/package/app1">app1</a>`, "owner")
	f.Add(`<a href="/users/o/packages/container/package/a">a</a><a href="/users/o/packages/container/package/b">b</a>`, "o")
	f.Add("<html>nothing here</html>", "owner")
	f.Add("", "owner")
	f.Add(`<a href="/users/owner/packages/container/package/good">good</a>`, "owner")
	f.Add(`<a href="/users/owner/packages/container/package/a%2fb">traversal</a>`, "owner")
	f.Fuzz(func(t *testing.T, html, owner string) {
		pkgs, refused := parsePackageList(html, owner, userOwner)
		for _, name := range pkgs {
			if !urlsafe.IsSafeURLSegment(name) {
				t.Errorf("parsePackageList(%q, %q) returned unsafe name %q", html, owner, name)
			}
		}
		if refused.Count > 0 && urlsafe.IsSafeURLSegment(refused.Sample) {
			t.Errorf("parsePackageList(%q, %q) sampled %q as refused, but it is a safe segment", html, owner, refused.Sample)
		}
	})
}
