package dockerhub

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/internal/registry"
)

func TestParseRepoMeta_LastUpdatedIsOptional(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		want      time.Time
		wantShape bool
	}{
		{name: "absent", data: `{"pull_count":1}`},
		{name: "null", data: `{"pull_count":1,"last_updated":null}`},
		{
			name: "rfc3339 with fraction", data: `{"pull_count":1,"last_updated":"2026-10-06T09:12:13.446611Z"}`,
			want: time.Date(2026, 10, 6, 9, 12, 13, 446611000, time.UTC),
		},
		{name: "wrong type", data: `{"pull_count":1,"last_updated":12}`, wantShape: true},
		{name: "not a time", data: `{"pull_count":1,"last_updated":"yesterday"}`, wantShape: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pulls, updated, err := parseRepoMeta([]byte(tt.data))
			if shapeChanged(err) != tt.wantShape {
				t.Fatalf("parseRepoMeta(%q) error = %v, want shape change %v", tt.data, err, tt.wantShape)
			}
			if tt.wantShape {
				return
			}
			if pulls != 1 || !updated.Equal(tt.want) {
				t.Errorf("parseRepoMeta(%q) = (%d, %v), want (1, %v)", tt.data, pulls, updated, tt.want)
			}
		})
	}
}

func TestParseRepoListPage_CarriesEachRowsLastUpdated(t *testing.T) {
	data := []byte(`{"count":2,"next":"","results":[` +
		`{"name":"a","pull_count":1,"last_updated":"2026-10-06T18:03:06.013342Z"},` +
		`{"name":"b","pull_count":2,"last_updated":null}]}`)
	repos, _, _, err := parseRepoListPage(data, "owner")
	if err != nil || len(repos) != 2 {
		t.Fatalf("parseRepoListPage(%s) = (%d repos, %v), want 2 repos", data, len(repos), err)
	}
	if want := time.Date(2026, 10, 6, 18, 3, 6, 13342000, time.UTC); !repos[0].Updated.Equal(want) || !repos[1].Updated.IsZero() {
		t.Errorf("parseRepoListPage updated = (%v, %v), want (%v, zero)", repos[0].Updated, repos[1].Updated, want)
	}
}

func TestParseTagCount(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		want      int64
		wantShape bool
	}{
		{name: "served page", data: `{"count":341,"next":"https://hub.docker.com/v2/repositories/o/r/tags?page=2&page_size=1","previous":null,"results":[{}]}`, want: 341},
		{name: "no tags", data: `{"count":0,"results":[]}`, want: 0},
		{name: "count missing", data: `{"results":[]}`, wantShape: true},
		{name: "count null", data: `{"count":null}`, wantShape: true},
		{name: "count negative", data: `{"count":-1}`, wantShape: true},
		{name: "not json", data: `<html>`, wantShape: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTagCount([]byte(tt.data))
			if shapeChanged(err) != tt.wantShape || (!tt.wantShape && got != tt.want) {
				t.Errorf("parseTagCount(%q) = (%d, %v), want (%d, shape change %v)", tt.data, got, err, tt.want, tt.wantShape)
			}
		})
	}
}

// TestClient_Collect_SortsEveryFetchByOutcome pins the accounting the presence
// and completeness series read: an entry and a 404 are definitive answers, and any
// other failure leaves the repository unread.
func TestClient_Collect_SortsEveryFetchByOutcome(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/repositories/o/here/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pull_count":5,"last_updated":"2026-10-06T09:12:13Z"}`))
	})
	mux.HandleFunc("GET /v2/repositories/o/gone/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /v2/repositories/o/refused/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("GET /v2/repositories/o/garbled/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pull_count":null}`))
	})
	srv := httptest.NewTestServer(t, mux)
	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})
	ref := func(repo string) registry.RepoRef { return registry.RepoRef{Owner: "o", Repo: repo} }

	got := c.Collect(t.Context(), []registry.RepoRef{ref("here"), ref("gone"), ref("refused"), ref("garbled")})

	if len(got.Entries) != 1 || got.Entries[0].Repo != "here" || got.Entries[0].Updated.IsZero() {
		t.Errorf("Collect entries = %+v, want o/here with its last_updated", got.Entries)
	}
	if !slices.Equal(got.Absent, []registry.RepoRef{ref("gone")}) {
		t.Errorf("Collect absent = %v, want [o/gone] (a 404 is a definitive absence)", got.Absent)
	}
	if !slices.Equal(got.Unread, []registry.RepoRef{ref("refused"), ref("garbled")}) {
		t.Errorf("Collect unread = %v, want [o/refused o/garbled]", got.Unread)
	}
	if got.Attempted != 4 || got.Definitive != 2 || got.Fetched != 1 {
		t.Errorf("Collect (attempted, definitive, fetched) = (%d, %d, %d), want (4, 2, 1)", got.Attempted, got.Definitive, got.Fetched)
	}
}

// TestClient_Collect_ListsOnlyOwnersReadWhole pins which wildcard owners count as
// listed: a listing read to its end. A page cap or a lost page leaves the owner out,
// so no repository is read as absent from a listing that may not hold it.
func TestClient_Collect_ListsOnlyOwnersReadWhole(t *testing.T) {
	tests := []struct {
		name  string
		pages func(page string) (int, any)
		want  bool
	}{
		{"one complete page", func(string) (int, any) {
			return http.StatusOK, map[string]any{"count": 1, "next": "", "results": []map[string]any{{"name": "a", "pull_count": 1}}}
		}, true},
		{"page cap reached", func(page string) (int, any) {
			return http.StatusOK, map[string]any{"count": 1000, "next": "more", "results": []map[string]any{{"name": "a" + page, "pull_count": 1}}}
		}, false},
		{"second page lost", func(page string) (int, any) {
			if page == "2" {
				return http.StatusForbidden, nil
			}
			return http.StatusOK, map[string]any{"count": 2, "next": "more", "results": []map[string]any{{"name": "a", "pull_count": 1}}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status, body := tt.pages(r.URL.Query().Get("page"))
				w.WriteHeader(status)
				if body != nil {
					_ = json.NewEncoder(w).Encode(body)
				}
			}))
			c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})

			got := c.Collect(t.Context(), []registry.RepoRef{{Owner: "o", Repo: "*"}})

			if listed := slices.Contains(got.Listed, "o"); listed != tt.want {
				t.Errorf("Collect(%s) listed owner = %v, want %v", tt.name, listed, tt.want)
			}
		})
	}
}

