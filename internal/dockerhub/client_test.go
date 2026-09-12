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

	"github.com/cplieger/registry-stats/v2/internal/dockerhub"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

func TestClient_Source(t *testing.T) {
	c := dockerhub.NewClient(http.DefaultClient, dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	if got := c.Source().String(); got != "dockerhub" {
		t.Errorf("Source().String() = %q, want dockerhub", got)
	}
}

func TestClient_Collect_ExplicitRef(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/myapp/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"pull_count": 5000})
	})
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "myapp"}}
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	fetched := collection.Fetched
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if fetched != 1 {
		t.Errorf("fetched = %d, want 1", fetched)
	}
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
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	fetched := collection.Fetched
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if fetched != 0 {
		t.Errorf("fetched = %d, want 0 (listing rows are not fetches)", fetched)
	}
	if attempted != 0 {
		t.Errorf("attempted = %d, want 0 (listing rows are not fetches)", attempted)
	}
	if listingFailed {
		t.Errorf("listingFailed = true, want false")
	}
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(entries))
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
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{
		{Owner: "owner", Repo: "*"},
		{Owner: "owner", Repo: "app1"},
	}
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	fetched := collection.Fetched
	attempted := collection.Attempted

	if fetched != 0 || attempted != 0 {
		t.Errorf("fetch counts = (fetched %d, attempted %d), want (0, 0); wildcard covered the explicit ref", fetched, attempted)
	}
	if len(entries) != 1 {
		t.Errorf("entries len = %d, want 1 (deduped)", len(entries))
	}
}

func TestClient_Collect_AllExplicitFailuresAreNotListingFailure(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{{Owner: "owner", Repo: "a"}, {Owner: "owner", Repo: "b"}}
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	fetched := collection.Fetched
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if attempted != 2 {
		t.Errorf("attempted = %d, want 2", attempted)
	}
	if fetched != 0 {
		t.Errorf("fetched = %d, want 0 (every explicit fetch failed)", fetched)
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
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{{Owner: "bad", Repo: "*"}, {Owner: "good", Repo: "a"}}
	entries := c.Collect(t.Context(), refs).Entries

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

		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
		collection := c.Collect(t.Context(), wildcard)
		entries := collection.Entries
		attempted := collection.Attempted
		listingFailed := collection.ListingFailed

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

	t.Run("partial_failure", func(t *testing.T) {
		tests := []struct {
			name        string
			page2Status int
			page2Body   string
			wantError   string
			wantLevel   string
		}{
			{name: "http_error", page2Status: http.StatusNotFound, wantError: "list repos page 2", wantLevel: "WARN"},
			{name: "parse_error", page2Status: http.StatusOK, page2Body: `{"count":1,"results":[],"results":[]}`, wantError: "parse repo list page 2", wantLevel: "ERROR"},
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
				srv := httptest.NewTestServer(t, mux)

				logger, buf := captureLogger()
				c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
				collection := c.Collect(t.Context(), wildcard)
				entries := collection.Entries
				attempted := collection.Attempted
				listingFailed := collection.ListingFailed

				if listingFailed {
					t.Error("listingFailed = true, want false for a partial listing failure")
				}
				if len(entries) != 1 || entries[0].Owner != "o" || entries[0].Repo != "a1" {
					t.Errorf("entries = %+v, want one entry o/a1 (partial results served)", entries)
				}
				if attempted != 0 {
					t.Errorf("attempted = %d, want 0 (listing rows are not fetches)", attempted)
				}
				logs := buf.String()
				if !strings.Contains(logs, `msg="docker hub listing partially failed"`) || !strings.Contains(logs, tt.wantError) {
					t.Errorf("Collect() later-page %s did not log the partial-listing branch; logs:\n%s", tt.name, logs)
				}
				if !strings.Contains(logs, "level="+tt.wantLevel) {
					t.Errorf("Collect() later-page %s level missing %s; logs:\n%s", tt.name, tt.wantLevel, logs)
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
	srv := httptest.NewTestServer(t, mux)

	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: slog.New(slog.DiscardHandler)})
	refs := []registry.RepoRef{{Owner: "bad", Repo: "*"}, {Owner: "good", Repo: "*"}}
	collection := c.Collect(t.Context(), refs)
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if !listingFailed {
		t.Error("Collect after an earlier wildcard listing failure = listingFailed false, want true")
	}
	if attempted != 0 {
		t.Errorf("Collect after an earlier wildcard listing failure attempted = %d, want 0 (listing rows are not fetches)", attempted)
	}
	if len(entries) != 1 {
		t.Fatalf("Collect after an earlier wildcard listing failure returned %d entries, want 1", len(entries))
	}
	if entries[0].Owner != "good" || entries[0].Repo != "app" || entries[0].Pulls != 42 {
		t.Errorf("Collect after an earlier wildcard listing failure entry = %+v, want good/app with 42 pulls", entries[0])
	}
}

// TestClient_Collect_PartialExplicitFailureLogsRepo covers the explicit-ref
// state no other test reaches: exactly half the attempts fail while the
// listing-failure fact stays false. alerts/logql.yaml matches both records.
func TestClient_Collect_PartialExplicitFailureLogsRepo(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantLevel string
		wantMsg   string
		absentMsg string
	}{
		{"fetch_failure", http.StatusNotFound, "", "WARN", "docker hub fetch failed", "docker hub parse failed"},
		{"parse_failure", http.StatusOK, "not json", "ERROR", "docker hub parse failed", "docker hub fetch failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v2/repositories/good/app/", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"pull_count": 42})
			})
			mux.HandleFunc("GET /v2/repositories/bad/app/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			srv := httptest.NewTestServer(t, mux)

			logger, buf := captureLogger()
			c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
			refs := []registry.RepoRef{{Owner: "bad", Repo: "app"}, {Owner: "good", Repo: "app"}}
			collection := c.Collect(t.Context(), refs)
			entries := collection.Entries
			fetched := collection.Fetched
			attempted := collection.Attempted
			listingFailed := collection.ListingFailed

			if fetched != 1 || attempted != 2 || listingFailed {
				t.Errorf("Collect partial %s = (fetched=%d, attempted=%d, listingFailed=%v), want (1, 2, false)", tt.name, fetched, attempted, listingFailed)
			}
			if len(entries) != 1 {
				t.Fatalf("Collect partial %s returned %d entries, want 1", tt.name, len(entries))
			}
			if entries[0].Owner != "good" || entries[0].Repo != "app" || entries[0].Pulls != 42 {
				t.Errorf("Collect partial %s entry = %+v, want good/app with 42 pulls", tt.name, entries[0])
			}
			logs := buf.String()
			if !strings.Contains(logs, "level="+tt.wantLevel) || !strings.Contains(logs, `msg="`+tt.wantMsg+`"`) ||
				!strings.Contains(logs, "repo=bad/app") || !strings.Contains(logs, "error=") {
				t.Errorf("Collect partial %s did not log %s %q with repo and error; logs:\n%s", tt.name, tt.wantLevel, tt.wantMsg, logs)
			}
			if strings.Contains(logs, `msg="`+tt.absentMsg+`"`) {
				t.Errorf("Collect partial %s logged wrong branch %q; logs:\n%s", tt.name, tt.absentMsg, logs)
			}
		})
	}
}

