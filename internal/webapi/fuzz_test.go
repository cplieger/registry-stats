package webapi

import (
	"testing"

	"registry-stats/internal/model"
)

func FuzzParseRepoFilter(f *testing.F) {
	f.Add("myrepo")
	f.Add("$__all")
	f.Add("{repo1,repo2}")
	f.Add("")
	f.Add("{$__all}")

	f.Fuzz(func(t *testing.T, input string) {
		result := parseRepoFilter([]string{input})
		if len(result) > 1000 {
			t.Errorf("result map unexpectedly large: %d", len(result))
		}
	})
}

func FuzzParseRegistryFilter(f *testing.F) {
	f.Add("")
	f.Add("dockerhub")
	f.Add("ghcr")
	f.Add("$__all")
	f.Add("{dockerhub}")
	f.Add("{ghcr,dockerhub}")
	f.Add("unknown")

	f.Fuzz(func(t *testing.T, input string) {
		result := model.ParseRegistryFilter(input)
		if result.Set && result.Only != model.SourceDockerHub && result.Only != model.SourceGHCR {
			t.Errorf("Set=true but Only=%v is not a known source", result.Only)
		}
	})
}
