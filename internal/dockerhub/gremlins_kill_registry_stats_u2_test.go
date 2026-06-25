package dockerhub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// gk_registry_stats_u2_capHandler records slog message texts so a test
// can assert whether a specific Client log line was (or was not) emitted.
type gk_registry_stats_u2_capHandler struct {
	msgs []string
	mu   sync.Mutex
}

func (h *gk_registry_stats_u2_capHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *gk_registry_stats_u2_capHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}

func (h *gk_registry_stats_u2_capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gk_registry_stats_u2_capHandler) WithGroup(string) slog.Handler      { return h }

func (h *gk_registry_stats_u2_capHandler) gk_registry_stats_u2_saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}

// gk_registry_stats_u2_shortRetry keeps retry backoff at 1ms so the
// failing-listing path resolves quickly.
func gk_registry_stats_u2_shortRetry() []httpx.Option {
	return []httpx.Option{httpx.WithBaseDelay(time.Millisecond)}
}

// TestNewClient_logger_fallback_negation kills the CONDITIONALS_NEGATION
// mutant at client.go:61 (`logger == nil` -> `!= nil`). A provided logger
// must be stored verbatim; a nil logger must fall back to a non-nil
// default. The mutant would do the opposite in both cases.
func TestNewClient_logger_fallback_negation(t *testing.T) {
	custom := slog.New(slog.DiscardHandler)
	c := NewClient(http.DefaultClient, nil, 0, custom)
	if c.logger != custom {
		t.Error("NewClient(logger=custom): provided logger was replaced, want it kept")
	}

	c2 := NewClient(http.DefaultClient, nil, 0, nil)
	if c2.logger == nil {
		t.Error("NewClient(logger=nil): c.logger = nil, want slog.Default() fallback (non-nil)")
	}
}

// TestCollectWildcards_list_error_warn_negation kills the
// CONDITIONALS_NEGATION mutant at client.go:150 (`err != nil` -> `== nil`)
// in collectWildcards. A failing owner listing must log the partial-
// failure warning; a successful listing must stay silent. The mutant
// inverts both observations.
func TestCollectWildcards_list_error_warn_negation(t *testing.T) {
	const warnMsg = "docker hub listing partially failed"
	wildcard := []model.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("list_error_logs_warn", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		h := &gk_registry_stats_u2_capHandler{}
		c := NewClient(testsupport.MockClient(srv), gk_registry_stats_u2_shortRetry(), 0, slog.New(h))
		collectWildcards(t.Context(), c, wildcard)

		if !h.gk_registry_stats_u2_saw(warnMsg) {
			t.Errorf("collectWildcards with a failing listing did not log %q, want it", warnMsg)
		}
	})

	t.Run("list_ok_silent", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		h := &gk_registry_stats_u2_capHandler{}
		c := NewClient(testsupport.MockClient(srv), gk_registry_stats_u2_shortRetry(), 0, slog.New(h))
		collectWildcards(t.Context(), c, wildcard)

		if h.gk_registry_stats_u2_saw(warnMsg) {
			t.Errorf("collectWildcards with a successful listing logged %q, want silence", warnMsg)
		}
	})
}

// TestCollectTags_page_loop_boundary kills the CONDITIONALS_BOUNDARY
// mutant at client.go:278 (`page <= maxPages` -> `page < maxPages`) in
// collectTags. With the page cap forced to 3 and the server always
// offering another page, `<=` walks exactly 3 pages (3 tags / 3 requests)
// while `<` walks only 2.
func TestCollectTags_page_loop_boundary(t *testing.T) {
	const pageCap = 3
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/a/tags/", func(w http.ResponseWriter, r *http.Request) {
		requests++
		page := r.URL.Query().Get("page")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"name": "tag-" + page}},
			"next":    "always-more", // never terminate via Next; only the page cap stops the loop
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(testsupport.MockClient(srv), gk_registry_stats_u2_shortRetry(), pageCap, testsupport.QuietLogger())
	tags := c.CollectTags(t.Context(), "o/a")

	if len(tags) != pageCap {
		t.Errorf("CollectTags collected %d tags, want %d (page <= maxPages over cap=%d)",
			len(tags), pageCap, pageCap)
	}
	if requests != pageCap {
		t.Errorf("tags page requests = %d, want %d", requests, pageCap)
	}
}
