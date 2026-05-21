package webapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"registry-stats/internal/api"
	"registry-stats/internal/model"
)

// handlers holds the per-request dependencies the HTTP endpoints need.
// Constructed once at startup by New and shared across requests. Every
// field is safe for concurrent use: api.Store implementations must be
// thread-safe (FS is), api.HealthSignal likewise, and slog.Logger is
// inherently concurrent-safe.
//
// Field names carry a trailing signal noun so methods can share the
// short verb names (health, snapshot, pulls, etc.) without colliding.
type handlers struct {
	store   api.Store
	healthS api.HealthSignal
	logger  *slog.Logger
}

// newHandlers returns a handlers bound to the given dependencies.
// A nil logger falls back to slog.Default.
func newHandlers(store api.Store, health api.HealthSignal, logger *slog.Logger) *handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &handlers{store: store, healthS: health, logger: logger}
}

// resolveSnapshot fetches the snapshot for the requested date. When
// date is empty it falls back to the most recent available snapshot.
// Preserves the pre-refactor behavior of returning an error when no
// snapshots exist at all.
func (h *handlers) resolveSnapshot(ctx context.Context, date string) (*model.Snapshot, error) {
	if date != "" {
		return h.store.Load(ctx, date)
	}
	dates, err := h.store.ListDates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dates: %w", err)
	}
	if len(dates) == 0 {
		return nil, errors.New("no snapshots available")
	}
	return h.store.Load(ctx, dates[len(dates)-1])
}

// health handles GET /api/health. Returns the canonical JSON envelope
// shared across the homelab's custom Go apps: 200 with {"status":"ok"}
// when the health signal reports ready, 503 with
// {"status":"unready","reason":"..."} otherwise. Handler bodies
// downstream can assume the app is running normally.
func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.healthS == nil || !h.healthS.Healthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{
			"status": "unready",
			"reason": "health signal reports unhealthy (no successful collect yet)",
		}, h.logger)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"}, h.logger)
}

// snapshot handles GET /api/snapshot[?date=YYYY-MM-DD]. Returns the
// requested (or latest) full snapshot as JSON, or 404 when the date
// is absent or corrupt. The URL query parameter name and the
// "snapshot not found" error body match the pre-refactor contract.
func (h *handlers) snapshot(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	snap, err := h.resolveSnapshot(r.Context(), date)
	if err != nil {
		h.logger.Warn("snapshot not found", "date", date, "error", err)
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	writeJSON(w, snap, h.logger)
}

// forEachFilteredPull reads the pre-computed pull index and yields
// every (date, repoPull) that matches the request's repo + registry
// filters. Uses the index instead of iterating all snapshot files.
func (h *handlers) forEachFilteredPull(
	r *http.Request,
	fn func(date string, rp model.RepoPull),
) {
	repoFilter := parseRepoFilter(r.URL.Query()["repo"])
	filter := parseRegistryQuery(r.URL.Query().Get("registry"))

	entries := h.store.PullSeries(r.Context())

	// Merge entries by (date, repo) — same repo may appear from
	// multiple registries and their pull counts are summed.
	type key struct{ date, repo string }
	merged := map[key]int64{}
	for _, e := range entries {
		if !filter.Includes(e.Source) {
			continue
		}
		if repoFilter != nil && !repoFilter[e.Repo] {
			continue
		}
		merged[key{e.Date, e.Repo}] += e.PullCount
	}

	for k, pulls := range merged {
		fn(k.date, model.RepoPull{Repo: k.repo, PullCount: pulls})
	}
}

// pulls handles GET /api/pulls. Returns per-snapshot per-repo pull
// counts, filtered by optional repo + registry query params, sorted
// (timestamp, repo) so the JSON body is stable across calls.
func (h *handlers) pulls(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Timestamp string `json:"timestamp"`
		Repo      string `json:"repo"`
		PullCount int64  `json:"pull_count"`
	}
	rows := []row{}

	h.forEachFilteredPull(r, func(date string, rp model.RepoPull) {
		rows = append(rows, row{
			Timestamp: dateToISO(date),
			Repo:      rp.Repo,
			PullCount: rp.PullCount,
		})
	})

	slices.SortFunc(rows, func(a, b row) int {
		if a.Timestamp != b.Timestamp {
			return strings.Compare(a.Timestamp, b.Timestamp)
		}
		return strings.Compare(a.Repo, b.Repo)
	})

	h.logger.Debug("pulls response", "rows", len(rows))
	writeJSON(w, rows, h.logger)
}

