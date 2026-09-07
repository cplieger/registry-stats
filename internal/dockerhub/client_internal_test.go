package dockerhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/slogx/capture"
)

// shortRetry returns httpx options with a 1 ms base delay so retry tests
// don't wait a full second between attempts.
func shortRetry() []httpx.GetOption {
	return []httpx.GetOption{httpx.WithBaseDelay(time.Millisecond)}
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
	repos, advertised, err := c.listRepos(t.Context(), "o")
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("listRepos() = %d repos, want 2", len(repos))
	}
	if advertised != 2 {
		t.Errorf("listRepos() advertised = %d, want 2", advertised)
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

func TestClient_ListRepos_ReportsMidWalkPopulationChange(t *testing.T) {
	page1 := make([]map[string]any, 0, pageSize)
	for i := range pageSize {
		page1 = append(page1, map[string]any{"name": "r" + strconv.Itoa(i), "pull_count": 1})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 100, "results": page1, "next": "page2"})
		case "2":
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 99, "results": []map[string]any{}, "next": ""})
		}
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: slog.Default()})

	repos, advertised, err := c.listRepos(t.Context(), "o")
	if !shapeChanged(err) {
		t.Errorf("listRepos(total changed mid-walk) error = %v, want a shape-change error", err)
	}
	if len(repos) != 99 || advertised != 100 {
		t.Errorf("listRepos(total changed mid-walk) = (%d repos, advertised %d), want (99, 100)", len(repos), advertised)
	}
}

