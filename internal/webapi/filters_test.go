package webapi

import (
	"fmt"
	"testing"

	"registry-stats/internal/model"
	"registry-stats/internal/testsupport"
)

// TestMemStore_StoreContract verifies that the in-memory fake satisfies
// the same api.Store contract as store.FS, preventing silent drift.

func TestStripGrafanaBraces(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"", ""},
		{"{a}", "a"},
		{"{a,b}", "a,b"},
		{"a", "a"},
		{"$__all", "$__all"},
	}
	for _, c := range cases {
		if got := stripGrafanaBraces(c.in); got != c.out {
			t.Errorf("stripGrafanaBraces(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestRegistryFilterIncludes(t *testing.T) {
	cases := []struct {
		name           string
		filter         model.RegistryFilter
		wantHub, wantG bool
	}{
		{"zero-value", model.RegistryFilter{}, true, true},
		{"set-dockerhub", model.RegistryFilter{Only: model.SourceDockerHub, Set: true}, true, false},
		{"set-ghcr", model.RegistryFilter{Only: model.SourceGHCR, Set: true}, false, true},
		{"set-unknown", model.RegistryFilter{Only: model.SourceUnknown, Set: true}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.filter.Includes(model.SourceDockerHub); got != c.wantHub {
				t.Errorf("Includes(SourceDockerHub) = %v, want %v", got, c.wantHub)
			}
			if got := c.filter.Includes(model.SourceGHCR); got != c.wantG {
				t.Errorf("Includes(SourceGHCR) = %v, want %v", got, c.wantG)
			}
		})
	}
}

func TestParseRegistryFilter(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSet bool
		wantSrc model.RegistrySource
	}{
		{"empty", "", false, model.SourceUnknown},
		{"grafana-all", "$__all", false, model.SourceUnknown},
		{"grafana-all-braces", "{$__all}", false, model.SourceUnknown},
		{"multi-value-braces", "{dockerhub,ghcr}", false, model.SourceUnknown},
		{"dockerhub", "dockerhub", true, model.SourceDockerHub},
		{"ghcr", "ghcr", true, model.SourceGHCR},
		{"braces-ghcr", "{ghcr}", true, model.SourceGHCR},
		{"braces-dockerhub", "{dockerhub}", true, model.SourceDockerHub},
		{"unknown-single", "nowhere", false, model.SourceUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := model.ParseRegistryFilter(c.raw)
			if got.Set != c.wantSet {
				t.Errorf("ParseRegistryFilter(%q).Set = %v, want %v", c.raw, got.Set, c.wantSet)
			}
			if got.Only != c.wantSrc {
				t.Errorf("ParseRegistryFilter(%q).Only = %v, want %v", c.raw, got.Only, c.wantSrc)
			}
		})
	}
}

func TestParseRegistryFilter_unknown_falls_back_to_include_all(t *testing.T) {
	// An unknown single-value filter must fall back to "include
	// every source" (zero-value RegistryFilter) to preserve the
	// pre-refactor registryIncludes behaviour.
	got := model.ParseRegistryFilter("nowhere")
	if got.Set {
		t.Errorf("ParseRegistryFilter(unknown).Set = true, want false (include all)")
	}
	if !got.Includes(model.SourceDockerHub) || !got.Includes(model.SourceGHCR) {
		t.Errorf("unknown filter should include every source; got %+v", got)
	}
}

func TestParseRepoFilter(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   map[string]bool
	}{
		{"empty", nil, nil},
		{"single-empty", []string{""}, nil},
		{"grafana-all", []string{"$__all"}, nil},
		{"single", []string{"owner/a"}, map[string]bool{"owner/a": true}},
		{"comma", []string{"owner/a,owner/b"}, map[string]bool{"owner/a": true, "owner/b": true}},
		{"repeated", []string{"owner/a", "owner/b"}, map[string]bool{"owner/a": true, "owner/b": true}},
		{"braces", []string{"{owner/a,owner/b}"}, map[string]bool{"owner/a": true, "owner/b": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRepoFilter(c.values)
			if len(got) != len(c.want) {
				t.Fatalf("parseRepoFilter(%v) = %v, want %v", c.values, got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("parseRepoFilter(%v)[%q] = %v, want %v", c.values, k, got[k], v)
				}
			}
		})
	}
}