// pullsDaily handles GET /api/pulls/daily. Returns per-snapshot
// per-repo delta pulls, carrying forward last-seen counts across
// missing (repo, registry) pairs so a transient scrape failure
// doesn't poison the series. Uses the pre-computed pull index.
func (h *handlers) pullsDaily(w http.ResponseWriter, r *http.Request) {
	repoFilter := parseRepoFilter(r.URL.Query()["repo"])
	filter := parseRegistryQuery(r.URL.Query().Get("registry"))

	entries := h.store.PullSeries(r.Context())

	// Build a sorted list of unique dates from the index.
	dateSet := map[string]struct{}{}
	for _, e := range entries {
		dateSet[e.Date] = struct{}{}
	}
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet {
		dates = append(dates, d)
	}
	slices.Sort(dates)

	// carry is keyed by repo name → registry source → last-seen pull
	// count. When a (repo, source) pair is absent from a snapshot
	// (transient scrape failure, rate limit), the carry value is added
	// into the day's merged total so the delta doesn't spuriously dip.
	carry := map[string]map[model.RegistrySource]int64{}

	type dateCount struct {
		date  string
		pulls int64
	}
	byRepo := map[string][]dateCount{}

	for _, date := range dates {
		reposToday := map[string]struct{}{}
		for _, e := range entries {
			if e.Date != date {
				continue
			}
			if !filter.Includes(e.Source) {
				continue
			}
			if repoFilter != nil && !repoFilter[e.Repo] {
				continue
			}
			if carry[e.Repo] == nil {
				carry[e.Repo] = map[model.RegistrySource]int64{}
			}
			carry[e.Repo][e.Source] = e.PullCount
			reposToday[e.Repo] = struct{}{}
		}

		for repo := range reposToday {
			var total int64
			for _, v := range carry[repo] {
				total += v
			}
			byRepo[repo] = append(byRepo[repo], dateCount{date: date, pulls: total})
		}
	}

	type row struct {
		Timestamp  string `json:"timestamp"`
		Repo       string `json:"repo"`
		DailyPulls int64  `json:"daily_pulls"`
		FirstSeen  bool   `json:"first_seen,omitempty"`
	}
	rows := []row{}

	for repo, counts := range byRepo {
		for i, c := range counts {
			var delta int64
			if i > 0 {
				delta = dailyDelta(h.logger, repo, counts[i-1].date, counts[i-1].pulls, c.date, c.pulls)
			}
			rows = append(rows, row{
				Timestamp:  dateToISO(c.date),
				Repo:       repo,
				DailyPulls: delta,
				FirstSeen:  i == 0,
			})
		}
	}

	slices.SortFunc(rows, func(a, b row) int {
		if a.Timestamp != b.Timestamp {
			return strings.Compare(a.Timestamp, b.Timestamp)
		}
		return strings.Compare(a.Repo, b.Repo)
	})

	h.logger.Debug("daily pulls response", "dates", len(dates), "rows", len(rows))
	writeJSON(w, rows, h.logger)
}

// summary handles GET /api/summary[?date=...]. Returns a flat list of
// (registry, name, pull_count, tag_count) rows for one snapshot,
// sorted (registry, name).
func (h *handlers) summary(w http.ResponseWriter, r *http.Request) {
	repoFilter := parseRepoFilter(r.URL.Query()["repo"])
	filter := parseRegistryQuery(r.URL.Query().Get("registry"))
	date := r.URL.Query().Get("date")
	snap, err := h.resolveSnapshot(r.Context(), date)
	if err != nil {
		h.logger.Warn("snapshot not found", "date", date, "error", err)
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}

	type row struct {
		Registry  string `json:"registry"`
		Name      string `json:"name"`
		PullCount int64  `json:"pull_count"`
		TagCount  int    `json:"tag_count"`
	}
	rows := []row{}
	forEachSummaryEntry(snap, repoFilter, filter, func(_ model.RegistrySource, e model.SummaryEntry) {
		rows = append(rows, row(e))
	})

	slices.SortFunc(rows, func(a, b row) int {
		if a.Registry != b.Registry {
			return strings.Compare(a.Registry, b.Registry)
		}
		return strings.Compare(a.Name, b.Name)
	})

	writeJSON(w, rows, h.logger)
}

// dailyDelta computes the per-day pull delta from prev to curr,
// clamping counter resets to 0 and smoothing across missing days.
// Parse errors on listDates-sourced strings are impossible (dates
// are validated upstream by time.Parse in ListDates) but the code
// falls back to gap=1 defensively.
func dailyDelta(logger *slog.Logger, repo, prevDate string, prevPulls int64, currDate string, currPulls int64) int64 {
	diff := max(0, currPulls-prevPulls)
	gap := int64(1)
	t1, e1 := time.Parse("2006-01-02", prevDate)
	t2, e2 := time.Parse("2006-01-02", currDate)
	if e1 == nil && e2 == nil {
		if g := int64(t2.Sub(t1).Hours() / 24); g > 1 {
			gap = g
			if logger != nil {
				logger.Debug("daily delta smoothed across missing days",
					"repo", repo, "from", prevDate, "to", currDate, "gap_days", gap)
			}
		}
	}
	return diff / gap
}
