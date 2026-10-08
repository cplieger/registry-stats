package collect_test

import (
	"fmt"
	"log/slog"
	"reflect"
	"testing"

	"github.com/cplieger/registry-stats/internal/collect"
	"github.com/cplieger/registry-stats/internal/obs"
	"github.com/cplieger/registry-stats/internal/registry"
)

func runCycle(t *testing.T, sources ...collect.SourceRefs) collect.Cycle {
	t.Helper()
	return collect.Run(t.Context(), collect.Options{Metrics: obs.New(), Sources: sources, Logger: slog.New(slog.DiscardHandler)})
}

func ref(owner, repo string) registry.RepoRef { return registry.RepoRef{Owner: owner, Repo: repo} }

func entries(refs ...registry.RepoRef) []registry.Entry {
	out := make([]registry.Entry, 0, len(refs))
	for _, r := range refs {
		out = append(out, registry.Entry{Owner: r.Owner, Repo: r.Repo, Pulls: 1})
	}
	return out
}

// TestRun_DetailCapKeepsBothRegistriesOfANameTogether pins MaxDetailedNames: 300
// configured names detail the first 250 in byte order, on both registries alike, and
// each source counts its own 50 images past the cap.
func TestRun_DetailCapKeepsBothRegistriesOfANameTogether(t *testing.T) {
	var refs []registry.RepoRef
	for i := range 300 {
		refs = append(refs, ref("o", fmt.Sprintf("r%03d", i)))
	}
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	dh.collection = &registry.Collection{Entries: entries(refs...), Attempted: 300, Fetched: 300, Definitive: 300}
	gh.collection = &registry.Collection{Entries: entries(refs...), Attempted: 300, Fetched: 300, Definitive: 300}

	cycle := runCycle(t, workItem(dh, refs...), workItem(gh, refs...))

	detailed := map[registry.ID]map[string]bool{registry.DockerHub: {}, registry.GHCR: {}}
	for _, image := range cycle.Images {
		if image.Detailed {
			detailed[image.Registry][image.Repo] = true
		}
	}
	if !reflect.DeepEqual(detailed[registry.DockerHub], detailed[registry.GHCR]) {
		t.Error("Run detailed different names on the two registries, want a name's images in or out together")
	}
	if n := len(detailed[registry.GHCR]); n != collect.MaxDetailedNames || !detailed[registry.GHCR]["r249"] || detailed[registry.GHCR]["r250"] {
		t.Errorf("Run detailed %d names (r249 %v, r250 %v), want the first %d in byte order",
			n, detailed[registry.GHCR]["r249"], detailed[registry.GHCR]["r250"], collect.MaxDetailedNames)
	}
	for _, s := range cycle.Sources {
		if s.Omitted != 50 {
			t.Errorf("Run %s omitted = %d, want 50", s.Source, s.Omitted)
		}
	}
	if len(cycle.Presence) != 2*collect.MaxDetailedNames {
		t.Errorf("Run published %d presence series, want %d (detailed names only)", len(cycle.Presence), 2*collect.MaxDetailedNames)
	}
}

func TestRun_CompleteOnlyWhenEveryReadWasDefinitive(t *testing.T) {
	wildcard := ref("o", "*")
	tests := []struct {
		name       string
		refs       []registry.RepoRef
		collection registry.Collection
		want       bool
	}{
		{
			"every fetch an entry or a 404",
			[]registry.RepoRef{ref("o", "a"), ref("o", "b")},
			registry.Collection{Entries: entries(ref("o", "a")), Absent: []registry.RepoRef{ref("o", "b")}, Attempted: 2, Fetched: 1, Definitive: 2},
			true,
		},
		{
			"one fetch failed",
			[]registry.RepoRef{ref("o", "a"), ref("o", "b")},
			registry.Collection{Entries: entries(ref("o", "a")), Unread: []registry.RepoRef{ref("o", "b")}, Attempted: 2, Fetched: 1, Definitive: 1},
			false,
		},
		{
			"listing read whole",
			[]registry.RepoRef{wildcard},
			registry.Collection{Entries: entries(ref("o", "a")), Listed: []string{"o"}},
			true,
		},
		{
			"listing truncated at its page cap",
			[]registry.RepoRef{wildcard},
			registry.Collection{Entries: entries(ref("o", "a"))},
			false,
		},
		{
			"listing failed",
			[]registry.RepoRef{wildcard},
			registry.Collection{ListingFailed: true},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dh := newFakeDockerHub()
			dh.collection = &tt.collection

			cycle := runCycle(t, workItem(dh, tt.refs...))

			if len(cycle.Sources) != 1 || cycle.Sources[0].Complete != tt.want {
				t.Errorf("Run(%s) sources = %+v, want one source with Complete %v", tt.name, cycle.Sources, tt.want)
			}
		})
	}
}

// TestRun_PresenceSeparatesNotConfiguredFromAbsent pins the presence rules the two
// missing-package rows of Registries out of step read: no series where the
// configuration does not cover a name or the read was not definitive, 0 only for a
// definitive absence.
func TestRun_PresenceSeparatesNotConfiguredFromAbsent(t *testing.T) {
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	// Docker Hub: o/* read whole holds o/a only; x/explicit answered 404; x/flaky failed.
	dh.collection = &registry.Collection{
		Entries:    entries(ref("o", "a")),
		Absent:     []registry.RepoRef{ref("x", "explicit")},
		Unread:     []registry.RepoRef{ref("x", "flaky")},
		Listed:     []string{"o"},
		Attempted:  2,
		Definitive: 1,
	}
	// GHCR: o/a and o/b present, plus z/only on an owner Docker Hub does not cover.
	// No source measured x/explicit, so only its explicit ref makes it a detailed name.
	gh.collection = &registry.Collection{Entries: entries(ref("o", "a"), ref("o", "b"), ref("z", "only"), ref("x", "flaky"))}

	cycle := runCycle(t,
		workItem(dh, ref("o", "*"), ref("x", "explicit"), ref("x", "flaky")),
		workItem(gh, ref("o", "*"), ref("z", "*"), ref("x", "*")))

	got := map[string]bool{}
	for _, p := range cycle.Presence {
		if p.Source == registry.DockerHub {
			got[p.Owner+"/"+p.Repo] = p.Present
		}
	}
	want := map[string]bool{"o/a": true, "o/b": false, "x/explicit": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run Docker Hub presence = %v, want %v (z/only uncovered and x/flaky unread have no series)", got, want)
	}
}

