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

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
)

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
}

// tagCountHandler returns a tags-listing handler with the given total
// count, so a test passes only when the count field (not the page
// length) drives the value. "next" is deliberately non-empty: tagCount
// must not follow it.
func tagCountHandler(count int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"count":   count,
			"next":    "page2",
			"results": []map[string]any{{"name": "latest"}},
		})
	}
}

func TestClient_Name(t *testing.T) {
	c := dockerhub.NewClient(http.DefaultClient, dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	if got := c.Source().String(); got != "dockerhub" {
		t.Errorf("Name() = %q, want dockerhub", got)
	}
}

func TestClient_Collect_ExplicitRef(t *testing.T) {
	tagRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/myapp/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"pull_count": 5000})
	})
	mux.HandleFunc("GET /v2/repositories/owner/myapp/tags/", func(w http.ResponseWriter, r *http.Request) {
		tagRequests++
		tagCountHandler(164)(w, r)
	})
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "myapp"}}
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1", attempted)
	}
	if listingFailed {
		t.Errorf("listingFailed = true, want false")
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].Owner != "owner" || entries[0].Repo != "myapp" || entries[0].Pulls != 5000 {
		t.Errorf("entries[0] = %+v, want owner/myapp with 5000 pulls", entries[0])
	}
	if entries[0].TagCount == nil || *entries[0].TagCount != 164 {
		t.Errorf("entries[0].TagCount = %v, want 164 (the count field, not the page length)", entries[0].TagCount)
	}
	if tagRequests != 1 {
		t.Errorf("tags requests = %d, want exactly 1 per repo per cycle", tagRequests)
	}
}

// TestClient_Collect_TagCountFailureKeepsEntry pins the skip-don't-invent
// contract for the tag count: a failing tags endpoint must not drop the
// repo's entry (pulls stay intact), but leaves TagCount nil because no value
// was measured.
func TestClient_Collect_TagCountFailureKeepsEntry(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantLevel string
		wantMsg   string
		wantShape bool
		absentMsg string
	}{
		{"fetch_failure", http.StatusInternalServerError, "", "WARN", "docker hub tag count fetch failed", false, "docker hub tag count parse failed"},
		{"parse_failure", http.StatusOK, `{"results":[{"name":"latest"}]}`, "ERROR", "docker hub tag count parse failed", true, "docker hub tag count fetch failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v2/repositories/owner/myapp/", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"pull_count": 5000})
			})
			mux.HandleFunc("GET /v2/repositories/owner/myapp/tags/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			srv := httptest.NewTestServer(t, mux)

			logger, buf := captureLogger()
			c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
			entries, attempted, listingFailed := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "myapp"}})

			if attempted != 1 || listingFailed {
				t.Errorf("Collect tag-count %s = (attempted=%d, listingFailed=%v), want (1, false)", tt.name, attempted, listingFailed)
			}
			if len(entries) != 1 {
				t.Fatalf("Collect tag-count %s returned %d entries, want 1", tt.name, len(entries))
			}
			if entries[0].Pulls != 5000 || entries[0].TagCount != nil {
				t.Errorf("Collect tag-count %s entry = %+v, want pulls 5000 with no TagCount", tt.name, entries[0])
			}
			logs := buf.String()
			if !strings.Contains(logs, "level="+tt.wantLevel) || !strings.Contains(logs, `msg="`+tt.wantMsg+`"`) ||
				!strings.Contains(logs, "repo=owner/myapp") || !strings.Contains(logs, "error=") {
				t.Errorf("Collect tag-count %s did not log %s %q with repo and error; logs:\n%s", tt.name, tt.wantLevel, tt.wantMsg, logs)
			}
			if tt.wantShape && !strings.Contains(logs, "shape_change=true") {
				t.Errorf("Collect tag-count %s did not classify the response-shape failure; logs:\n%s", tt.name, logs)
			}
			if !tt.wantShape && strings.Contains(logs, "shape_change=true") {
				t.Errorf("Collect tag-count %s classified a fetch failure as a response-shape change; logs:\n%s", tt.name, logs)
			}
			if strings.Contains(logs, `msg="`+tt.absentMsg+`"`) {
				t.Errorf("Collect tag-count %s logged wrong branch %q; logs:\n%s", tt.name, tt.absentMsg, logs)
			}
		})
	}
}