func TestDateToISO(t *testing.T) {
	if got := dateToISO("2026-03-06"); got != "2026-03-06T00:00:00Z" {
		t.Errorf("dateToISO = %q, want 2026-03-06T00:00:00Z", got)
	}
}

// filteredPulls is a test-only helper that extracts repo pull data from
// a snapshot, applying repo and registry filters. Moved from production
// code (filters.go) after arch-rs-p1 replaced its sole production caller
// with PullSeries-based index reads.

func filteredPulls(
	snap *model.Snapshot,
	repoFilter []string,
	filter model.RegistryFilter,
) []model.RepoPull {
	repos := parseRepoFilter(repoFilter)
	merged := map[string]int64{}
	forEachSummaryEntry(snap, repos, filter, func(_ model.RegistrySource, e model.SummaryEntry) {
		merged[e.Name] += e.PullCount
	})
	out := make([]model.RepoPull, 0, len(merged))
	for repo, pulls := range merged {
		out = append(out, model.RepoPull{Repo: repo, PullCount: pulls})
	}
	return out
}

func TestFilteredPullsMergesBothRegistries(t *testing.T) {
	snap := &model.Snapshot{
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 10}},
		GHCR:      []model.GhcrStats{{Package: "owner/app", DownloadCount: 5}},
	}
	pulls := filteredPulls(snap, nil, model.RegistryFilter{})
	if len(pulls) != 1 {
		t.Fatalf("len = %d, want 1", len(pulls))
	}
	if pulls[0].PullCount != 15 {
		t.Errorf("merged pulls = %d, want 15", pulls[0].PullCount)
	}
}

func TestFilteredPullsZeroDownloadsExcluded(t *testing.T) {
	snap := &model.Snapshot{
		GHCR: []model.GhcrStats{{Package: "owner/app", DownloadCount: 0}},
	}
	pulls := filteredPulls(snap, nil, model.RegistryFilter{})
	if len(pulls) != 0 {
		t.Errorf("len = %d, want 0 (zero-downloads excluded)", len(pulls))
	}
}

func TestDailyDelta(t *testing.T) {
	logger := testsupport.QuietLogger()
	cases := []struct {
		name                 string
		prevDate, currDate   string
		prevPulls, currPulls int64
		want                 int64
	}{
		{"consecutive", "2026-03-05", "2026-03-06", 100, 110, 10},
		{"gap-2-days", "2026-03-05", "2026-03-07", 100, 120, 10},
		{"counter-reset", "2026-03-05", "2026-03-06", 200, 100, 0},
		{"no-change", "2026-03-05", "2026-03-06", 100, 100, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dailyDelta(logger, "owner/app", c.prevDate, c.prevPulls, c.currDate, c.currPulls)
			if got != c.want {
				t.Errorf("dailyDelta() = %d, want %d", got, c.want)
			}
		})
	}
}

// --- handlers via httptest ---

func TestFilteredPullsRegistryFilter(t *testing.T) {
	snap := &model.Snapshot{
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 100}},
		GHCR:      []model.GhcrStats{{Package: "owner/pkg", DownloadCount: 50}},
	}

	t.Run("dockerhub only", func(t *testing.T) {
		pulls := filteredPulls(snap, nil, model.RegistryFilter{Only: model.SourceDockerHub, Set: true})
		if len(pulls) != 1 || pulls[0].Repo != "owner/app" {
			t.Errorf("got %+v, want only owner/app", pulls)
		}
	})

	t.Run("ghcr only", func(t *testing.T) {
		pulls := filteredPulls(snap, nil, model.RegistryFilter{Only: model.SourceGHCR, Set: true})
		if len(pulls) != 1 || pulls[0].Repo != "owner/pkg" {
			t.Errorf("got %+v, want only owner/pkg", pulls)
		}
	})
}

