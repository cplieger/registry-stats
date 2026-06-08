package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// TestMemStore_StoreContract verifies that the in-memory fake satisfies
// the same api.Store contract as store.FS, preventing silent drift.

func fixedSnapshot(date string, pulls, downloads int64) *model.Snapshot {
	t, _ := time.Parse("2006-01-02", date)
	return &model.Snapshot{
		Timestamp: t.UTC(),
		DockerHub: []model.RepoStats{{
			Repo:        "owner/app",
			PullCount:   pulls,
			LastUpdated: "2026-03-06T12:00:00Z",
			Tags: []model.TagInfo{{
				Name: "latest", FullSize: 1024, Digest: "sha256:abc",
			}},
		}},
		GHCR: []model.GhcrStats{{Package: "owner/pkg", DownloadCount: downloads}},
	}
}

// --- Pure helpers ---

func newTestHandlers(t *testing.T) (*handlers, *testsupport.MemStore, *fakeHealth) {
	t.Helper()
	store := testsupport.NewMemStore()
	health := &fakeHealth{}
	h := newHandlers(store, health, testsupport.QuietLogger())
	return h, store, health
}

func TestHandlersHealth_healthy(t *testing.T) {
	h, _, health := newTestHandlers(t)
	health.Set(true)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", http.NoBody)
	w := httptest.NewRecorder()
	h.health(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %q, want ok", got["status"])
	}
}

func TestHandlersHealth_unhealthy(t *testing.T) {
	h, _, _ := newTestHandlers(t)
	// default fakeHealth is unhealthy

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/health", http.NoBody)
	w := httptest.NewRecorder()
	h.health(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "unready" {
		t.Errorf("status = %q, want unready (canonical wire shape)", got["status"])
	}
	if got["reason"] == "" {
		t.Errorf("unready response missing reason field")
	}
}

func TestHandlersSnapshot_latest(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 42, 5))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot", http.NoBody)
	w := httptest.NewRecorder()
	h.snapshot(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var got model.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.DockerHub) != 1 || got.DockerHub[0].PullCount != 42 {
		t.Errorf("snapshot = %+v, want 42 pulls", got.DockerHub)
	}
}

func TestHandlersSnapshot_byDate(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 10, 0))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 20, 0))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot?date=2026-03-05", http.NoBody)
	w := httptest.NewRecorder()
	h.snapshot(w, req)

	var got model.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DockerHub[0].PullCount != 10 {
		t.Errorf("PullCount = %d, want 10", got.DockerHub[0].PullCount)
	}
}

func TestHandlersSnapshot_notFound(t *testing.T) {
	h, _, _ := newTestHandlers(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/snapshot", http.NoBody)
	w := httptest.NewRecorder()
	h.snapshot(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandlersPulls(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 10, 5))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 20, 8))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	type row struct {
		Timestamp string `json:"timestamp"`
		Repo      string `json:"repo"`
		PullCount int64  `json:"pull_count"`
	}
	var rows []row
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 2 dates × 2 repos (owner/app in dockerhub, owner/pkg in ghcr) = 4 rows
	if len(rows) != 4 {
		t.Fatalf("len = %d, want 4; rows=%+v", len(rows), rows)
	}
	// sorted by (timestamp, repo) ascending
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Timestamp > rows[i].Timestamp {
			t.Errorf("rows not sorted by timestamp: %v", rows)
			break
		}
	}
}

func TestHandlersPulls_listDatesError(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.ListDatesErr = errors.New("boom")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	// With the index-based read path, a store-level listDates error
	// manifests as an empty index (no entries), not a 500. The error
	// is surfaced at startup/save time, not request time.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandlersPullsDaily(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 100, 50))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 110, 55))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls/daily", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	type row struct {
		Timestamp  string `json:"timestamp"`
		Repo       string `json:"repo"`
		DailyPulls int64  `json:"daily_pulls"`
		FirstSeen  bool   `json:"first_seen,omitempty"`
	}
	var rows []row
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	// first-seen rows have delta=0
	var sawFirst, sawDelta bool
	for _, r := range rows {
		if r.FirstSeen && r.DailyPulls == 0 {
			sawFirst = true
		}
		if !r.FirstSeen && r.DailyPulls > 0 {
			sawDelta = true
		}
	}
	if !sawFirst {
		t.Error("expected a first-seen row with delta=0")
	}
	if !sawDelta {
		t.Error("expected a non-first-seen row with positive delta")
	}
}