func TestClient_Collect_Wildcard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"name": "app1", "pull_count": 100},
				{"name": "app2", "pull_count": 200},
			},
			"next": "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/app1/tags/", tagCountHandler(3))
	mux.HandleFunc("GET /v2/repositories/owner/app2/tags/", tagCountHandler(5))
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Errorf("attempted = %d, want 2", attempted)
	}
	if listingFailed {
		t.Errorf("listingFailed = true, want false")
	}
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
	}
	if entries[0].TagCount == nil || *entries[0].TagCount != 3 ||
		entries[1].TagCount == nil || *entries[1].TagCount != 5 {
		t.Errorf("tag counts = %v, %v, want 3, 5", entries[0].TagCount, entries[1].TagCount)
	}
}

func TestClient_Collect_WildcardTagFailuresKeepEntries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"name": "app1", "pull_count": 100},
				{"name": "app2", "pull_count": 200},
			},
			"next": "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/o/app1/tags/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /v2/repositories/o/app2/tags/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	entries, attempted, listingFailed := c.Collect(t.Context(), []registry.RepoRef{{Owner: "o", Repo: "*"}})

	if attempted != 2 || listingFailed {
		t.Errorf("Collect with wildcard tag failures = (attempted=%d, listingFailed=%v), want (2, false)", attempted, listingFailed)
	}
	if len(entries) != 2 {
		t.Fatalf("Collect with wildcard tag failures returned %d entries, want 2", len(entries))
	}
	if entries[0].Pulls != 100 || entries[0].TagCount != nil || entries[1].Pulls != 200 || entries[1].TagCount != nil {
		t.Errorf("Collect with wildcard tag failures entries = %+v, want both pull counts with no TagCount", entries)
	}
}

func TestClient_Collect_WildcardDedupAgainstExplicit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"count": 1,
			"results": []map[string]any{
				{"name": "app1", "pull_count": 100},
			},
			"next": "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/owner/app1/tags/", tagCountHandler(0))
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "app1"},
	}
	entries, attempted, _ := c.Collect(t.Context(), refs)

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1 (wildcard covered the explicit ref)", attempted)
	}
	if len(entries) != 1 {
		t.Errorf("entries len = %d, want 1 (deduped)", len(entries))
	}
}

func TestClient_Collect_AllExplicitFailuresAreNotListingFailure(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "a"}, {Owner: "owner", Repo: "b"}}
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if attempted != 2 {
		t.Errorf("attempted = %d, want 2", attempted)
	}
	if len(entries) != 0 {
		t.Errorf("entries len = %d, want 0 (all fetches failed)", len(entries))
	}
	if listingFailed {
		t.Error("listingFailed = true, want false for explicit-repo failures")
	}
}

func TestClient_Collect_WildcardListError_SkipsButContinues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/bad/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /v2/repositories/good/a/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"pull_count": 42})
	})
	mux.HandleFunc("GET /v2/repositories/good/a/tags/", tagCountHandler(1))
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{{Owner: "bad", Repo: "*"}, {Owner: "good", Repo: "a"}}
	entries, _, _ := c.Collect(t.Context(), refs)

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1 (good/a survives bad wildcard listing)", len(entries))
	}
	if entries[0].Owner != "good" || entries[0].Repo != "a" {
		t.Errorf("entries[0] = %+v, want good/a", entries[0])
	}
}

func TestClient_Collect_WildcardListingFailure(t *testing.T) {
	wildcard := []registry.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("wholesale_failure", func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
		entries, attempted, listingFailed := c.Collect(t.Context(), wildcard)

		if !listingFailed {
			t.Error("listingFailed = false, want true for a wholesale wildcard listing failure")
		}
		if len(entries) != 0 {
			t.Errorf("entries len = %d, want 0", len(entries))
		}
		if attempted != 0 {
			t.Errorf("attempted = %d, want 0 (nothing listed to attempt)", attempted)
		}
	})

	t.Run("legitimately_empty_owner", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []any{}, "next": ""})
		})
		srv := httptest.NewTestServer(t, mux)

		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
		entries, _, listingFailed := c.Collect(t.Context(), wildcard)

		if listingFailed {
			t.Error("listingFailed = true, want false for a legitimately empty owner")
		}
		if len(entries) != 0 {
			t.Errorf("entries len = %d, want 0", len(entries))
		}
	})

	t.Run("partial_failure", func(t *testing.T) {
		tests := []struct {
			name       string
			page2Status int
			page2Body   string
			wantError   string
			wantShape   bool
		}{
			{name: "http_error", page2Status: http.StatusNotFound, wantError: "list repos page 2"},
			{name: "parse_error", page2Status: http.StatusOK, page2Body: `{"count":1,"results":[],"results":[]}`, wantError: "parse repo list page 2", wantShape: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				mux := http.NewServeMux()
				mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Query().Get("page") {
					case "", "1":
						_ = json.NewEncoder(w).Encode(map[string]any{
							"count":   1,
							"results": []map[string]any{{"name": "a1", "pull_count": 1}},
							"next":    "page2",
						})
					default:
						w.WriteHeader(tt.page2Status)
						_, _ = w.Write([]byte(tt.page2Body))
					}
				})
				mux.HandleFunc("GET /v2/repositories/o/a1/tags/", tagCountHandler(2))
				srv := httptest.NewTestServer(t, mux)

				logger, buf := captureLogger()
				c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
				entries, attempted, listingFailed := c.Collect(t.Context(), wildcard)

				if listingFailed {
					t.Error("listingFailed = true, want false for a partial listing failure")
				}
				if len(entries) != 1 || entries[0].Owner != "o" || entries[0].Repo != "a1" {
					t.Errorf("entries = %+v, want one entry o/a1 (partial results served)", entries)
				}
				if attempted != 1 {
					t.Errorf("attempted = %d, want 1", attempted)
				}
				logs := buf.String()
				if !strings.Contains(logs, `msg="docker hub listing partially failed"`) || !strings.Contains(logs, tt.wantError) {
					t.Errorf("Collect() later-page %s did not log the partial-listing branch; logs:\n%s", tt.name, logs)
				}
				if strings.Contains(logs, "shape_change=true") != tt.wantShape {
					t.Errorf("Collect() later-page %s shape_change=true presence = %v, want %v; logs:\n%s", tt.name, strings.Contains(logs, "shape_change=true"), tt.wantShape, logs)
				}
			})
		}
	})
}

