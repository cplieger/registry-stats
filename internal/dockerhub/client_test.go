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

// TestClient_Collect_WildcardListingFailure_Health pins finding l-f4: the
// healthy verdict must distinguish a wholesale wildcard listing outage
// (unhealthy) from a legitimately-empty owner and a partial failure (both
// healthy). Without the wholesale-outage signal a total Docker Hub listing
// outage leaves attempted == 0, which the severe-degradation rule alone
// reads as healthy — masking the outage from collect_errors_total.
func TestClient_Collect_WildcardListingFailure_Health(t *testing.T) {
	wildcard := []model.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("wholesale_failure_is_unhealthy", func(t *testing.T) {
		// Owner listing 404s on page 1 → zero usable repos → wholesale outage.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
		entries, attempted, healthy := c.Collect(context.Background(), wildcard)

		if healthy {
			t.Errorf("healthy = true, want false (wildcard listing wholly failed)")
		}
		if len(entries) != 0 {
			t.Errorf("entries len = %d, want 0", len(entries))
		}
		if attempted != 0 {
			t.Errorf("attempted = %d, want 0 (nothing listed to attempt)", attempted)
		}
	})

	t.Run("legitimately_empty_owner_is_healthy", func(t *testing.T) {
		// Owner listing succeeds with zero repos → not an outage, stays healthy.
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
		entries, _, healthy := c.Collect(context.Background(), wildcard)

		if !healthy {
			t.Errorf("healthy = false, want true (owner legitimately has zero repos)")
		}
		if len(entries) != 0 {
			t.Errorf("entries len = %d, want 0", len(entries))
		}
	})

	t.Run("partial_failure_is_healthy", func(t *testing.T) {
		// Page 1 returns one repo and signals another page; page 2 fails.
		// listRepos returns the page-1 repo alongside the error → partial,
		// not wholesale → stays healthy and serves the partial result.
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"results": []map[string]any{{"name": "a1", "pull_count": 1, "last_updated": "2026-03-06T12:00:00Z"}},
					"next":    "page2",
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		mux.HandleFunc("GET /v2/repositories/o/a1/tags/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
		entries, attempted, healthy := c.Collect(context.Background(), wildcard)

		if !healthy {
			t.Errorf("healthy = false, want true (partial listing failure, one repo returned)")
		}
		if len(entries) != 1 || entries[0].Name != "o/a1" {
			t.Errorf("entries = %+v, want one entry o/a1 (partial results served)", entries)
		}
		if attempted != 1 {
			t.Errorf("attempted = %d, want 1", attempted)
		}
	})
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

// captureLogger returns a logger that records everything (Debug and up)
// into the returned buffer, so a test can assert which log lines a
// Client emitted.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestClient_Collect_WildcardListingError_LogsWarn pins the wildcard
// expansion warn logs: a wholesale listing failure (zero usable repos)
// logs the "wholly failed" warn, a partial failure (some pages returned)
// logs the "partially failed" warn, and a successful listing stays
// silent. Driving it through the public Collect (with a capturing
// logger) also confirms the supplied logger is the one actually used.
func TestClient_Collect_WildcardListingError_LogsWarn(t *testing.T) {
	const (
		whollyMsg    = "docker hub listing wholly failed"
		partiallyMsg = "docker hub listing partially failed"
	)
	wildcard := []model.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("wholesale_error_logs_wholly", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		logger, buf := captureLogger()
		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, logger)
		c.Collect(context.Background(), wildcard)

		if !strings.Contains(buf.String(), whollyMsg) {
			t.Errorf("Collect with a wholesale listing failure did not log %q; logs:\n%s", whollyMsg, buf.String())
		}
		if strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a wholesale listing failure logged %q (want wholly, not partially); logs:\n%s", partiallyMsg, buf.String())
		}
	})

	t.Run("partial_error_logs_partially", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"results": []map[string]any{{"name": "a1", "pull_count": 1}},
					"next":    "page2",
				})
			default:
				w.WriteHeader(http.StatusNotFound) // page 2 fails; page 1 already yielded a repo
			}
		})
		mux.HandleFunc("GET /v2/repositories/o/a1/tags/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "next": ""})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		logger, buf := captureLogger()
		c := dockerhub.NewClient(mockClient(srv), shortRetry(), 0, logger)
		c.Collect(context.Background(), wildcard)

		if !strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a partial listing failure did not log %q; logs:\n%s", partiallyMsg, buf.String())
		}
		if strings.Contains(buf.String(), whollyMsg) {
			t.Errorf("Collect with a partial listing failure logged %q (want partially, not wholly); logs:\n%s", whollyMsg, buf.String())
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

		if strings.Contains(buf.String(), whollyMsg) || strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a successful wildcard listing logged a failure warn, want silence; logs:\n%s", buf.String())
		}
	})
}

// TestParseRepoListPage_dropsUnsafeName asserts the ParseRepoListPage
// security guard: a listing name carrying URL metacharacters (a slash
// here) is a path/query-injection vector into the tags URL built from it
// in collectTags, so it must be dropped while safe names on the same page
// survive. A removed guard cannot be caught by FuzzDockerHubRepoListUnmarshal's
// owner-prefix invariant (an unsafe name kept with the "owner/" prefix still
// satisfies it), so this direct assertion is the only thing that pins the drop.
func TestParseRepoListPage_dropsUnsafeName(t *testing.T) {
	data := []byte(`{"next":"","results":[{"name":"bad/traversal","pull_count":1},{"name":"good","pull_count":2}]}`)
	_, repos, err := dockerhub.ParseRepoListPage(data, "owner")
	if err != nil {
		t.Fatalf("ParseRepoListPage(%q) error = %v", data, err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos len = %d, want 1 (unsafe name dropped, safe survives)", len(repos))
	}
	if repos[0].Repo != "owner/good" || repos[0].PullCount != 2 {
		t.Errorf("repos[0] = %+v, want owner/good with 2 pulls", repos[0])
	}
}