func TestHandlersSummary(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 42, 7))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/summary", http.NoBody)
	w := httptest.NewRecorder()
	h.summary(w, req)

	type row struct {
		Registry  string `json:"registry"`
		Name      string `json:"name"`
		PullCount int64  `json:"pull_count"`
		TagCount  int    `json:"tag_count"`
	}
	var rows []row
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	// sort: dockerhub before ghcr (d < g).
	if rows[0].Registry != "dockerhub" || rows[1].Registry != "ghcr" {
		t.Errorf("order = %q, %q; want dockerhub, ghcr", rows[0].Registry, rows[1].Registry)
	}
}

func TestHandlersSummary_notFound(t *testing.T) {
	h, _, _ := newTestHandlers(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/summary", http.NoBody)
	w := httptest.NewRecorder()
	h.summary(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandlersResolveSnapshot_empty(t *testing.T) {
	h, _, _ := newTestHandlers(t)
	_, err := h.resolveSnapshot(t.Context(), "")
	if err == nil {
		t.Error("expected error for empty store")
	}
}

func TestHandlersResolveSnapshot_listDatesError(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.ListDatesErr = errors.New("boom")
	_, err := h.resolveSnapshot(t.Context(), "")
	if err == nil {
		t.Error("expected error when ListDates fails")
	}
}

// --- writeJSON ---

func pullsDailyTwoDaySnap(s *testsupport.MemStore, day1Pulls, day2Pulls int64) {
	s.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: day1Pulls}},
	})
	s.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: day2Pulls}},
	})
}

// --- filter-helper tests ---

func TestHandleSnapshot_byDate(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 80, 40))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/snapshot?date=2026-03-05", http.NoBody)
	w := httptest.NewRecorder()
	h.snapshot(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.DockerHub[0].PullCount != 80 {
		t.Errorf("PullCount = %d, want 80 (from 2026-03-05)", snap.DockerHub[0].PullCount)
	}
}

// --- handler tests: /api/pulls ---

func TestHandlePulls_twoDays(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 80, 40))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []struct {
		Timestamp string `json:"timestamp"`
		Repo      string `json:"repo"`
		PullCount int64  `json:"pull_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 2 dates × (1 dockerhub + 1 ghcr) = 4 rows
	if len(rows) != 4 {
		t.Errorf("len = %d, want 4", len(rows))
	}
}

func TestHandlePulls_sortedOutput(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "z/repo", PullCount: 10},
			{Repo: "a/repo", PullCount: 20},
		},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	var rows []struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	if rows[0].Repo != "a/repo" || rows[1].Repo != "z/repo" {
		t.Errorf("rows not sorted: %v", rows)
	}
}

func TestHandlePulls_sortOrderExact(t *testing.T) {
	// Exact-order pin: two dates × four repos, sort key is (timestamp, repo).
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/z", PullCount: 1},
			{Repo: "owner/a", PullCount: 2},
		},
	})
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/z", PullCount: 3},
			{Repo: "owner/a", PullCount: 4},
		},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	var rows []struct {
		Timestamp string `json:"timestamp"`
		Repo      string `json:"repo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("len = %d, want 4", len(rows))
	}
	want := []struct{ ts, repo string }{
		{"2026-03-05T00:00:00Z", "owner/a"},
		{"2026-03-05T00:00:00Z", "owner/z"},
		{"2026-03-06T00:00:00Z", "owner/a"},
		{"2026-03-06T00:00:00Z", "owner/z"},
	}
	for i, w := range want {
		if rows[i].Timestamp != w.ts || rows[i].Repo != w.repo {
			t.Errorf("rows[%d] = %+v, want %+v", i, rows[i], w)
		}
	}
}

