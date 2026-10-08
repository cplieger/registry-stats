package ghcr

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/internal/registry"
)

// TestClient_Collect_SortsEveryFetchByOutcome pins the accounting the presence
// and completeness series read: an entry and a 404 are definitive answers, and any
// other failure leaves the package unread.
func TestClient_Collect_SortsEveryFetchByOutcome(t *testing.T) {
	synctest.Test(t, testClientCollectSortsEveryFetchByOutcome)
}

func testClientCollectSortsEveryFetchByOutcome(t *testing.T) {
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/here"):
			_, _ = w.Write([]byte(downloadsHTML("4")))
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}), capturingLogger(&bytes.Buffer{}))
	here := registry.RepoRef{Owner: "owner", Repo: "here"}
	gone := registry.RepoRef{Owner: "owner", Repo: "gone"}
	refused := registry.RepoRef{Owner: "owner", Repo: "refused"}

	got := c.Collect(t.Context(), []registry.RepoRef{here, gone, refused})

	if len(got.Entries) != 1 || got.Entries[0].Repo != "here" {
		t.Errorf("Collect entries = %+v, want only owner/here", got.Entries)
	}
	if !slices.Equal(got.Absent, []registry.RepoRef{gone}) {
		t.Errorf("Collect absent = %v, want [owner/gone] (a 404 is a definitive absence)", got.Absent)
	}
	if !slices.Equal(got.Unread, []registry.RepoRef{refused}) {
		t.Errorf("Collect unread = %v, want [owner/refused] (a 403 answers nothing about presence)", got.Unread)
	}
	if got.Attempted != 3 || got.Definitive != 2 {
		t.Errorf("Collect (attempted, definitive) = (%d, %d), want (3, 2)", got.Attempted, got.Definitive)
	}
	if want := time.Date(2026, 9, 9, 14, 14, 12, 0, time.UTC); !got.Entries[0].LastPushed.Equal(want) {
		t.Errorf("Collect entry LastPushed = %v, want %v from the page's Last published", got.Entries[0].LastPushed, want)
	}
}

