package dockerhub

import "testing"

// FuzzDockerHubRepoUnmarshal drives the production single-repo metadata
// parser. Invariant: a nil error implies a non-negative pull count, so a
// response with no usable pull_count can never reach the cumulative gauge
// as a bogus 0.
func FuzzDockerHubRepoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"pull_count":5000,"last_updated":"2026-03-06T12:00:00Z"}`))
	f.Add([]byte(`{"pull_count":0}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"pull_count":9999999999}`))
	f.Add([]byte(`{"pull_count":null}`))
	f.Add([]byte(`{"pull_count":-1}`))
	f.Add([]byte(`{"pull_count":1,"pull_count":2}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := parseRepoMeta(data)
		if err != nil {
			return
		}
		if n < 0 {
			t.Errorf("parseRepoMeta(%q) = %d with nil error, want errors on negative counts", data, n)
		}
	})
}

// FuzzDockerHubRepoListUnmarshal drives the production owner-listing
// parser. Invariant: every entry carries exactly the requested owner, a
// non-empty safe repo name, and a non-negative pull count, so a crafted
// listing response cannot smuggle a foreign owner into the label set or land
// an unsafe name or negative value in a cumulative counter.
func FuzzDockerHubRepoListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"app","pull_count":100,"last_updated":"2026-01-01T00:00:00Z"}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"results":[{"name":""},{"name":"../evil"},{"name":"ok"}]}`))
	f.Add([]byte(`{"results":[{"name":"a","pull_count":1},{"name":"b"}]}`))
	f.Add([]byte(`{"results":[{"name":"a","pull_count":-5}]}`))

	const owner = "owner"
	f.Fuzz(func(t *testing.T, data []byte) {
		repos, _, _, err := parseRepoListPage(data, owner)
		if err != nil {
			return
		}
		for _, r := range repos {
			if r.Owner != owner {
				t.Errorf("parseRepoListPage(%q) produced entry with owner %q, want %q", data, r.Owner, owner)
			}
			if r.Repo == "" {
				t.Errorf("parseRepoListPage(%q) produced an entry with an empty repo name, want empties dropped", data)
			}
			if r.Pulls < 0 {
				t.Errorf("parseRepoListPage(%q) produced entry %q with %d pulls, want the page rejected", data, r.Repo, r.Pulls)
			}
		}
	})
}