func TestHandlePulls_filters(t *testing.T) {
	// Table-driven consolidation of filter/edge tests for /api/pulls.
	// Each case seeds the store, issues a GET with query params, and
	// asserts status + returned repo names.
	type testCase struct {
		setup     func(*testsupport.MemStore)
		name      string
		query     string
		wantRepos []string // sorted; nil means "assert empty array"
		wantCode  int
	}
	cases := []testCase{
		{
			name: "repo filter",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))
			},
			query:     "repo=owner/app",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/app"},
		},
		{
			name: "registry filter dockerhub",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))
			},
			query:     "registry=dockerhub",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/app"},
		},
		{
			name: "excessive repo params no match",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))
			},
			query: func() string {
				var b strings.Builder
				for i := range 100 {
					if i > 0 {
						b.WriteByte('&')
					}
					fmt.Fprintf(&b, "repo=fake/repo%d", i)
				}
				return b.String()
			}(),
			wantCode:  http.StatusOK,
			wantRepos: nil,
		},
		{
			name:      "empty store returns 200 + empty array",
			setup:     func(_ *testsupport.MemStore) {},
			query:     "",
			wantCode:  http.StatusOK,
			wantRepos: nil,
		},
		{
			name: "grafana multi-value braces repo",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", &model.Snapshot{
					Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
					DockerHub: []model.RepoStats{
						{Repo: "owner/app1", PullCount: 100},
						{Repo: "owner/app2", PullCount: 200},
						{Repo: "owner/app3", PullCount: 300},
					},
					GHCR: []model.GhcrStats{{Package: "owner/pkg1", DownloadCount: 50}},
				})
			},
			query:     "repo=%7Bowner%2Fapp1%2Cowner%2Fapp2%7D",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/app1", "owner/app2"},
		},
		{
			name: "grafana brace-wrapped registry dockerhub",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", &model.Snapshot{
					Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
					DockerHub: []model.RepoStats{
						{Repo: "owner/app1", PullCount: 100},
						{Repo: "owner/app2", PullCount: 200},
						{Repo: "owner/app3", PullCount: 300},
					},
					GHCR: []model.GhcrStats{{Package: "owner/pkg1", DownloadCount: 50}},
				})
			},
			query:     "registry=%7Bdockerhub%7D",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/app1", "owner/app2", "owner/app3"},
		},
		{
			name: "grafana brace-wrapped registry ghcr",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", &model.Snapshot{
					Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
					DockerHub: []model.RepoStats{
						{Repo: "owner/app1", PullCount: 100},
						{Repo: "owner/app2", PullCount: 200},
						{Repo: "owner/app3", PullCount: 300},
					},
					GHCR: []model.GhcrStats{{Package: "owner/pkg1", DownloadCount: 50}},
				})
			},
			query:     "registry=%7Bghcr%7D",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/pkg1"},
		},
		{
			name: "grafana brace-wrapped $__all",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", &model.Snapshot{
					Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
					DockerHub: []model.RepoStats{
						{Repo: "owner/app1", PullCount: 100},
						{Repo: "owner/app2", PullCount: 200},
						{Repo: "owner/app3", PullCount: 300},
					},
					GHCR: []model.GhcrStats{{Package: "owner/pkg1", DownloadCount: 50}},
				})
			},
			query:     "repo=%7B%24__all%7D",
			wantCode:  http.StatusOK,
			wantRepos: []string{"owner/app1", "owner/app2", "owner/app3", "owner/pkg1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, store, _ := newTestHandlers(t)
			c.setup(store)

			url := "/api/pulls"
			if c.query != "" {
				url += "?" + c.query
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
			w := httptest.NewRecorder()
			h.pulls(w, req)

			if w.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, c.wantCode)
			}
			var rows []struct {
				Repo string `json:"repo"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.Repo)
			}
			slices.Sort(got)
			if c.wantRepos == nil {
				if len(got) != 0 {
					t.Errorf("expected empty array, got %v", got)
				}
			} else {
				if !slices.Equal(got, c.wantRepos) {
					t.Errorf("repos = %v, want %v", got, c.wantRepos)
				}
			}
		})
	}
}

func TestHandlePulls_skipsCorruptSnapshot(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 80, 40))
	// 2026-03-06 load errors but listDates reports it.
	store.ByDate["2026-03-06"] = nil
	store.LoadErr["2026-03-06"] = errors.New("corrupt snapshot")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/pulls", http.NoBody)
	w := httptest.NewRecorder()
	h.pulls(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, r := range rows {
		if r.Timestamp == "2026-03-06T00:00:00Z" {
			t.Error("corrupt snapshot should be skipped")
		}
	}
}

// --- handler tests: /api/pulls/daily ---

func TestHandlePullsDaily_filters(t *testing.T) {
	// Table-driven consolidation of filter/edge tests for /api/pulls/daily.
	type testCase struct {
		setup    func(*testsupport.MemStore)
		check    func(t *testing.T, body []byte)
		name     string
		query    string
		wantCode int
	}
	cases := []testCase{
		{
			name: "counter reset clamps to 0",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-05", fixedSnapshot("2026-03-05", 100, 50))
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 10, 5))
			},
			wantCode: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var rows []struct {
					Repo       string `json:"repo"`
					DailyPulls int64  `json:"daily_pulls"`
				}
				if err := json.Unmarshal(body, &rows); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				for _, r := range rows {
					if r.Repo == "owner/app" && r.DailyPulls != 0 {
						t.Errorf("daily delta on counter reset = %d, want 0 (clamped)", r.DailyPulls)
					}
				}
			},
		},
		{
			name: "single day delta is 0",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))
			},
			wantCode: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var rows []struct {
					DailyPulls int64 `json:"daily_pulls"`
				}
				if err := json.Unmarshal(body, &rows); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				for _, r := range rows {
					if r.DailyPulls != 0 {
						t.Errorf("single-day delta = %d, want 0", r.DailyPulls)
					}
				}
			},
		},
		{
			name:     "empty store returns 200 + empty array",
			setup:    func(_ *testsupport.MemStore) {},
			wantCode: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var rows []any
				if err := json.Unmarshal(body, &rows); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if len(rows) != 0 {
					t.Errorf("expected empty array, got %d rows", len(rows))
				}
			},
		},
		{
			name: "registry filter ghcr excludes dockerhub",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-05", fixedSnapshot("2026-03-05", 80, 40))
				s.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))
			},
			query:    "registry=ghcr",
			wantCode: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var rows []struct {
					Repo string `json:"repo"`
				}
				if err := json.Unmarshal(body, &rows); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				for _, r := range rows {
					if r.Repo == "owner/app" {
						t.Error("dockerhub repo should be excluded with ghcr filter")
					}
				}
			},
		},
		{
			name: "repo filter excludes non-matching",
			setup: func(s *testsupport.MemStore) {
				s.Put("2026-03-05", &model.Snapshot{
					Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
					DockerHub: []model.RepoStats{
						{Repo: "owner/app1", PullCount: 100},
						{Repo: "owner/app2", PullCount: 200},
					},
				})
			},
			query:    "repo=owner/app1",
			wantCode: http.StatusOK,
			check: func(t *testing.T, body []byte) {
				var rows []struct {
					Repo string `json:"repo"`
				}
				if err := json.Unmarshal(body, &rows); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				for _, r := range rows {
					if r.Repo == "owner/app2" {
						t.Error("app2 should be filtered out")
					}
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, store, _ := newTestHandlers(t)
			c.setup(store)

			url := "/api/pulls/daily"
			if c.query != "" {
				url += "?" + c.query
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
			w := httptest.NewRecorder()
			h.pullsDaily(w, req)

			if w.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, c.wantCode)
			}
			c.check(t, w.Body.Bytes())
		})
	}
}

func TestHandlePullsDaily_multipleRepos(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/app1", PullCount: 100},
			{Repo: "owner/app2", PullCount: 200},
		},
	})
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/app1", PullCount: 150},
			{Repo: "owner/app2", PullCount: 250},
		},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	type row struct {
		Timestamp  string `json:"timestamp"`
		Repo       string `json:"repo"`
		DailyPulls int64  `json:"daily_pulls"`
	}
	var rows []row
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	deltas := map[string]int64{}
	for _, r := range rows {
		if r.Timestamp == "2026-03-06T00:00:00Z" {
			deltas[r.Repo] = r.DailyPulls
		}
	}
	if deltas["owner/app1"] != 50 {
		t.Errorf("app1 delta = %d, want 50", deltas["owner/app1"])
	}
	if deltas["owner/app2"] != 50 {
		t.Errorf("app2 delta = %d, want 50", deltas["owner/app2"])
	}
}

func TestHandlePullsDaily_skipsCorruptSnapshot(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 80, 40))
	// Inject a load error for 2026-03-06 while keeping it in the date list.
	store.ByDate["2026-03-06"] = nil
	store.LoadErr["2026-03-06"] = errors.New("corrupt")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestHandlePullsDaily_listDatesError(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.ListDatesErr = errors.New("boom")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	// With the index-based read path, a store-level listDates error
	// manifests as an empty index (no entries), not a 500.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandlePullsDaily_smoothsGapEndToEnd(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-01", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 100}},
	})
	store.Put("2026-03-02", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 110}},
	})
	// Skip 2026-03-03 and 2026-03-04 (simulated outage).
	store.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 140}},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily?repo=owner/app", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []struct {
		Timestamp  string `json:"timestamp"`
		Repo       string `json:"repo"`
		DailyPulls int64  `json:"daily_pulls"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var got0305 int64 = -1
	for _, r := range rows {
		if r.Repo == "owner/app" && r.Timestamp == "2026-03-05T00:00:00Z" {
			got0305 = r.DailyPulls
		}
	}
	// 140 - 110 = 30 pulls over 3-day span → 10/day smoothed.
	if got0305 != 10 {
		t.Errorf("2026-03-05 daily_pulls = %d, want 10", got0305)
	}
}