func TestClient_ListRepos_SecondPageStaysBelowAnonymousOffsetLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if (page-1)*size >= 100 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"pagination offset too large for anonymous requests"}`))
			return
		}
		if page == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 2, "results": []map[string]any{{"name": "a", "pull_count": 1}}, "next": "page2"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 2, "results": []map[string]any{{"name": "b", "pull_count": 2}}, "next": ""})
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: slog.Default()})

	repos, advertised, err := c.listRepos(t.Context(), "o")
	if err != nil {
		t.Fatalf("listRepos(two-page anonymous owner): %v", err)
	}
	if len(repos) != 2 || advertised != 2 {
		t.Errorf("listRepos(two-page anonymous owner) = (%d repos, advertised %d), want (2, 2)", len(repos), advertised)
	}
}

func TestClient_OwnerListing_TruncatesAtPageCap(t *testing.T) {
	// Count only exact owner-listing requests toward the page cap.
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
	})
	srv := httptest.NewTestServer(t, mux)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: logger})
	refs := []registry.RepoRef{{Owner: "o", Repo: "*"}}
	collection := c.Collect(t.Context(), refs)

	if ownerPages != 2 {
		t.Errorf("owner-listing requests = %d, want 2 (update if maxOwnerPages changes)", ownerPages)
	}
	if collection.Attempted != 0 {
		t.Errorf("attempted = %d, want 0 (listing rows are not fetches)", collection.Attempted)
	}
	logs := buf.String()
	// RegistryStatsCollectionIncomplete in alerts/logql.yaml consumes this literal.
	if !strings.Contains(logs, `level=WARN msg="docker hub owner listing hit page cap; results may be truncated"`) {
		t.Errorf("Collect stopping at the page cap did not emit the alert-keyed WARN; logs:\n%s", logs)
	}
	if !strings.Contains(logs, `msg="docker hub wildcard expanded"`) ||
		!strings.Contains(logs, "owner=o") || !strings.Contains(logs, "repos=2") ||
		!strings.Contains(logs, "advertised=1000") {
		t.Errorf("Collect stopping at the page cap did not report 2 of 1000 collected; logs:\n%s", logs)
	}
}

func TestClient_ListRepos_RejectsMoreReposThanAdvertised(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 1,
			"results": []map[string]any{
				{"name": "a", "pull_count": 1},
				{"name": "b", "pull_count": 2},
			},
			"next": "",
		})
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), Options{RetryOpts: shortRetry(), Logger: slog.Default()})

	_, _, err := c.listRepos(t.Context(), "o")

	if !shapeChanged(err) {
		t.Errorf("listRepos(count 1 with 2 repos) error = %v, want a shape-change error", err)
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

// TestParseRepoListPage_dropsUnsafeName asserts the repository-name guard: a
// listing name carrying URL metacharacters does not satisfy the allowlist for
// published repo labels, so it must be dropped while safe names on the same
// page survive.
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestClient_LogErrorsAreSanitizedAndBounded(t *testing.T) {
	hostile := "\n\x00\u202e" + string([]byte{0xff}) + strings.Repeat("x", 300)
	retryOpts := []httpx.GetOption{httpx.WithMaxAttempts(1)}
	wildcard := []registry.RepoRef{{Owner: "owner", Repo: "*"}}
	explicit := []registry.RepoRef{{Owner: "owner", Repo: "repo"}}

	tests := []struct {
		name string
		msg  string
		run  func(*testing.T, *slog.Logger)
	}{
		{
			name: "wholly_failed_listing",
			msg:  "docker hub listing wholly failed",
			run: func(t *testing.T, logger *slog.Logger) {
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New(hostile)
				})}
				NewClient(client, Options{Logger: logger, RetryOpts: retryOpts}).Collect(t.Context(), wildcard)
			},
		},
		{
			name: "partially_failed_listing",
			msg:  "docker hub listing partially failed",
			run: func(t *testing.T, logger *slog.Logger) {
				requests := 0
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					if requests == 1 {
						body := `{"count":1,"results":[{"name":"repo","pull_count":1}],"next":"page2"}`
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					}
					return nil, errors.New(hostile)
				})}
				NewClient(client, Options{Logger: logger, RetryOpts: retryOpts}).Collect(t.Context(), wildcard)
			},
		},
		{
			name: "cancelled_fetch",
			msg:  "docker hub fetch cancelled",
			run: func(t *testing.T, logger *slog.Logger) {
				ctx, cancel := context.WithCancel(t.Context())
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					cancel()
					return nil, errors.New(hostile)
				})}
				NewClient(client, Options{Logger: logger, RetryOpts: retryOpts}).Collect(ctx, explicit)
			},
		},
		{
			name: "failed_fetch",
			msg:  "docker hub fetch failed",
			run: func(t *testing.T, logger *slog.Logger) {
				client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New(hostile)
				})}
				NewClient(client, Options{Logger: logger, RetryOpts: retryOpts}).Collect(t.Context(), explicit)
			},
		},
		{
			name: "failed_parse",
			msg:  "docker hub parse failed",
			run: func(t *testing.T, logger *slog.Logger) {
				key := strings.Repeat("x", 400)
				body := `{"` + key + `":1,"` + key + `":2}`
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				NewClient(client, Options{Logger: logger, RetryOpts: retryOpts}).Collect(t.Context(), explicit)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, rec := capture.New()
			tt.run(t, logger)

			got, ok := rec.AttrValueExact(tt.msg, "error")
			if !ok {
				t.Fatalf("Collect() log %q has no error attribute", tt.msg)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Collect() log %q error = %q, want valid UTF-8", tt.msg, got)
			}
			if len(got) > 259 || (len(got) > 256 && !strings.HasSuffix(got, "...")) {
				t.Errorf("Collect() log %q error length = %d, want at most 256 content bytes plus a truncation marker", tt.msg, len(got))
			}
			if strings.ContainsAny(got, "\n\r\x00") || strings.ContainsRune(got, '\u202e') {
				t.Errorf("Collect() log %q error = %q, want no planted control or bidi runes", tt.msg, got)
			}
		})
	}
}

func TestClient_Collect_InternalBodyCapOverridesCaller(t *testing.T) {
	body := `{"count":0,"results":[],"next":"","padding":"` + strings.Repeat("x", 1<<20) + `"}`
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(srv.Client(), Options{
		Logger:    logger,
		RetryOpts: []httpx.GetOption{httpx.WithMaxBodyBytes(2 << 20)},
	})
	collection := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})

	if !collection.ListingFailed {
		t.Error("Collect() listingFailed = false, want true for a body past the internal cap")
	}
	if len(collection.Entries) != 0 || collection.Attempted != 0 {
		t.Errorf("Collect() past the internal body cap = (%d entries, attempted=%d), want (0, 0)", len(collection.Entries), collection.Attempted)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("Collect() past the internal body cap did not log at ERROR; logs:\n%s", buf.String())
	}
}
