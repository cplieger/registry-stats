package dockerhub_test

import (
	"strings"
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
		_, _, _ = dockerhub.ParseRepoMeta(data)
	})
}

// FuzzDockerHubRepoListUnmarshal drives the production owner-listing
// parser. Invariant: every repo it returns is namespaced under the
// requested owner ("owner/..."), so a crafted listing response cannot
// smuggle a foreign owner into the snapshot shape downstream code trusts.
func FuzzDockerHubRepoListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"app","pull_count":100,"last_updated":"2026-01-01T00:00:00Z"}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))

	const owner = "owner"
	f.Fuzz(func(t *testing.T, data []byte) {
		_, repos, err := dockerhub.ParseRepoListPage(data, owner)
		if err != nil {
			return
		}
		for _, r := range repos {
			if !strings.HasPrefix(r.Repo, owner+"/") {
				t.Errorf("ParseRepoListPage(%q) produced repo %q without %q prefix", data, r.Repo, owner+"/")
			}
		}
	})
}

// FuzzDockerHubTagListUnmarshal drives the production tag-page parser.
// Invariant: every returned tag has a non-empty name. collectTags relies
// on this filter so a nameless tag never reaches the snapshot's tag slice.
// The committed seed {"results":[{}]} pins the empty-name-drop edge.
func FuzzDockerHubTagListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"latest","digest":"sha256:abc","full_size":1024}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"results":[{"name":"v1.0","digest":"sha256:def","full_size":2048}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, tags, err := dockerhub.ParseTagPage(data)
		if err != nil {
			return
		}
		for _, tag := range tags {
			if tag.Name == "" {
				t.Errorf("ParseTagPage(%q) returned an empty tag name, want all empties filtered", data)
			}
		}
	})
}
