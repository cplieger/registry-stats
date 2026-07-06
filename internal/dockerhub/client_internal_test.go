package dockerhub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// mockClient wires an *http.Client whose transport redirects all requests
// to the test server. Defined here (package dockerhub) so the white-box
// tests below can build a Client without reaching into the external
// dockerhub_test package, where the black-box suite keeps its own copy.
func mockClient(srv *httptest.Server) *http.Client {
	return testsupport.MockClient(srv)
}

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.Option {
	return []httpx.Option{httpx.WithBaseDelay(time.Millisecond)}
}

func TestClient_ListRepos_PaginatesOwner(t *testing.T) {
	pageRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/testowner/", func(w http.ResponseWriter, r *http.Request) {
		pageRequests++
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{
					{"name": "app1", "pull_count": 100, "last_updated": "2026-03-06T12:00:00Z"},
					{"name": "app2", "pull_count": 200, "last_updated": "2026-03-05T12:00:00Z"},
				},
				"next": "page2",
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{
					{"name": "app3", "pull_count": 50, "last_updated": "2026-03-04T12:00:00Z"},
				},
				"next": "",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	repos, err := listRepos(context.Background(), c, "testowner")
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("repos len = %d, want 3", len(repos))
	}
	if pageRequests != 2 {
		t.Errorf("page requests = %d, want 2", pageRequests)
	}
	if repos[2].Repo != "testowner/app3" {
		t.Errorf("repos[2].Repo = %q, want testowner/app3", repos[2].Repo)
	}
}

func TestClient_ListRepos_ParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	_, err := listRepos(context.Background(), c, "testowner")
	if err == nil {
		t.Error("expected parse error for invalid JSON")
	}
}

func TestClient_CollectTags_PaginatesRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/app/tags/", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "latest", "digest": "sha256:abc"}},
				"next":    "page2",
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "v1", "digest": "sha256:xyz"}},
				"next":    "",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := collectTags(context.Background(), c, "owner/app")
	if len(tags) != 2 {
		t.Fatalf("tags len = %d, want 2", len(tags))
	}
	if tags[0].Name != "latest" || tags[1].Name != "v1" {
		t.Errorf("tags = %+v, want [latest, v1]", tags)
	}
}

func TestClient_CollectTags_FetchErrorReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := collectTags(context.Background(), c, "owner/app")
	if len(tags) != 0 {
		t.Errorf("tags len = %d, want 0 on fetch error", len(tags))
	}
}

func TestClient_CollectTags_ParseErrorReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := collectTags(context.Background(), c, "owner/app")
	if len(tags) != 0 {
		t.Errorf("tags len = %d, want 0 on parse error", len(tags))
	}
}

// TestClient_CollectTags_ExactPageCount mirrors the legacy
// TestCollectDockerHubTagsExactPageCount mutation-hunt test: verifies that
// collectTags walks exactly maxPages pages when the server keeps signaling
// "next". Kills boundary and increment mutants on the page loop.
func TestClient_CollectTags_ExactPageCount(t *testing.T) {
	pageRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/a/tags/", func(w http.ResponseWriter, r *http.Request) {
		pageRequests++
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "v1", "digest": "sha256:a"}},
				"next":    "page2",
			})
		case "2":
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "v2", "digest": "sha256:b"}},
				"next":    "",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := collectTags(context.Background(), c, "o/a")

	if len(tags) != 2 {
		t.Errorf("collectTags() = %d tags, want 2", len(tags))
	}
	if tags[0].Name != "v1" || tags[1].Name != "v2" {
		t.Errorf("tags = %+v, want [v1, v2]", tags)
	}
	if pageRequests != 2 {
		t.Errorf("page requests = %d, want 2", pageRequests)
	}
}

// TestClient_ListRepos_ExactPageCount mirrors the legacy
// TestListDockerHubReposExactPageCount mutation-hunt test.
func TestClient_ListRepos_ExactPageCount(t *testing.T) {
	pageRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		pageRequests++
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "a1", "pull_count": 10}},
				"next":    "page2",
			})
		case "2":
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "a2", "pull_count": 20}},
				"next":    "",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	repos, err := listRepos(context.Background(), c, "o")
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("listRepos() = %d repos, want 2", len(repos))
	}
	if repos[0].Repo != "o/a1" || repos[0].PullCount != 10 {
		t.Errorf("repos[0] = %+v, want o/a1 with 10 pulls", repos[0])
	}
	if repos[1].Repo != "o/a2" || repos[1].PullCount != 20 {
		t.Errorf("repos[1] = %+v, want o/a2 with 20 pulls", repos[1])
	}
	if pageRequests != 2 {
		t.Errorf("page requests = %d, want 2", pageRequests)
	}
}

// TestClient_NilLogger_DoesNotPanic verifies a Client built with a nil
// logger falls back to a usable default: a path that logs must not
// nil-panic. collectTags against a failing server hits the warn log
// path, so a missing fallback would crash here.
func TestClient_NilLogger_DoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, nil)
	tags := collectTags(context.Background(), c, "o/a")
	if len(tags) != 0 {
		t.Errorf("collectTags on a failing server = %d tags, want 0", len(tags))
	}
}

// TestClient_CollectTags_WalksToPageCap forces the page cap to 3 while
// the server always offers another page, so only the cap stops the loop:
// collectTags must walk exactly 3 pages (3 tags, 3 requests). Pins the
// page-loop upper bound.
func TestClient_CollectTags_WalksToPageCap(t *testing.T) {
	const pageCap = 3
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/a/tags/", func(w http.ResponseWriter, r *http.Request) {
		requests++
		page := r.URL.Query().Get("page")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"name": "tag-" + page}},
			"next":    "always-more",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), pageCap, testsupport.QuietLogger())
	tags := collectTags(context.Background(), c, "o/a")

	if len(tags) != pageCap {
		t.Errorf("collectTags collected %d tags, want %d (cap=%d)", len(tags), pageCap, pageCap)
	}
	if requests != pageCap {
		t.Errorf("tags page requests = %d, want %d", requests, pageCap)
	}
}