// TestClient_PacesEveryRequestAcrossCollectAndTagReads pins the one request budget:
// listing pages, metadata fetches and tag reads all start at least DefaultPacing
// apart, so a large explicit list no longer bursts past the anonymous rate limit.
func TestClient_PacesEveryRequestAcrossCollectAndTagReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			at = append(at, time.Now())
			switch {
			case strings.HasSuffix(r.URL.Path, "/tags"):
				_, _ = w.Write([]byte(`{"count":3}`))
			case r.URL.Path == "/v2/repositories/o/":
				_, _ = w.Write([]byte(`{"count":1,"next":"","results":[{"name":"listed","pull_count":1}]}`))
			default:
				_, _ = w.Write([]byte(`{"pull_count":2}`))
			}
		}))
		c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler), Pacing: DefaultPacing})

		c.Collect(t.Context(), []registry.RepoRef{{Owner: "o", Repo: "*"}, {Owner: "o", Repo: "x"}, {Owner: "o", Repo: "y"}})
		for _, repo := range []string{"x", "y"} {
			if _, err := c.ReadDetail(t.Context(), registry.RepoRef{Owner: "o", Repo: repo}); err != nil {
				t.Fatalf("ReadDetail(o/%s) error = %v", repo, err)
			}
		}

		if len(at) != 5 {
			t.Fatalf("server saw %d requests, want 5 (listing, two fetches, two tag reads)", len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < DefaultPacing {
				t.Errorf("request %d started %v after request %d, want at least %v", i, gap, i-1, DefaultPacing)
			}
		}
	})
}

func TestClient_ReadDetail_ReadsTheTagCount(t *testing.T) {
	var asked string
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"count":341,"results":[{}]}`))
	}))
	c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler)})

	got, err := c.ReadDetail(t.Context(), registry.RepoRef{Owner: "o", Repo: "app"})

	if err != nil || got != (registry.Detail{Tagged: 341}) {
		t.Errorf("ReadDetail(o/app) = (%+v, %v), want ({341 0}, nil)", got, err)
	}
	if asked != "/v2/repositories/o/app/tags?page_size=1" {
		t.Errorf("ReadDetail(o/app) asked %q, want the one-row tag page", asked)
	}
}

func TestClient_ReadDetail_FailuresAreLoggedByCause(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		limited bool
	}{
		{"shape change", http.StatusOK, `{"count":null}`, `level=ERROR msg="docker hub tag read failed" repo=o/app`, false},
		{"rate limit", http.StatusTooManyRequests, "", `level=WARN msg="docker hub tag read failed" repo=o/app`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var buf bytes.Buffer
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				}))
				c := NewClient(srv.Client(), Options{Logger: slog.New(slog.NewTextHandler(&buf, nil))})

				_, err := c.ReadDetail(t.Context(), registry.RepoRef{Owner: "o", Repo: "app"})

				if err == nil {
					t.Fatalf("ReadDetail(%s) error = nil, want an error", tt.name)
				}
				if got := errors.Is(err, httpx.ErrRateLimited); got != tt.limited {
					t.Errorf("ReadDetail(%s) errors.Is(err, ErrRateLimited) = %v, want %v", tt.name, got, tt.limited)
				}
				if !strings.Contains(buf.String(), tt.want) {
					t.Errorf("ReadDetail(%s) logs lack %q:\n%s", tt.name, tt.want, buf.String())
				}
			})
		})
	}
}

// TestClient_PacesRetriesAndRedirectHops pins pacing at the request a server
// sees: a retried attempt, the request after it, and a redirect hop each start
// at least DefaultPacing after the previous one.
func TestClient_PacesRetriesAndRedirectHops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			at = append(at, time.Now())
			switch {
			case len(at) == 1:
				w.WriteHeader(http.StatusInternalServerError)
			case r.URL.Path == "/v2/repositories/o/app/tags":
				http.Redirect(w, r, "/v2/repositories/o/moved/tags", http.StatusFound)
			case r.URL.Path == "/v2/repositories/o/moved/tags":
				_, _ = w.Write([]byte(`{"count":3}`))
			default:
				_, _ = w.Write([]byte(`{"pull_count":2}`))
			}
		}))
		c := NewClient(srv.Client(), Options{Logger: slog.New(slog.DiscardHandler), Pacing: DefaultPacing})
		ref := registry.RepoRef{Owner: "o", Repo: "app"}

		if got := c.Collect(t.Context(), []registry.RepoRef{ref}); len(got.Entries) != 1 {
			t.Fatalf("Collect(o/app) entries = %d, want 1 after one retry", len(got.Entries))
		}
		if _, err := c.ReadDetail(t.Context(), ref); err != nil {
			t.Fatalf("ReadDetail(o/app) error = %v", err)
		}

		if len(at) != 4 {
			t.Fatalf("server saw %d requests, want 4 (failed attempt, retry, tag read, redirect hop)", len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < DefaultPacing {
				t.Errorf("request %d started %v after request %d, want at least %v", i, gap, i-1, DefaultPacing)
			}
		}
	})
}