func TestClient_Collect_CancelledWildcardListingIsNotAnOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	srv := httptest.NewTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))

	logger, buf := captureLogger()
	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
	collection := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if listingFailed {
		t.Error("Collect with a cancelled wildcard listing = listingFailed true, want false")
	}
	if len(entries) != 0 || attempted != 0 {
		t.Errorf("Collect with a cancelled wildcard listing = (%d entries, attempted=%d), want (0, 0)", len(entries), attempted)
	}
	logs := buf.String()
	if strings.Contains(logs, "docker hub listing wholly failed") ||
		strings.Contains(logs, "docker hub listing partially failed") ||
		strings.Contains(logs, "level=ERROR") {
		t.Errorf("Collect with a cancelled wildcard listing logged an outage; logs:\n%s", logs)
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
	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
	collection := c.Collect(ctx, []registry.RepoRef{{Owner: "owner", Repo: "myapp"}})
	entries := collection.Entries
	fetched := collection.Fetched
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if fetched != 0 {
		t.Errorf("fetched = %d, want 0 (the cancelled fetch yielded no entry)", fetched)
	}
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
		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
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
		c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
		c.Collect(t.Context(), wildcard)

		if strings.Contains(buf.String(), whollyMsg) || strings.Contains(buf.String(), partiallyMsg) {
			t.Errorf("Collect with a successful wildcard listing logged a failure warn, want silence; logs:\n%s", buf.String())
		}
	})
}

func TestClient_Collect_EmptyWildcardLogsWarn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []any{}, "next": ""})
	})
	srv := httptest.NewTestServer(t, mux)

	logger, buf := captureLogger()
	c := dockerhub.NewClient(srv.Client(), dockerhub.Options{Logger: logger})
	collection := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})
	entries := collection.Entries
	attempted := collection.Attempted
	listingFailed := collection.ListingFailed

	if listingFailed || len(entries) != 0 || attempted != 0 {
		t.Errorf("Collect() empty wildcard = (%d entries, attempted=%d, listingFailed=%v), want (0, 0, false)", len(entries), attempted, listingFailed)
	}
	logs := buf.String()
	if !strings.Contains(logs, "level=WARN") ||
		!strings.Contains(logs, `msg="docker hub wildcard expanded no repos"`) ||
		!strings.Contains(logs, "owner=owner") || !strings.Contains(logs, "repos=0") {
		t.Errorf("Collect() empty wildcard did not emit the alert-reaching WARN record; logs:\n%s", logs)
	}
}
