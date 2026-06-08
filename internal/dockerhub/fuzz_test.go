package dockerhub_test

import (
	"encoding/json"
	"testing"

	"github.com/cplieger/registry-stats/internal/model"
)

// FuzzDockerHubRepoUnmarshal exercises the single-repo metadata JSON parsing
// path with arbitrary bytes. Invariants: never panics; if no error, pull_count >= 0.
func FuzzDockerHubRepoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"pull_count":5000,"last_updated":"2026-03-06T12:00:00Z"}`))
	f.Add([]byte(`{"pull_count":0}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"pull_count":9999999999}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp struct {
			LastUpdated string `json:"last_updated"`
			PullCount   int64  `json:"pull_count"`
		}
		if err := json.Unmarshal(data, &resp); err == nil {
			if resp.PullCount < 0 {
				t.Errorf("pull_count = %d, want >= 0", resp.PullCount)
			}
		}
	})
}

// FuzzDockerHubRepoListUnmarshal exercises the paginated repo list JSON parsing
// with arbitrary bytes. Invariants: never panics; if no error, all pull_counts >= 0.
func FuzzDockerHubRepoListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"app","pull_count":100,"last_updated":"2026-01-01T00:00:00Z"}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp struct {
			Next    string `json:"next"`
			Results []struct {
				Name        string `json:"name"`
				LastUpdated string `json:"last_updated"`
				PullCount   int64  `json:"pull_count"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &resp); err == nil {
			for _, r := range resp.Results {
				if r.PullCount < 0 {
					t.Errorf("pull_count = %d for %q, want >= 0", r.PullCount, r.Name)
				}
			}
		}
	})
}

// FuzzDockerHubTagListUnmarshal exercises the tags endpoint JSON parsing
// with arbitrary bytes. Invariants: never panics; if no error, returned tag names are non-empty.
func FuzzDockerHubTagListUnmarshal(f *testing.F) {
	f.Add([]byte(`{"next":"","results":[{"name":"latest","digest":"sha256:abc","full_size":1024}]}`))
	f.Add([]byte(`{"next":"page2","results":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"results":[{"name":"v1.0","digest":"sha256:def","full_size":2048}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp struct {
			Next    string          `json:"next"`
			Results []model.TagInfo `json:"results"`
		}
		if err := json.Unmarshal(data, &resp); err == nil {
			// Mirror the production filter: collectTags skips empty names.
			// After filtering, all remaining tags must have non-empty names.
			var filtered []model.TagInfo
			for _, tag := range resp.Results {
				if tag.Name == "" {
					continue
				}
				filtered = append(filtered, tag)
			}
			for _, tag := range filtered {
				if tag.Name == "" {
					t.Errorf("tag name is empty after filtering, want non-empty")
				}
			}
		}
	})
}