func TestClient_Collect_WildcardListingFailureIsSticky(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/bad/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /v2/repositories/good/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   1,
			"results": []map[string]any{{"name": "app", "pull_count": 42}},
			"next":    "",
		})
	})
	mux.HandleFunc("GET /v2/repositories/good/app/tags/", tagCountHandler(1))
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	refs := []registry.RepoRef{{Owner: "bad", Repo: "*"}, {Owner: "good", Repo: "*"}}
	entries, attempted, listingFailed := c.Collect(t.Context(), refs)

	if !listingFailed {
		t.Error("Collect after an earlier wildcard listing failure = listingFailed false, want true")
	}
	if attempted != 1 {
		t.Errorf("Collect after an earlier wildcard listing failure attempted = %d, want 1", attempted)
	}
	if len(entries) != 1 {
		t.Fatalf("Collect after an earlier wildcard listing failure returned %d entries, want 1", len(entries))
	}
	if entries[0].Owner != "good" || entries[0].Repo != "app" || entries[0].Pulls != 42 {
		t.Errorf("Collect after an earlier wildcard listing failure entry = %+v, want good/app with 42 pulls", entries[0])
	}
}

// TestClient_Collect_PartialExplicitFailureLogsRepo covers the explicit-ref
// state no other test reaches: exactly half the attempts fail, while the
// listing-failure fact stays false and the per-repo ERROR records the omitted
// repo. level=ERROR is what alerts/logql.yaml keys on.
func TestClient_Collect_PartialExplicitFailureLogsRepo(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantMsg   string
		absentMsg string
	}{
		{"fetch_failure", http.StatusNotFound, "", "docker hub fetch failed", "docker hub parse failed"},
		{"parse_failure", http.StatusOK, "not json", "docker hub parse failed", "docker hub fetch failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v2/repositories/good/app/", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"pull_count": 42})
			})
			mux.HandleFunc("GET /v2/repositories/good/app/tags/", tagCountHandler(1))
			mux.HandleFunc("GET /v2/repositories/bad/app/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			srv := httptest.NewTestServer(t, mux)

			logger, buf := captureLogger()
			c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
			refs := []registry.RepoRef{{Owner: "bad", Repo: "app"}, {Owner: "good", Repo: "app"}}
			entries, attempted, listingFailed := c.Collect(t.Context(), refs)

			if attempted != 2 || listingFailed {
				t.Errorf("Collect partial %s = (attempted=%d, listingFailed=%v), want (2, false)", tt.name, attempted, listingFailed)
			}
			if len(entries) != 1 {
				t.Fatalf("Collect partial %s returned %d entries, want 1", tt.name, len(entries))
			}
			if entries[0].Owner != "good" || entries[0].Repo != "app" || entries[0].Pulls != 42 {
				t.Errorf("Collect partial %s entry = %+v, want good/app with 42 pulls", tt.name, entries[0])
			}
			logs := buf.String()
			if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, `msg="`+tt.wantMsg+`"`) ||
				!strings.Contains(logs, "repo=bad/app") || !strings.Contains(logs, "error=") {
				t.Errorf("Collect partial %s did not log ERROR %q with repo and error; logs:\n%s", tt.name, tt.wantMsg, logs)
			}
			if strings.Contains(logs, `msg="`+tt.absentMsg+`"`) {
				t.Errorf("Collect partial %s logged wrong branch %q; logs:\n%s", tt.name, tt.absentMsg, logs)
			}
		})
	}
}

