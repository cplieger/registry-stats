package dockerhub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/testsupport"
)

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
}

func TestClient_ListRepos_ParseError(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))

	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	_, err := listRepos(t.Context(), c, "testowner")
	if err == nil {
		t.Error("expected parse error for invalid JSON")
	}
}

func TestClient_TagCount_ParseErrorReturnsZero(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))

	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
	if got := tagCount(t.Context(), c, "owner", "app"); got != 0 {
		t.Errorf("tagCount = %d, want 0 on parse error", got)
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
			got, err := parseTagCount([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTagCount(%q) error = %v, wantErr %v", tt.data, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseTagCount(%q) = %d, want %d", tt.data, got, tt.want)
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
	srv := httptest.NewTestServer(t, mux)

	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: testsupport.QuietLogger()})
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
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry()})
	if got := tagCount(t.Context(), c, "o", "a"); got != 0 {
		t.Errorf("tagCount on a failing server = %d, want 0", got)
	}
}

func tagCountHandler(count int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"count":   count,
			"next":    "",
			"results": []map[string]any{{"name": "latest"}},
		})
	}
}

// TestClient_PageCap_TruncatesOwnerListing lives in the internal test file
// because pageCap is the unexported test seam (go.md forbids a test-only
// parameter on the production constructor, which is where the cap used to
// ride). It also pins the truncation WARN, which alerts.yaml matches on
// `hit page cap`: the bound and the signal that it bit are one behaviour.
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
		// Tags endpoint under the same prefix: serve a count so the
		// wildcard expansion's tag-count fetch for "a" succeeds quietly.
		tagCountHandler(1)(w, r)
	})
	srv := httptest.NewTestServer(t, mux)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: logger})
	c.pageCap = 1 // in-package: the cap needs no production setter
	refs := []registry.RepoRef{{Owner: "o", Repo: "*"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if ownerPages != 1 {
		t.Errorf("owner-listing requests = %d, want 1 (pageCap=1 enforced)", ownerPages)
	}
	if attempted != 1 {
		t.Errorf("attempted = %d, want 1 (one repo from the capped page)", attempted)
	}
	const message = "docker hub owner listing hit page cap; results may be truncated"
	logs := buf.String()
	if !strings.Contains(logs, `msg="`+message+`"`) ||
		!strings.Contains(logs, "owner=o") || !strings.Contains(logs, "max_pages=1") {
		t.Errorf("Collect at the page cap did not log %q with owner=o and max_pages=1; logs:\n%s", message, logs)
	}
}

func TestDegraded(t *testing.T) {
	tests := []struct {
		name      string
		results   []registry.Entry
		attempted int
		want      bool
	}{
		{"zero attempted", nil, 0, false},
		{"all failed (0 of 3)", nil, 3, true},
		{"empty results (0 of 1)", []registry.Entry{}, 1, true},
		{"majority failed (1 of 3)", []registry.Entry{{Repo: "b"}}, 3, true},
		{"exactly half (1 of 2)", []registry.Entry{{Repo: "b"}}, 2, false},
		{"all succeeded (2 of 2)", []registry.Entry{{Repo: "b"}, {Repo: "d"}}, 2, false},
		{"one of one", []registry.Entry{{Repo: "b"}}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := degraded(tt.results, tt.attempted)
			if got != tt.want {
				t.Errorf("degraded(len=%d, attempted=%d) = %v, want %v",
					len(tt.results), tt.attempted, got, tt.want)
			}
		})
	}
}

// TestParseRepoMeta pins the required-field contract on the cumulative pull
// count: a present, non-negative pull_count is returned, and absent, null or
// negative is an ERROR rather than 0. The distinction matters because 0 is a
// legitimate pull count and image_pulls_total is cumulative — a silent 0 for a
// repo that has pulls reads downstream as a regression, not as missing data.
//
// The duplicate-key row is the witness for the encoding/json/v2 adoption: v1
// silently kept the LAST value for a repeated member, so a reshaped or
// tampered response could pick which number reached the gauge. v2 rejects it,
// which routes it into the existing "docker hub parse failed" ERROR and skips
// the repo for the cycle.
func TestParseRepoMeta(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    int64
		wantErr bool
	}{
		{"real shape", `{"pull_count":5000,"last_updated":"2026-03-06T12:00:00Z"}`, 5000, false},
		{"zero pulls is a real value", `{"pull_count":0}`, 0, false},
		{"large count", `{"pull_count":9999999999}`, 9999999999, false},
		{"missing pull_count", `{}`, 0, true},
		{"null pull_count", `{"pull_count":null}`, 0, true},
		{"negative pull_count", `{"pull_count":-1}`, 0, true},
		{"duplicate pull_count", `{"pull_count":1,"pull_count":2}`, 0, true},
		{"malformed json", `not json`, 0, true},
		{"empty input", ``, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRepoMeta([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRepoMeta(%q) error = %v, wantErr %v", tt.data, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseRepoMeta(%q) = %d, want %d", tt.data, got, tt.want)
			}
		})
	}
}

