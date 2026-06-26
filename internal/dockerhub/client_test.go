package dockerhub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/dockerhub"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/testsupport"
)

// Compile-time assertion kept here (not in the package) so a change that
// narrows api.RegistrySource past what *Client satisfies trips the build
// in the test binary before reaching any other consumer.
var _ api.RegistrySource = (*dockerhub.Client)(nil)

func mockClient(srv *httptest.Server) *http.Client {
	return testsupport.MockClient(srv)
}

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.Option {
	return []httpx.Option{httpx.WithBaseDelay(time.Millisecond)}
}

func TestClient_Name(t *testing.T) {
	c := dockerhub.NewClient(http.DefaultClient, shortRetry(), 0, testsupport.QuietLogger())
	if got := c.Name(); got != "dockerhub" {
		t.Errorf("Name() = %q, want dockerhub", got)
	}
}

func TestClient_Collect_ExplicitRef(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/myapp/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"pull_count": 5000, "last_updated": "2026-03-06T12:00:00Z",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/myapp/tags/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"name": "latest", "digest": "sha256:abc", "full_size": 1024},
			},
			"next": "",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "myapp"}}
	entries, attempted, healthy := c.Collect(context.Background(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1", attempted)
	}
	if !healthy {
		t.Errorf("healthy = false, want true")
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].Name != "owner/myapp" || entries[0].PullCount != 5000 {
		t.Errorf("entries[0] = %+v, want owner/myapp with 5000 pulls", entries[0])
	}
	if len(entries[0].Tags) != 1 {
		t.Errorf("entries[0].Tags len = %d, want 1", len(entries[0].Tags))
	}
}

func TestClient_Collect_Wildcard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"name": "app1", "pull_count": 100, "last_updated": "2026-03-06T12:00:00Z"},
				{"name": "app2", "pull_count": 200, "last_updated": "2026-03-05T12:00:00Z"},
			},
			"next": "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/app1/tags/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"name": "latest", "digest": "sha256:a1"}},
			"next":    "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/app2/tags/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"name": "v1", "digest": "sha256:a2"}},
			"next":    "",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "*"}}
	entries, attempted, healthy := c.Collect(context.Background(), refs)

	if attempted != 2 {
		t.Errorf("attempted = %d, want 2", attempted)
	}
	if !healthy {
		t.Errorf("healthy = false, want true")
	}
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
}

func TestClient_Collect_WildcardDedupAgainstExplicit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"name": "app1", "pull_count": 100, "last_updated": "2026-03-06T12:00:00Z"},
			},
			"next": "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/app1/tags/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "app1"},
	}
	entries, attempted, _ := c.Collect(context.Background(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1 (wildcard covered the explicit ref)", attempted)
	}
	if len(entries) != 1 {
		t.Errorf("entries len = %d, want 1 (deduped)", len(entries))
	}
}

func TestClient_Collect_DegradedOnAllFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "a"}, {Owner: "owner", Repo: "b"}}
	entries, attempted, healthy := c.Collect(context.Background(), refs)

	if attempted != 2 {
		t.Errorf("attempted = %d, want 2", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("entries len = %d, want 0 (all fetches failed)", len(entries))
	}
	if healthy {
		t.Errorf("healthy = true, want false (2/2 failures = fully degraded)")
	}
}

func TestClient_Collect_WildcardListError_SkipsButContinues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/bad/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /v2/repositories/good/a/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"pull_count": 42, "last_updated": "2026-03-06T12:00:00Z"})
	})
	mux.HandleFunc("GET /v2/repositories/good/a/tags/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "bad", Repo: "*"}, {Owner: "good", Repo: "a"}}
	entries, _, _ := c.Collect(context.Background(), refs)

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1 (good/a survives bad wildcard listing)", len(entries))
	}
	if entries[0].Name != "good/a" {
		t.Errorf("entries[0].Name = %q, want good/a", entries[0].Name)
	}
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	repos, err := c.ListRepos(context.Background(), "testowner")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	_, err := c.ListRepos(context.Background(), "testowner")
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := c.CollectTags(context.Background(), "owner/app")
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := c.CollectTags(context.Background(), "owner/app")
	if len(tags) != 0 {
		t.Errorf("tags len = %d, want 0 on fetch error", len(tags))
	}
}

func TestClient_CollectTags_ParseErrorReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := c.CollectTags(context.Background(), "owner/app")
	if len(tags) != 0 {
		t.Errorf("tags len = %d, want 0 on parse error", len(tags))
	}
}