func TestFilteredPullsRepoFilter(t *testing.T) {
	snap := &model.Snapshot{
		DockerHub: []model.RepoStats{
			{Repo: "owner/app1", PullCount: 100},
			{Repo: "owner/app2", PullCount: 200},
		},
		GHCR: []model.GhcrStats{
			{Package: "owner/pkg1", DownloadCount: 50},
			{Package: "owner/pkg2", DownloadCount: 75},
		},
	}
	pulls := filteredPulls(snap, []string{"owner/app1,owner/pkg2"}, model.RegistryFilter{})
	repos := map[string]bool{}
	for _, p := range pulls {
		repos[p.Repo] = true
	}
	if !repos["owner/app1"] || !repos["owner/pkg2"] {
		t.Errorf("expected app1 and pkg2, got %v", repos)
	}
	if repos["owner/app2"] || repos["owner/pkg1"] {
		t.Errorf("unexpected repos in result: %v", repos)
	}
}

func TestFilteredPullsEmptySnapshot(t *testing.T) {
	pulls := filteredPulls(&model.Snapshot{}, nil, model.RegistryFilter{})
	if len(pulls) != 0 {
		t.Errorf("expected 0 pulls from empty snapshot, got %d", len(pulls))
	}
}

func TestDateToISO_always_appends_suffix(t *testing.T) {
	// Sample matrix (property-like, not rapid) — just verify a few
	// well-formed YYYY-MM-DD strings always yield the ISO 8601 shape.
	dates := []string{"2026-03-06", "2000-01-01", "2099-12-31"}
	for _, d := range dates {
		got := dateToISO(d)
		want := d + "T00:00:00Z"
		if got != want {
			t.Errorf("dateToISO(%q) = %q, want %q", d, got, want)
		}
	}
}

// --- handler tests: /api/snapshot ---

func TestDailyDelta_smoothsAcrossMissingDays(t *testing.T) {
	logger := testsupport.QuietLogger()
	cases := []struct {
		name                 string
		prevDate, currDate   string
		prevPulls, currPulls int64
		want                 int64
	}{
		{"consecutive days", "2026-03-05", "2026-03-06", 100, 120, 20},
		{"1-day gap (2 day span)", "2026-03-05", "2026-03-07", 100, 120, 10},
		{"2-day gap (3 day span)", "2026-03-05", "2026-03-08", 100, 120, 6},
		{"7-day gap", "2026-03-05", "2026-03-12", 100, 140, 5},
		{"counter reset with gap clamps to 0", "2026-03-05", "2026-03-08", 100, 50, 0},
		{"zero diff with gap stays 0", "2026-03-05", "2026-03-08", 100, 100, 0},
		{"invalid dates fall back to gap=1", "bad", "worse", 100, 120, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dailyDelta(logger, "owner/app", c.prevDate, c.prevPulls, c.currDate, c.currPulls)
			if got != c.want {
				t.Errorf("dailyDelta(%q→%q, %d→%d) = %d, want %d",
					c.prevDate, c.currDate, c.prevPulls, c.currPulls, got, c.want)
			}
		})
	}
}

// --- Benchmarks ---

func BenchmarkFilteredPulls(b *testing.B) {
	for _, n := range []int{1, 10, 50} {
		snap := &model.Snapshot{
			DockerHub: make([]model.RepoStats, n),
			GHCR:      make([]model.GhcrStats, n),
		}
		for i := range n {
			snap.DockerHub[i] = model.RepoStats{Repo: fmt.Sprintf("owner/app%d", i), PullCount: int64(i * 100)}
			snap.GHCR[i] = model.GhcrStats{Package: fmt.Sprintf("owner/pkg%d", i), DownloadCount: int64(i * 50)}
		}
		b.Run(fmt.Sprintf("repos=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				filteredPulls(snap, nil, model.RegistryFilter{})
			}
		})
	}
}