func TestHandlePullsDaily_carryForwardMissingRegistry(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	// Day 1: both registries report. DockerHub=100, GHCR=50, merged=150.
	store.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 100}},
		GHCR:      []model.GhcrStats{{Package: "owner/app", DownloadCount: 50}},
	})
	// Day 2: GHCR scrape failed, DockerHub=120. Without carry-forward the
	// merged total would be 120 and delta would clamp to 0.
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 120}},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily?repo=owner/app", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []struct {
		Timestamp  string `json:"timestamp"`
		DailyPulls int64  `json:"daily_pulls"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var day2 int64 = -1
	for _, r := range rows {
		if r.Timestamp == "2026-03-06T00:00:00Z" {
			day2 = r.DailyPulls
		}
	}
	// GHCR carried forward at 50, DockerHub rose 100→120, so merged went
	// 150→170, delta = 20.
	if day2 != 20 {
		t.Errorf("day2 daily_pulls = %d, want 20 (DockerHub +20, GHCR carried forward)", day2)
	}
}

func TestHandlePullsDaily_noCarryForwardWhenRepoFullyDrops(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/gone", PullCount: 100}},
		GHCR:      []model.GhcrStats{{Package: "owner/gone", DownloadCount: 50}},
	})
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/other", PullCount: 200}},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	var rows []struct {
		Timestamp string `json:"timestamp"`
		Repo      string `json:"repo"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, r := range rows {
		if r.Timestamp == "2026-03-06T00:00:00Z" && r.Repo == "owner/gone" {
			t.Error("owner/gone should not appear on day 2 after being dropped")
		}
	}
}