// TestParseRepoListPage_requiresPullCount pins that a listing result with no
// usable pull_count fails the whole PAGE instead of being dropped, and that a
// page whose every result is dropped as unsafe fails too. Dropping would hand
// listRepos zero repos with a nil error, which collectWildcardRef reads as a
// legitimately empty owner — so a Docker Hub schema change would wipe every
// wildcard series while the cycle still reported healthy. An error routes it
// into the "listing wholly failed" WARN and an unhealthy verdict.
func TestParseRepoListPage_requiresPullCount(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"every result carries a count", `{"next":"","results":[{"name":"a","pull_count":1},{"name":"b","pull_count":0}]}`, false},
		{"empty page is fine", `{"next":"","results":[]}`, false},
		{"one result missing the count fails the page", `{"next":"","results":[{"name":"a","pull_count":1},{"name":"b"}]}`, true},
		{"null count fails the page", `{"results":[{"name":"a","pull_count":null}]}`, true},
		{"negative count fails the page", `{"results":[{"name":"a","pull_count":-5}]}`, true},
		{"an unsafe name is dropped before its count is required", `{"results":[{"name":"bad/traversal"},{"name":"a","pull_count":1}]}`, false},
		{"a page of nothing but unsafe names fails", `{"results":[{"name":"bad/traversal"},{"name":"../evil"}]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, _, err := parseRepoListPage([]byte(tt.data), "owner")
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRepoListPage(%q) error = %v, wantErr %v", tt.data, err, tt.wantErr)
			}
			if tt.wantErr && len(repos) != 0 {
				t.Errorf("parseRepoListPage(%q) returned %d repos alongside an error, want none", tt.data, len(repos))
			}
		})
	}
}

// TestParseRepoListPage_refusesMoreResultsThanRequested pins the element-count
// bound: every request asks for pageSize results, so a page carrying more is a
// response the app did not ask for, and each accepted element costs one further
// outbound tag-count request. The page is refused before any entry is built.
func TestParseRepoListPage_refusesMoreResultsThanRequested(t *testing.T) {
	results := make([]map[string]any, 0, pageSize+1)
	for i := range pageSize + 1 {
		results = append(results, map[string]any{"name": "r" + strconv.Itoa(i), "pull_count": 1})
	}
	data, err := json.Marshal(map[string]any{"next": "", "results": results})
	if err != nil {
		t.Fatalf("Setup: marshal listing page: %v", err)
	}

	repos, more, err := parseRepoListPage(data, "owner")
	if err == nil {
		t.Fatalf("parseRepoListPage(%d results) error = nil, want a refusal", len(results))
	}
	if len(repos) != 0 || more {
		t.Errorf("parseRepoListPage(%d results) = (%d repos, more=%v), want (0, false)", len(results), len(repos), more)
	}
}

// TestParseRepoListPage_dropsUnsafeName asserts the parseRepoListPage
// security guard: a listing name carrying URL metacharacters (a slash
// here) is a path/query-injection vector into the tags URL built from it
// in tagCount, so it must be dropped while safe names on the same page
// survive. A removed guard cannot be caught by FuzzDockerHubRepoListUnmarshal's
// owner invariant (an unsafe name kept under the right owner still
// satisfies it), so this direct assertion is the only thing that pins the drop.
func TestParseRepoListPage_dropsUnsafeName(t *testing.T) {
	data := []byte(`{"next":"","results":[{"name":"bad/traversal","pull_count":1},{"name":"good","pull_count":2}]}`)
	repos, _, err := parseRepoListPage(data, "owner")
	if err != nil {
		t.Fatalf("parseRepoListPage(%q) error = %v", data, err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos len = %d, want 1 (unsafe name dropped, safe survives)", len(repos))
	}
	if repos[0].Owner != "owner" || repos[0].Repo != "good" || repos[0].Pulls != 2 {
		t.Errorf("repos[0] = %+v, want owner/good with 2 pulls", repos[0])
	}
}
