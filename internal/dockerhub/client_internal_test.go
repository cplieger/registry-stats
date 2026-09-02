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
)

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
}

// TestParseTagCount: a present, non-negative count is returned; malformed
// JSON, a missing count, and a negative count are all errors so a
// reshaped response can never flow a bogus value into the image_tags
// gauge.
func TestParseTagCount(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		want      int
		wantErr   bool
		wantShape bool
	}{
		{"real shape", `{"count":164,"next":"p2","results":[{"name":"latest"}]}`, 164, false, false},
		{"zero tags", `{"count":0,"next":"","results":[]}`, 0, false, false},
		{"missing count", `{"next":"","results":[]}`, 0, true, true},
		{"negative count", `{"count":-1}`, 0, true, true},
		{"duplicate count", `{"count":1,"count":2}`, 0, true, true},
		{"malformed json", `not json`, 0, true, true},
		{"empty input", ``, 0, true, true},
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
			if gotShape := shapeChanged(err); gotShape != tt.wantShape {
				t.Errorf("shapeChanged(parseTagCount(%q)) = %v, want %v", tt.data, gotShape, tt.wantShape)
			}
		})
	}
}

func TestClient_ListRepos_ExactPageCount(t *testing.T) {
	pageRequests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		pageRequests++
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			json.NewEncoder(w).Encode(map[string]any{
				"count":   2,
				"results": []map[string]any{{"name": "a1", "pull_count": 10}},
				"next":    "page2",
			})
		case "2":
			json.NewEncoder(w).Encode(map[string]any{
				"count":   2,
				"results": []map[string]any{{"name": "a2", "pull_count": 20}},
				"next":    "",
			})
		}
	})
	srv := httptest.NewTestServer(t, mux)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: logger})
	repos, err := c.listRepos(t.Context(), "o")
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
	if logs := buf.String(); strings.Contains(logs, "hit page cap") {
		t.Errorf("listRepos() logged a page-cap warning after normal pagination; logs:\n%s", logs)
	}
}

// TestClient_NilLogger_DoesNotPanic verifies a Client built with a nil
// logger falls back to a usable default.
func TestClient_NilLogger_DoesNotPanic(t *testing.T) {
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry()})
	if got := c.tagCount(t.Context(), "o", "a"); got != nil {
		t.Errorf("tagCount on a failing server = %v, want nil", got)
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

func TestClient_OwnerListing_TruncatesAtPageCap(t *testing.T) {
	// Filter by exact URL: the mux pattern /v2/repositories/o/ would
	// otherwise also match nested paths like /v2/repositories/o/a/tags/.
	ownerPages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/repositories/o/" {
			ownerPages++
			page := r.URL.Query().Get("page")
			json.NewEncoder(w).Encode(map[string]any{
				"count":   1000,
				"results": []map[string]any{{"name": "a" + page, "pull_count": 1}},
				"next":    "keep-going",
			})
			return
		}
		tagCountHandler(1)(w, r)
	})
	srv := httptest.NewTestServer(t, mux)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: logger})
	refs := []registry.RepoRef{{Owner: "o", Repo: "*"}}
	_, attempted, _ := c.Collect(t.Context(), refs)

	if ownerPages != 10 {
		t.Errorf("owner-listing requests = %d, want 10 (update if maxOwnerPages changes)", ownerPages)
	}
	if attempted != 10 {
		t.Errorf("attempted = %d, want 10 distinct repos (update if maxOwnerPages changes)", attempted)
	}
	const message = "docker hub owner listing hit page cap; results may be truncated"
	logs := buf.String()
	if !strings.Contains(logs, `msg="`+message+`"`) ||
		!strings.Contains(logs, "owner=o") || !strings.Contains(logs, "max_pages=10") {
		t.Errorf("Collect at the page cap did not log %q with owner=o and max_pages=10; logs:\n%s", message, logs)
	}
}

// TestParseRepoMeta pins the required-field contract on the cumulative pull
// count: a present, non-negative pull_count is returned, and absent, null or
// negative is an ERROR rather than 0 — 0 is a legitimate pull count and
// image_pulls_total is cumulative, so a silent 0 reads downstream as a
// regression.
//
// The duplicate-key row is the witness for encoding/json/v2: v1 silently
// kept the LAST value for a repeated member, so a reshaped or tampered
// response could pick which number reached the gauge; v2 rejects it.
func TestParseRepoMeta(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		want      int64
		wantErr   bool
		wantShape bool
	}{
		{"real shape", `{"pull_count":5000,"last_updated":"2026-03-06T12:00:00Z"}`, 5000, false, false},
		{"zero pulls is a real value", `{"pull_count":0}`, 0, false, false},
		{"large count", `{"pull_count":9999999999}`, 9999999999, false, false},
		{"missing pull_count", `{}`, 0, true, true},
		{"case-variant pull_count", `{"Pull_Count":7}`, 0, true, true},
		{"null pull_count", `{"pull_count":null}`, 0, true, true},
		{"negative pull_count", `{"pull_count":-1}`, 0, true, true},
		{"duplicate pull_count", `{"pull_count":1,"pull_count":2}`, 0, true, true},
		{"malformed json", `not json`, 0, true, true},
		{"empty input", ``, 0, true, true},
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
			if gotShape := shapeChanged(err); gotShape != tt.wantShape {
				t.Errorf("shapeChanged(parseRepoMeta(%q)) = %v, want %v", tt.data, gotShape, tt.wantShape)
			}
		})
	}
}