// TestClient_Collect_UnreadablePublishTimeKeepsTheCount pins the field-failure
// rule: a page whose download count reads but whose publish time does not still
// publishes the count, logs the field, and stays out of the format-error majority.
func TestClient_Collect_UnreadablePublishTimeKeepsTheCount(t *testing.T) {
	var buf bytes.Buffer
	c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<span>Total downloads</span><h3 title="9">9</h3>`))
	}), capturingLogger(&buf))

	got := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "pkg"}})

	if len(got.Entries) != 1 || got.Entries[0].Pulls != 9 || !got.Entries[0].LastPushed.IsZero() {
		t.Errorf("Collect entries = %+v, want owner/pkg with 9 pulls and no push time", got.Entries)
	}
	logs := buf.String()
	if !strings.Contains(logs, `level=WARN msg="ghcr package field unreadable" package=owner/pkg field=last_published`) {
		t.Errorf("Collect did not log the unreadable field; logs:\n%s", logs)
	}
	if strings.Contains(logs, "majority of scrapes hit format errors") {
		t.Errorf("a field failure counted toward the format-error majority; logs:\n%s", logs)
	}
}

// TestClient_Collect_ListsOnlyOwnersReadWhole pins which wildcard owners count as
// listed: a walk that ended on its own with every printed count checked. A page cap,
// an unchecked page or a refused candidate leaves the owner out, so no package is
// read as absent from a listing that may not hold it.
func TestClient_Collect_ListsOnlyOwnersReadWhole(t *testing.T) {
	tests := []struct {
		name  string
		page1 string
		pages int
		want  bool
	}{
		{"every page checked", `<span>1 package</span>` + packageLink(userOwner, "owner", "a"), 2, true},
		{"a page with no usable count", packageLink(userOwner, "owner", "a"), 2, false},
		{"a refused candidate", `<span>2 packages</span>` + packageLink(userOwner, "owner", "a") + `<a href="` + linkPrefix(userOwner, "owner") + `%%%%">x</a>`, 2, false},
		{"page cap reached", "", maxListingPages, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, inSynctest(func(t *testing.T) {
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/orgs/"):
					w.WriteHeader(http.StatusNotFound)
				case strings.Contains(r.URL.Path, "/packages/container/package/"):
					_, _ = w.Write([]byte(downloadsHTML("1")))
				case tt.page1 == "":
					// Every page adds a new name, so the walk only stops at the cap.
					page := r.URL.Query().Get("page")
					_, _ = w.Write([]byte(`<span>1 package</span>` + packageLink(userOwner, "owner", "p"+page)))
				case r.URL.Query().Get("page") == "1":
					_, _ = w.Write([]byte(tt.page1))
				default:
					_, _ = w.Write([]byte(`<span>0 packages</span>`))
				}
			}), capturingLogger(&bytes.Buffer{}))

			got := c.Collect(t.Context(), []registry.RepoRef{{Owner: "owner", Repo: "*"}})

			if listed := slices.Contains(got.Listed, "owner"); listed != tt.want {
				t.Errorf("Collect(%s) listed owner = %v, want %v", tt.name, listed, tt.want)
			}
		}))
	}
}

func TestClient_ReadDetail_ReadsTheVersionsPageThroughThePacer(t *testing.T) {
	inSynctest(func(t *testing.T) {
		var asked []string
		var at []time.Time
		c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.String())
			at = append(at, time.Now())
			if strings.Contains(r.URL.Path, "/versions") {
				_, _ = w.Write([]byte(versionsHTML("12", "340")))
				return
			}
			_, _ = w.Write([]byte(downloadsHTML("1")))
		}), capturingLogger(&bytes.Buffer{}))
		ref := registry.RepoRef{Owner: "owner", Repo: "pkg"}

		c.Collect(t.Context(), []registry.RepoRef{ref})
		got, err := c.ReadDetail(t.Context(), ref)

		if err != nil || got != (registry.Detail{Tagged: 12, Untagged: 340}) {
			t.Fatalf("ReadDetail(owner/pkg) = (%+v, %v), want ({12 340}, nil)", got, err)
		}
		if want := "/users/owner/packages/container/pkg/versions?filters%5Bversion_type%5D=tagged"; asked[1] != want {
			t.Errorf("ReadDetail requested %q, want %q", asked[1], want)
		}
		if gap := at[1].Sub(at[0]); gap < DefaultMinPacing {
			t.Errorf("ReadDetail followed the Collect request after %v, want at least %v (one pacer for both)", gap, DefaultMinPacing)
		}
	})(t)
}

func TestClient_ReadDetail_FailuresAreLoggedByCause(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		limited bool
	}{
		{"format change", http.StatusOK, versionsHTML("1000+", "1"), `level=ERROR msg="ghcr version read failed" package=owner/pkg`, false},
		{"rate limit", http.StatusTooManyRequests, "", `level=WARN msg="ghcr version read failed" package=owner/pkg`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, inSynctest(func(t *testing.T) {
			var buf bytes.Buffer
			c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}), capturingLogger(&buf))

			_, err := c.ReadDetail(t.Context(), registry.RepoRef{Owner: "owner", Repo: "pkg"})

			if err == nil {
				t.Fatalf("ReadDetail(%s) error = nil, want an error", tt.name)
			}
			if got := errors.Is(err, httpx.ErrRateLimited); got != tt.limited {
				t.Errorf("ReadDetail(%s) errors.Is(err, ErrRateLimited) = %v, want %v", tt.name, got, tt.limited)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("ReadDetail(%s) logs lack %q:\n%s", tt.name, tt.want, buf.String())
			}
		}))
	}
}

// TestClient_PacesRetriesAndRedirectHops pins pacing at the request a server
// sees: a retried attempt, the request after it, and the redirect GitHub sends
// an organization's /users/ URL each start at least DefaultMinPacing after the
// previous one.
func TestClient_PacesRetriesAndRedirectHops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var at []time.Time
		c := listingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			at = append(at, time.Now())
			switch {
			case len(at) == 1:
				w.WriteHeader(http.StatusInternalServerError)
			case strings.HasPrefix(r.URL.Path, "/users/owner/packages/container/pkg/versions"):
				http.Redirect(w, r, "/orgs/owner/packages/container/pkg/versions?"+r.URL.RawQuery, http.StatusFound)
			case strings.HasPrefix(r.URL.Path, "/orgs/"):
				_, _ = w.Write([]byte(versionsHTML("12", "340")))
			default:
				_, _ = w.Write([]byte(downloadsHTML("1")))
			}
		}), capturingLogger(&bytes.Buffer{}))
		ref := registry.RepoRef{Owner: "owner", Repo: "pkg"}

		if got := c.Collect(t.Context(), []registry.RepoRef{ref}); len(got.Entries) != 1 {
			t.Fatalf("Collect(owner/pkg) entries = %d, want 1 after one retry", len(got.Entries))
		}
		if _, err := c.ReadDetail(t.Context(), ref); err != nil {
			t.Fatalf("ReadDetail(owner/pkg) error = %v", err)
		}

		if len(at) != 4 {
			t.Fatalf("server saw %d requests, want 4 (failed attempt, retry, versions page, redirect hop)", len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < DefaultMinPacing {
				t.Errorf("request %d started %v after request %d, want at least %v", i, gap, i-1, DefaultMinPacing)
			}
		}
	})
}
