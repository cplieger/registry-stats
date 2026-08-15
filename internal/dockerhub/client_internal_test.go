package dockerhub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
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
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
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
					{"name": "app1", "pull_count": 100},
					{"name": "app2", "pull_count": 200},
				},
				"next": "page2",
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{
					{"name": "app3", "pull_count": 50},
				},
				"next": "",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	repos, err := listRepos(t.Context(), c, "testowner")
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 3 {
		t.Fatalf("repos len = %d, want 3", len(repos))
	}
	if pageRequests != 2 {
		t.Errorf("page requests = %d, want 2", pageRequests)
	}
	if repos[2].Owner != "testowner" || repos[2].Repo != "app3" {
		t.Errorf("repos[2] = %+v, want testowner/app3", repos[2])
	}
}

func TestClient_ListRepos_ParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	_, err := listRepos(t.Context(), c, "testowner")
	if err == nil {
		t.Error("expected parse error for invalid JSON")
	}
}

// TestClient_TagCount_ReadsCountField pins the aggregate-count contract:
// tagCount reads the tags listing's top-level "count" — the registry's
// exact total — never the length of the results page, and it must issue
// exactly ONE request per repo regardless of the "next" page token.
func TestClient_TagCount_ReadsCountField(t *testing.T) {
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/owner/app/tags/", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		// count deliberately differs from len(results), and next offers
		// more pages: only the count field may drive the result.
		json.NewEncoder(w).Encode(map[string]any{
			"count":   164,
			"next":    "page2",
			"results": []map[string]any{{"name": "latest"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	got := tagCount(t.Context(), c, "owner", "app")
	if got != 164 {
		t.Errorf("tagCount = %d, want 164 (the count field, not len(results))", got)
	}
	if requests != 1 {
		t.Errorf("tags requests = %d, want exactly 1", requests)
	}
}

func TestClient_TagCount_FetchErrorReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	if got := tagCount(t.Context(), c, "owner", "app"); got != 0 {
		t.Errorf("tagCount = %d, want 0 on fetch error", got)
	}
}

func TestClient_TagCount_ParseErrorReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	if got := tagCount(t.Context(), c, "owner", "app"); got != 0 {
		t.Errorf("tagCount = %d, want 0 on parse error", got)
	}
}

func TestClient_TagCount_MissingCountReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"next":"","results":[{"name":"latest"}]}`))
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, testsupport.QuietLogger())
	if got := tagCount(t.Context(), c, "owner", "app"); got != 0 {
		t.Errorf("tagCount = %d, want 0 when the response carries no count field", got)
	}
}

// TestParseTagCount unit-tests the pure parse core: a present,
// non-negative count is returned; malformed JSON, a missing count, and a
// negative count are all errors so a reshaped response can never flow a
// bogus value into the image_tags gauge.
func TestParseTagCount(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    int
		wantErr bool
	}{
		{"real shape", `{"count":164,"next":"p2","results":[{"name":"latest"}]}`, 164, false},
		{"zero tags", `{"count":0,"next":"","results":[]}`, 0, false},
		{"count differs from results length", `{"count":7,"results":[{"name":"a"}]}`, 7, false},
		{"missing count", `{"next":"","results":[]}`, 0, true},
		{"negative count", `{"count":-1}`, 0, true},
		{"malformed json", `not json`, 0, true},
		{"empty input", ``, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTagCount([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseTagCount(%q) error = %v, wantErr %v", tt.data, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseTagCount(%q) = %d, want %d", tt.data, got, tt.want)
			}
		})
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
	repos, err := listRepos(t.Context(), c, "o")
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("listRepos() = %d repos, want 2", len(repos))
	}
	if repos[0].Owner != "o" || repos[0].Repo != "a1" || repos[0].Pulls != 10 {
		t.Errorf("repos[0] = %+v, want o/a1 with 10 pulls", repos[0])
	}
	if repos[1].Owner != "o" || repos[1].Repo != "a2" || repos[1].Pulls != 20 {
		t.Errorf("repos[1] = %+v, want o/a2 with 20 pulls", repos[1])
	}
	if pageRequests != 2 {
		t.Errorf("page requests = %d, want 2", pageRequests)
	}
}

// TestClient_NilLogger_DoesNotPanic verifies a Client built with a nil
// logger falls back to a usable default: a path that logs must not
// nil-panic. tagCount against a failing server hits the warn log path,
// so a missing fallback would crash here.
func TestClient_NilLogger_DoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(mockClient(srv), shortRetry(), 0, nil)
	if got := tagCount(t.Context(), c, "o", "a"); got != 0 {
		t.Errorf("tagCount on a failing server = %d, want 0", got)
	}
}