func TestHandlePullsDaily_firstSeenAnnotation(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	pullsDailyTwoDaySnap(store, 5000, 5010)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily?repo=owner/app", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	var rows []struct {
		Timestamp  string `json:"timestamp"`
		DailyPulls int64  `json:"daily_pulls"`
		FirstSeen  bool   `json:"first_seen"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if !rows[0].FirstSeen {
		t.Errorf("rows[0] first_seen = false, want true (day-1 should be marked)")
	}
	if rows[0].DailyPulls != 0 {
		t.Errorf("rows[0] daily_pulls = %d, want 0 (day-1 delta always 0)", rows[0].DailyPulls)
	}
	if rows[1].FirstSeen {
		t.Errorf("rows[1] first_seen = true, want false (only day-1 marked)")
	}
	if rows[1].DailyPulls != 10 {
		t.Errorf("rows[1] daily_pulls = %d, want 10", rows[1].DailyPulls)
	}
}

func TestHandlePullsDaily_firstSeenOmittedWhenFalse(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	pullsDailyTwoDaySnap(store, 5000, 5010)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/pulls/daily?repo=owner/app", http.NoBody)
	w := httptest.NewRecorder()
	h.pullsDaily(w, req)

	body := w.Body.String()
	// Exactly one first_seen key in the response (for day-1).
	if n := strings.Count(body, `"first_seen"`); n != 1 {
		t.Errorf("first_seen key count = %d, want 1 (day-2 should omit)", n)
	}
}

// --- handler tests: /api/summary ---

func TestHandleSummary_withRegistryFilter(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))

	t.Run("dockerhub only", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/api/summary?registry=dockerhub", http.NoBody)
		w := httptest.NewRecorder()
		h.summary(w, req)

		var rows []struct{ Registry string }
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rows) != 1 || rows[0].Registry != "dockerhub" {
			t.Errorf("expected 1 dockerhub row, got %v", rows)
		}
	})

	t.Run("ghcr only", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/api/summary?registry=ghcr", http.NoBody)
		w := httptest.NewRecorder()
		h.summary(w, req)

		var rows []struct{ Registry string }
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(rows) != 1 || rows[0].Registry != "ghcr" {
			t.Errorf("expected 1 ghcr row, got %v", rows)
		}
	})
}

func TestHandleSummary_withRepoFilter(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/app1", PullCount: 100},
			{Repo: "owner/app2", PullCount: 200},
		},
		GHCR: []model.GhcrStats{{Package: "owner/pkg1", DownloadCount: 50}},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/summary?repo=owner/app1", http.NoBody)
	w := httptest.NewRecorder()
	h.summary(w, req)

	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "owner/app1" {
		t.Errorf("expected only owner/app1, got %v", rows)
	}
}

func TestHandleSummary_sortedOutput(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "z/repo", PullCount: 10},
			{Repo: "a/repo", PullCount: 20},
		},
		GHCR: []model.GhcrStats{{Package: "m/pkg", DownloadCount: 30}},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/summary", http.NoBody)
	w := httptest.NewRecorder()
	h.summary(w, req)

	type row struct {
		Registry string `json:"registry"`
		Name     string `json:"name"`
	}
	var rows []row
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
	// Sort order: dockerhub/a, dockerhub/z, ghcr/m.
	if rows[0].Registry != "dockerhub" || rows[0].Name != "a/repo" {
		t.Errorf("rows[0] = %+v, want dockerhub/a/repo", rows[0])
	}
	if rows[2].Registry != "ghcr" {
		t.Errorf("rows[2] = %+v, want ghcr", rows[2])
	}
}

func TestHandleSummary_excludesZeroDownloadGHCR(t *testing.T) {
	// Pins the zero-download GHCR skip inside handleSummary
	// (forEachSummaryEntry's GHCR branch).
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-06", &model.Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{
			{Repo: "owner/app", PullCount: 100},
		},
		GHCR: []model.GhcrStats{
			{Package: "owner/active", DownloadCount: 50},
			{Package: "owner/empty", DownloadCount: 0},
		},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/summary", http.NoBody)
	w := httptest.NewRecorder()
	h.summary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var rows []struct {
		Registry string `json:"registry"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2 (owner/empty should be excluded)", len(rows))
	}
	for _, r := range rows {
		if r.Name == "owner/empty" {
			t.Errorf("handleSummary included zero-download GHCR row %q, want excluded", r.Name)
		}
	}
}