func TestRun_PresenceNeedsAWholeListing(t *testing.T) {
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	dh.collection = &registry.Collection{Entries: entries(ref("o", "a"))} // capped: o not listed
	gh.collection = &registry.Collection{Entries: entries(ref("o", "a"), ref("o", "b"))}

	cycle := runCycle(t, workItem(dh, ref("o", "*")), workItem(gh, ref("o", "*")))

	for _, p := range cycle.Presence {
		if p.Source == registry.DockerHub && p.Repo == "b" {
			t.Errorf("Run published Docker Hub presence %+v for a name a truncated listing may hold", p)
		}
	}
}

// A Docker Hub repository name is one path segment, so a nested GHCR package such
// as o/app/dashboard can never have a Docker Hub pair to be missing from.
func TestRun_NoDockerHubPresenceForANestedGHCRName(t *testing.T) {
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	dh.collection = &registry.Collection{Entries: entries(ref("o", "app")), Listed: []string{"o"}}
	gh.collection = &registry.Collection{Entries: entries(ref("o", "app"), ref("o", "app/dashboard")), Listed: []string{"o"}}

	cycle := runCycle(t, workItem(dh, ref("o", "*")), workItem(gh, ref("o", "*")))

	got := map[string]bool{}
	for _, p := range cycle.Presence {
		got[p.Source.String()+" "+p.Repo] = p.Present
	}
	want := map[string]bool{"dockerhub app": true, "ghcr app": true, "ghcr app/dashboard": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run presence = %v, want %v (no Docker Hub series for app/dashboard)", got, want)
	}
}

// A 404 is a definitive answer, so a source whose explicit refs all answered 404
// is answered and complete: its read stamps a last success, which Registries out
// of step needs to report the package as missing rather than the read as stale.
func TestRun_ADefinitive404IsAnAnsweredRead(t *testing.T) {
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	dh.collection = &registry.Collection{Absent: []registry.RepoRef{ref("o", "app")}, Attempted: 1, Definitive: 1}
	gh.collection = &registry.Collection{Entries: entries(ref("o", "app")), Attempted: 1, Fetched: 1, Definitive: 1}

	cycle := runCycle(t, workItem(dh, ref("o", "app")), workItem(gh, ref("o", "app")))

	want := []obs.SourceCycle{
		{Source: registry.DockerHub, Answered: true, Complete: true},
		{Source: registry.GHCR, Answered: true, Complete: true},
	}
	if !reflect.DeepEqual(cycle.Sources, want) {
		t.Errorf("Run sources = %+v, want %+v", cycle.Sources, want)
	}
	for _, p := range cycle.Presence {
		if p.Source == registry.DockerHub && p.Present {
			t.Errorf("Run Docker Hub presence %+v, want 0 for the 404", p)
		}
	}
}

func TestRun_AMostlyUnreadSourceIsNotAnswered(t *testing.T) {
	tests := []struct {
		name       string
		collection registry.Collection
	}{
		{"most fetches unread", registry.Collection{
			Entries: entries(ref("o", "a")), Unread: []registry.RepoRef{ref("o", "b"), ref("o", "c")},
			Attempted: 3, Fetched: 1, Definitive: 1,
		}},
		{"listing failed", registry.Collection{ListingFailed: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dh := newFakeDockerHub()
			dh.collection = &tt.collection

			cycle := runCycle(t, workItem(dh, ref("o", "a"), ref("o", "b"), ref("o", "c")))

			if len(cycle.Sources) != 1 || cycle.Sources[0].Answered {
				t.Errorf("Run(%s) sources = %+v, want one source not Answered", tt.name, cycle.Sources)
			}
		})
	}
}

func TestRun_CarriesTheRegistryTimesOntoTheImages(t *testing.T) {
	dh, gh := newFakeDockerHub(), newFakeGHCR()
	updated := mustTime(t, "2026-10-06T09:12:13Z")
	pushed := mustTime(t, "2026-10-05T01:02:03Z")
	dh.collection = &registry.Collection{Entries: []registry.Entry{{Owner: "o", Repo: "a", Pulls: 1, Updated: updated}}}
	gh.collection = &registry.Collection{Entries: []registry.Entry{{Owner: "o", Repo: "a", Pulls: 2, LastPushed: pushed}}}

	cycle := runCycle(t, workItem(dh, ref("o", "a")), workItem(gh, ref("o", "a")))

	want := []obs.ImageMetric{
		{Registry: registry.DockerHub, Owner: "o", Repo: "a", Pulls: 1, Updated: updated, Detailed: true},
		{Registry: registry.GHCR, Owner: "o", Repo: "a", Pulls: 2, LastPushed: pushed, Detailed: true},
	}
	if !reflect.DeepEqual(cycle.Images, want) {
		t.Errorf("Run images = %+v, want %+v", cycle.Images, want)
	}
	if cycle.Finished.IsZero() {
		t.Error("Run Finished is zero, want the cycle's end time for the last-success stamp")
	}
}
