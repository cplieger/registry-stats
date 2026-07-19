package dockerhub_test

import (
	"testing"

	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
)

// FuzzDockerHubRepoUnmarshal drives the production single-repo metadata
// parser with arbitrary bytes. The Docker Hub response is untrusted
// input, so the invariant is panic-safety: ParseRepoMeta must always
// return (a parse error is a valid outcome) rather than crash. The seed
// corpus pins the real response shape plus malformed inputs.
func FuzzDockerHubRepoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"pull_count":5000,"last_updated":"2026-03-06T12:00:00Z"}`))
	f.Add([]byte(`{"pull_count":0}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"pull_count":9999999999}`))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = dockerhub.ParseRepoMeta(data)
	})
}

// FuzzDockerHubRepoListUnmarshal drives the production owner-listing
// parser. Invariant: every entry it returns carries exactly the
// requested owner and a non-empty repo name (the urlsafe guard drops
// unsafe and empty names), so a crafted listing response can neither
// smuggle a foreign owner into the label set downstream code trusts nor
// inject an empty/unsafe path segment into the tags URL built from it.
func FuzzDockerHubRepoListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"app","pull_count":100,"last_updated":"2026-01-01T00:00:00Z"}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"results":[{"name":""},{"name":"../evil"},{"name":"ok"}]}`))

	const owner = "owner"
	f.Fuzz(func(t *testing.T, data []byte) {
		_, repos, err := dockerhub.ParseRepoListPage(data, owner)
		if err != nil {
			return
		}
		for _, r := range repos {
			if r.Owner != owner {
				t.Errorf("ParseRepoListPage(%q) produced entry with owner %q, want %q", data, r.Owner, owner)
			}
			if r.Repo == "" {
				t.Errorf("ParseRepoListPage(%q) produced an entry with an empty repo name, want empties dropped", data)
			}
		}
	})
}

// FuzzDockerHubTagCountUnmarshal drives the production tag-count parser.
// The Docker Hub tags response is untrusted input feeding the image_tags
// gauge, so the invariant is: a nil error implies a non-negative count
// (a response without a usable count — malformed JSON, absent field,
// negative value — must error so it can never reach the gauge). The
// {"results":[{}]} seed carries over from the deleted tag-page parser's
// committed corpus (valid JSON with no usable payload).
func FuzzDockerHubTagCountUnmarshal(f *testing.F) {
	f.Add([]byte(`{"count":164,"next":"page2","results":[{"name":"latest","digest":"sha256:abc"}]}`))
	f.Add([]byte(`{"count":0,"next":"","results":[]}`))
	f.Add([]byte(`{"count":-1}`))
	f.Add([]byte(`{"results":[{}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := dockerhub.ParseTagCount(data)
		if err != nil {
			return
		}
		if n < 0 {
			t.Errorf("ParseTagCount(%q) = %d with nil error, want errors on negative counts", data, n)
		}
	})
}
