package testsupport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/model"
)

// RedirectTransport rewrites all outbound requests to point at a local
// test server. This lets tests exercise functions with hardcoded base
// URLs without modifying production code.
type RedirectTransport struct {
	Target *httptest.Server
}

func (rt *RedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	u := *req.URL
	target, err := req.URL.Parse(rt.Target.URL)
	if err != nil {
		return nil, fmt.Errorf("RedirectTransport: parse target URL %q: %w", rt.Target.URL, err)
	}
	u.Scheme = target.Scheme
	u.Host = target.Host
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}

// MockClient returns an *http.Client that redirects all requests to srv.
func MockClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: &RedirectTransport{Target: srv},
		Timeout:   5 * time.Second,
	}
}

// QuietLogger returns a logger that discards output, suitable for
// tests that don't assert on log lines.
func QuietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// MemStore is a configurable in-memory api.Store fake for tests.
// It supports error injection via exported fields so handler, collect,
// and contract tests can share a single implementation.
type MemStore struct {
	ByDate       map[string]*model.Snapshot
	LoadErr      map[string]error
	ListDatesErr error
	SaveErr      error
}

// Compile-time assertion: *MemStore satisfies api.Store.
var _ api.Store = (*MemStore)(nil)

// NewMemStore returns a ready-to-use MemStore with initialized maps.
func NewMemStore() *MemStore {
	return &MemStore{
		ByDate:  map[string]*model.Snapshot{},
		LoadErr: map[string]error{},
	}
}

func (m *MemStore) Save(_ context.Context, snap *model.Snapshot) error {
	if m.SaveErr != nil {
		return m.SaveErr
	}
	date := snap.Timestamp.UTC().Format("2006-01-02")
	m.ByDate[date] = snap
	return nil
}

func (m *MemStore) Load(_ context.Context, date string) (*model.Snapshot, error) {
	if err, ok := m.LoadErr[date]; ok {
		return nil, err
	}
	snap, ok := m.ByDate[date]
	if !ok {
		return nil, fmt.Errorf("not found: %s", date)
	}
	return snap, nil
}

func (m *MemStore) ListDates(_ context.Context) ([]string, error) {
	if m.ListDatesErr != nil {
		return nil, m.ListDatesErr
	}
	out := make([]string, 0, len(m.ByDate))
	for d := range m.ByDate {
		out = append(out, d)
	}
	slices.Sort(out)
	return out, nil
}

func (m *MemStore) Prune(_ context.Context, _ int) (int, error) { return 0, nil }
func (m *MemStore) CleanupStaleTmp(_ context.Context) error     { return nil }

func (m *MemStore) PullSeries(_ context.Context) []model.PullEntry {
	if m.ListDatesErr != nil {
		return nil
	}
	var entries []model.PullEntry
	for date, snap := range m.ByDate {
		if snap == nil {
			continue
		}
		if _, hasErr := m.LoadErr[date]; hasErr {
			continue
		}
		entries = append(entries, snap.PullEntries(date)...)
	}
	return entries
}

// Put is a test helper that inserts a snapshot by date key without
// going through Save (bypasses SaveErr injection).
func (m *MemStore) Put(date string, snap *model.Snapshot) {
	m.ByDate[date] = snap
}