// --- resolveSnapshot tests ---

func TestHandlersResolveSnapshot_latest(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-04", fixedSnapshot("2026-03-04", 10, 5))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 30, 15))
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 20, 10))

	snap, err := h.resolveSnapshot(t.Context(), "")
	if err != nil {
		t.Fatalf("resolveSnapshot: %v", err)
	}
	if snap.DockerHub[0].PullCount != 30 {
		t.Errorf("PullCount = %d, want 30 (latest)", snap.DockerHub[0].PullCount)
	}
}

func TestHandlersResolveSnapshot_specificDate(t *testing.T) {
	h, store, _ := newTestHandlers(t)
	store.Put("2026-03-05", fixedSnapshot("2026-03-05", 50, 25))
	store.Put("2026-03-06", fixedSnapshot("2026-03-06", 100, 50))

	snap, err := h.resolveSnapshot(t.Context(), "2026-03-05")
	if err != nil {
		t.Fatalf("resolveSnapshot: %v", err)
	}
	if snap.DockerHub[0].PullCount != 50 {
		t.Errorf("PullCount = %d, want 50 (from 2026-03-05)", snap.DockerHub[0].PullCount)
	}
}

func TestHandlersResolveSnapshot_invalidDate(t *testing.T) {
	// memStore returns "not found" for unknown keys; resolveSnapshot
	// surfaces the store error. Pre-refactor the store returned a
	// validation error for non-date strings; the contract here is
	// simply "some error" — equivalent.
	h, _, _ := newTestHandlers(t)
	_, err := h.resolveSnapshot(t.Context(), "not-a-date")
	if err == nil {
		t.Error("expected error for unknown key")
	}
}

// --- writeJSON additional coverage ---

func BenchmarkDailyDelta(b *testing.B) {
	logger := testsupport.QuietLogger()
	cases := []struct {
		name                 string
		prevDate, currDate   string
		prevPulls, currPulls int64
	}{
		{"consecutive", "2026-03-05", "2026-03-06", 100, 120},
		{"gap-3-days", "2026-03-05", "2026-03-08", 100, 130},
		{"counter-reset", "2026-03-05", "2026-03-06", 200, 50},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				dailyDelta(logger, "owner/app", c.prevDate, c.prevPulls, c.currDate, c.currPulls)
			}
		})
	}
}