func TestClient_PageCap_TruncatesOwnerListing(t *testing.T) {
	// A tiny pageCap (1) should visit only one page of the owner listing
	// even if the server signals "next" — proving the cap is applied.
	// Count requests to the owner-listing path specifically (the mux
	// pattern /v2/repositories/o/ would otherwise also match nested
	// paths like /v2/repositories/o/a/tags/, so filter by exact URL).
	ownerPages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/repositories/o/" {
			ownerPages++
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"name": "a", "pull_count": 1}},
				"next":    "keep-going", // server keeps offering more pages
			})
			return
		}
		// Tags endpoint etc. under the same prefix: return empty so
		// the wildcard expansion's tag fetch for "a" doesn't loop.
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 1, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "o", Repo: "*"}}
	_, attempted, _ := c.Collect(context.Background(), refs)

	if ownerPages != 1 {
		t.Errorf("owner-listing requests = %d, want 1 (pageCap=1 enforced)", ownerPages)
	}
	if attempted != 1 {
		t.Errorf("attempted = %d, want 1 (one repo from the capped page)", attempted)
	}
}

func TestDegraded(t *testing.T) {
	tests := []struct {
		name      string
		results   []model.RepoStats
		attempted int
		want      bool
	}{
		{"zero attempted", nil, 0, false},
		{"all failed (0 of 3)", nil, 3, true},
		{"empty results (0 of 1)", []model.RepoStats{}, 1, true},
		{"majority failed (1 of 3)", []model.RepoStats{{Repo: "a/b"}}, 3, true},
		{"exactly half (1 of 2)", []model.RepoStats{{Repo: "a/b"}}, 2, false},
		{"all succeeded (2 of 2)", []model.RepoStats{{Repo: "a/b"}, {Repo: "c/d"}}, 2, false},
		{"one of one", []model.RepoStats{{Repo: "a/b"}}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dockerhub.Degraded(tt.results, tt.attempted)
			if got != tt.want {
				t.Errorf("Degraded(len=%d, attempted=%d) = %v, want %v",
					len(tt.results), tt.attempted, got, tt.want)
			}
		})
	}
}

func TestClient_Collect_ExplicitRefParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "myapp"}}
	entries, attempted, _ := c.Collect(context.Background(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("entries len = %d, want 0 on parse error", len(entries))
	}
}

func TestClient_Collect_ExplicitRefFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	refs := []model.RepoRef{{Owner: "owner", Repo: "myapp"}}
	entries, attempted, healthy := c.Collect(context.Background(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("entries len = %d, want 0 on fetch error", len(entries))
	}
	if healthy {
		t.Errorf("healthy = true, want false (1/1 failure)")
	}
}

// TestClient_CollectTags_ExactPageCount mirrors the legacy
// TestCollectDockerHubTagsExactPageCount mutation-hunt test: verifies that
// CollectTags walks exactly maxPages pages when the server keeps signaling
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	tags := c.CollectTags(context.Background(), "o/a")

	if len(tags) != 2 {
		t.Errorf("CollectTags() = %d tags, want 2", len(tags))
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	repos, err := c.ListRepos(context.Background(), "o")
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("ListRepos() = %d repos, want 2", len(repos))
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

// captureLogger returns a logger that records everything (Debug and up)
// into the returned buffer, so a test can assert which log lines a
// Client emitted.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestClient_NilLogger_DoesNotPanic verifies a Client built with a nil
// logger falls back to a usable default: an error path that logs must
// not nil-panic. CollectTags against a failing server hits the Error
// log path, so a missing fallback would crash here.
func TestClient_NilLogger_DoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, nil)
	tags := c.CollectTags(context.Background(), "o/a")
	if len(tags) != 0 {
		t.Errorf("CollectTags on a failing server = %d tags, want 0", len(tags))
	}
}

// TestClient_Collect_WildcardListingError_LogsWarn pins the partial-
// failure warn in the wildcard expansion path: a failing owner listing
// must log the warning, and a successful one must stay silent. Driving
// it through the public Collect (with a capturing logger) also confirms
// the supplied logger is the one actually used.
func TestClient_Collect_WildcardListingError_LogsWarn(t *testing.T) {
	const warnMsg = "docker hub listing partially failed"
	wildcard := []model.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("listing_error_logs_warn", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		logger, buf := captureLogger()
		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, logger)
		c.Collect(context.Background(), wildcard)

		if !strings.Contains(buf.String(), warnMsg) {
			t.Errorf("Collect with a failing wildcard listing did not log %q; logs:\n%s", warnMsg, buf.String())
		}
	})

	t.Run("listing_ok_silent", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		logger, buf := captureLogger()
		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, logger)
		c.Collect(context.Background(), wildcard)

		if strings.Contains(buf.String(), warnMsg) {
			t.Errorf("Collect with a successful wildcard listing logged %q, want silence; logs:\n%s", warnMsg, buf.String())
		}
	})
}

// TestClient_CollectTags_WalksToPageCap forces the page cap to 3 while
// the server always offers another page, so only the cap stops the loop:
// CollectTags must walk exactly 3 pages (3 tags, 3 requests). Pins the
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

	c := dockerhub.NewClient(mockClient(srv), shortRetry(), pageCap, testsupport.QuietLogger())
	tags := c.CollectTags(context.Background(), "o/a")

	if len(tags) != pageCap {
		t.Errorf("CollectTags collected %d tags, want %d (cap=%d)", len(tags), pageCap, pageCap)
	}
	if requests != pageCap {
		t.Errorf("tags page requests = %d, want %d", requests, pageCap)
	}
}