// TestClient_Collect_CancelledMidFetch_IsNotAnOutage pins the cancellation
// classification on the explicit-ref path: a stop is not a registry
// failure, so the in-flight fetch error is recorded at DEBUG, never the
// ERROR alerts/logql.yaml pages on.
func TestClient_Collect_CancelledMidFetch_IsNotAnOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	srv := httptest.NewTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Cancel with the request in flight, then hold the handler until
		// the client aborts, so the fetch deterministically fails on the
		// stop rather than racing the response body.
		cancel()
		<-r.Context().Done()
	}))

	logger, buf := captureLogger()
	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
	entries, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "myapp"}})

	if attempted != 1 {
		t.Errorf("attempted = %d, want 1 (the ref was tried)", attempted)
	}
	if len(entries) != 0 || listingFailed {
		t.Errorf("Collect on a cancelled cycle = (%d entries, listingFailed=%v), want (0, false)", len(entries), listingFailed)
	}
	logs := buf.String()
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("Collect on a cancelled cycle logged an ERROR, want the cancellation recorded at DEBUG; logs:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="docker hub fetch cancelled"`) {
		t.Errorf("Collect on a cancelled cycle did not record the cancelled fetch; logs:\n%s", logs)
	}
}

func TestClient_Collect_CancelledTagFetchKeepsPulls(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/myapp/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"pull_count": 5000})
	})
	mux.HandleFunc("GET /v2/repositories/owner/myapp/tags/", func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	srv := httptest.NewTestServer(t, mux)

	logger, buf := captureLogger()
	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
	entries, attempted, listingFailed := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "myapp"}})

	if attempted != 1 || listingFailed {
		t.Errorf("Collect after tag cancellation = (attempted=%d, listingFailed=%v), want (1, false)", attempted, listingFailed)
	}
	if len(entries) != 1 {
		t.Fatalf("Collect after tag cancellation returned %d entries, want 1", len(entries))
	}
	if entries[0].Pulls != 5000 || entries[0].TagCount != nil {
		t.Errorf("Collect after tag cancellation entry = %+v, want pulls 5000 with no TagCount", entries[0])
	}
	logs := buf.String()
	if !strings.Contains(logs, `msg="docker hub tag count fetch cancelled"`) {
		t.Errorf("Collect after tag cancellation did not record the cancelled tag fetch; logs:\n%s", logs)
	}
	if strings.Contains(logs, `msg="docker hub tag count fetch failed"`) || strings.Contains(logs, `msg="docker hub tag count parse failed"`) {
		t.Errorf("Collect after tag cancellation recorded a tag-count failure; logs:\n%s", logs)
	}
}

// captureLogger returns a logger recording everything (Debug and up)
// into the returned buffer.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestClient_Collect_WildcardListingError_LogsWarn pins the wildcard
// expansion warn logs: a wholesale listing failure logs the "wholly
// failed" warn, while a successful listing stays silent. Driving it
// through the public Collect with a capturing logger also confirms the
// supplied logger is used.
func TestClient_Collect_WildcardListingError_LogsWarn(t *testing.T) {
	const (
		whollyMsg    = "docker hub listing wholly failed"
		partiallyMsg = "docker hub listing partially failed"
	)
	wildcard := []registry.RepoRef{{Owner: "o", Repo: "*"}}

	t.Run("wholesale_error_logs_wholly", func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

		logger, buf := captureLogger()
		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
		c.Collect(t.Context(), wildcard)

		if !strings.Contains(buf.String(), whollyMsg) {
			t.Errorf("Collect with a wholesale listing failure did not log %q; logs:\n%s", whollyMsg, buf.String())
		}
		if strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a wholesale listing failure logged %q (want wholly, not partially); logs:\n%s", partiallyMsg, buf.String())
		}
	})

	t.Run("listing_ok_silent", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []any{}, "next": ""})
		})
		srv := httptest.NewTestServer(t, mux)

		logger, buf := captureLogger()
		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{RetryOpts: shortRetry(), Logger: logger})
		c.Collect(t.Context(), wildcard)

		if strings.Contains(buf.String(), whollyMsg) || strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a successful wildcard listing logged a failure warn, want silence; logs:\n%s", buf.String())
		}
	})
}