// TestParseRepoListPage_requiresPullCount pins that a listing result with no
// usable pull_count fails the whole PAGE instead of being dropped, and that a
// page whose every result is dropped as unsafe fails too. Dropping would hand
// listRepos zero repos with a nil error, which reads as a legitimately empty
// owner — wiping every wildcard series on a Docker Hub schema change while
// the cycle still reported healthy.
func TestParseRepoListPage_requiresPullCount(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantErr   bool
		wantShape bool
	}{
		{"every result carries a count", `{"count":2,"next":"","results":[{"name":"a","pull_count":1},{"name":"b","pull_count":0}]}`, false, false},
		{"empty page is fine", `{"count":0,"next":"","results":[]}`, false, false},
		{"null results is empty", `{"count":0,"next":"","results":null}`, false, false},
		{"missing results is empty", `{"count":0,"next":""}`, false, false},
		{"unknown member is skipped", `{"count":0,"unknown":{"nested":true},"results":[]}`, false, false},
		{"case-variant count is ignored", `{"Count":0,"results":[]}`, true, true},
		{"missing total fails the page", `{"next":"","results":[]}`, true, true},
		{"trailing data fails the page", `{"count":0,"results":[]} true`, true, true},
		{"duplicate results member", `{"results":[],"results":[]}`, true, true},
		{"one result missing the count fails the page", `{"next":"","results":[{"name":"a","pull_count":1},{"name":"b"}]}`, true, true},
		{"null count fails the page", `{"results":[{"name":"a","pull_count":null}]}`, true, true},
		{"negative count fails the page", `{"results":[{"name":"a","pull_count":-5}]}`, true, true},
		{"an unsafe name is dropped before its count is required", `{"count":2,"results":[{"name":"bad/traversal"},{"name":"a","pull_count":1}]}`, false, false},
		{"a page of nothing but unsafe names fails", `{"results":[{"name":"bad/traversal"},{"name":"../evil"}]}`, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos, _, _, err := parseRepoListPage([]byte(tt.data), "owner")
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRepoListPage(%q) error = %v, wantErr %v", tt.data, err, tt.wantErr)
			}
			if tt.wantErr && len(repos) != 0 {
				t.Errorf("parseRepoListPage(%q) returned %d repos alongside an error, want none", tt.data, len(repos))
			}
			if gotShape := shapeChanged(err); gotShape != tt.wantShape {
				t.Errorf("shapeChanged(parseRepoListPage(%q)) = %v, want %v", tt.data, gotShape, tt.wantShape)
			}
		})
	}
}

func TestParseRepoListPage_allowsModestPageWidening(t *testing.T) {
	results := make([]map[string]any, 0, pageSize+1)
	for i := range pageSize + 1 {
		results = append(results, map[string]any{"name": "r" + strconv.Itoa(i), "pull_count": 1})
	}
	data, err := json.Marshal(map[string]any{"count": len(results), "next": "", "results": results})
	if err != nil {
		t.Fatalf("Setup: marshal listing page: %v", err)
	}

	repos, more, _, err := parseRepoListPage(data, "owner")
	if err != nil {
		t.Fatalf("parseRepoListPage(%d results) error = %v, want nil", len(results), err)
	}
	if len(repos) != len(results) || more {
		t.Errorf("parseRepoListPage(%d results) = (%d repos, more=%v), want (%d, false)", len(results), len(repos), more, len(results))
	}
}

func TestParseRepoListPage_refusesPastElementCap(t *testing.T) {
	results := make([]map[string]any, 0, listingElemCap+1)
	for i := range listingElemCap + 1 {
		results = append(results, map[string]any{"name": "r" + strconv.Itoa(i), "pull_count": 1})
	}
	data, err := json.Marshal(map[string]any{"count": len(results), "next": "", "results": results})
	if err != nil {
		t.Fatalf("Setup: marshal listing page: %v", err)
	}

	repos, more, _, err := parseRepoListPage(data, "owner")
	if err == nil {
		t.Fatalf("parseRepoListPage(%d results) error = nil, want a refusal", len(results))
	}
	if len(repos) != 0 || more {
		t.Errorf("parseRepoListPage(%d results) = (%d repos, more=%v), want (0, false)", len(results), len(repos), more)
	}
	if !shapeChanged(err) {
		t.Errorf("shapeChanged(parseRepoListPage(%d results)) = false, want true", len(results))
	}
}

// TestParseRepoListPage_dropsUnsafeName asserts the security guard: a
// listing name carrying URL metacharacters (a slash here) is a
// path/query-injection vector into the tags URL built from it, so it
// must be dropped while safe names on the same page survive.
func TestParseRepoListPage_dropsUnsafeName(t *testing.T) {
	data := []byte(`{"count":2,"next":"","results":[{"name":"bad/traversal","pull_count":1},{"name":"good","pull_count":2}]}`)
	repos, _, _, err := parseRepoListPage(data, "owner")
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
