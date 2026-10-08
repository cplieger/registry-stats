package obs

import (
	"strings"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/registry"
)

var (
	pushedAt  = time.Date(2026, 10, 5, 1, 2, 3, 0, time.UTC)
	updatedAt = time.Date(2026, 10, 6, 9, 12, 13, 0, time.UTC)
)

func assertLines(t *testing.T, call, body string, present, absent []string) {
	t.Helper()
	for _, line := range present {
		if !strings.Contains(body, line) {
			t.Errorf("%s exposition missing %q:\n%s", call, line, body)
		}
	}
	for _, line := range absent {
		if strings.Contains(body, line) {
			t.Errorf("%s exposition carries %q, want it absent:\n%s", call, line, body)
		}
	}
}

// TestSetImage_PublishesRegistryTimesForDetailedImagesOnly pins three rules: GHCR's
// push time is the only push-time series, Docker Hub's last_updated feeds only the
// repository-updated family, and an image past the detail cap carries neither.
func TestSetImage_PublishesRegistryTimesForDetailedImagesOnly(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: registry.GHCR, Owner: "o", Repo: "a", Pulls: 1, LastPushed: pushedAt, Detailed: true},
		{Registry: registry.DockerHub, Owner: "o", Repo: "a", Pulls: 2, Updated: updatedAt, Detailed: true},
		{Registry: registry.GHCR, Owner: "z", Repo: "late", Pulls: 3, LastPushed: pushedAt},
	})

	assertLines(t, "SetImage", scrapeBody(t, m), []string{
		`registrystats_image_last_pushed_timestamp_seconds{owner="o",registry="ghcr",repo="a"} 1791162123`,
		`registrystats_dockerhub_repository_updated_timestamp_seconds{owner="o",repo="a"} 1791277933`,
		`registrystats_image_pulls_total{owner="z",registry="ghcr",repo="late"} 3`,
	}, []string{
		`registrystats_image_last_pushed_timestamp_seconds{owner="o",registry="dockerhub"`,
		`repo="late"} 1791162123`,
	})
}

// TestSetImage_RetiresEveryPerImageSeriesOfADepartedImage pins the departure rule:
// an image a cycle did not measure loses its pull count, push time, update time and
// its tag and version counts in the same cycle.
func TestSetImage_RetiresEveryPerImageSeriesOfADepartedImage(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: registry.GHCR, Owner: "o", Repo: "gone", Pulls: 1, LastPushed: pushedAt, Detailed: true},
		{Registry: registry.DockerHub, Owner: "o", Repo: "gone", Pulls: 2, Updated: updatedAt, Detailed: true},
		{Registry: registry.GHCR, Owner: "o", Repo: "kept", Pulls: 3, LastPushed: pushedAt, Detailed: true},
	})
	m.SetDetails([]DetailMetric{
		{Registry: registry.GHCR, Owner: "o", Repo: "gone", Detail: registry.Detail{Tagged: 4, Untagged: 5}},
		{Registry: registry.DockerHub, Owner: "o", Repo: "gone", Detail: registry.Detail{Tagged: 6}},
		{Registry: registry.GHCR, Owner: "o", Repo: "kept", Detail: registry.Detail{Tagged: 7, Untagged: 8}},
	}, nil)

	m.SetImage([]ImageMetric{{Registry: registry.GHCR, Owner: "o", Repo: "kept", Pulls: 3, LastPushed: pushedAt, Detailed: true}})

	body := scrapeBody(t, m)
	assertLines(t, "SetImage(after departure)", body, []string{
		`registrystats_ghcr_versions{owner="o",repo="kept",state="untagged"} 8`,
		`registrystats_image_last_pushed_timestamp_seconds{owner="o",registry="ghcr",repo="kept"}`,
	}, nil)
	if strings.Contains(body, `repo="gone"`) {
		t.Errorf("SetImage(after departure) left a series of the departed image:\n%s", body)
	}
}

func TestSetPresence_ReplacesTheSet(t *testing.T) {
	m := New()
	m.SetPresence([]Presence{
		{Source: registry.DockerHub, Owner: "o", Repo: "a", Present: true},
		{Source: registry.DockerHub, Owner: "o", Repo: "b"},
	})
	m.SetPresence([]Presence{{Source: registry.DockerHub, Owner: "o", Repo: "b"}})

	assertLines(t, "SetPresence", scrapeBody(t, m),
		[]string{`registrystats_image_present{owner="o",repo="b",source="dockerhub"} 0`},
		[]string{`registrystats_image_present{owner="o",repo="a"`})
}

// TestSetSources_StampsOnlyAnsweredSources pins the active-source rule: every
// listed source gets its completeness and omitted count, only an answered one gets
// a last-success time, and a source the cycle did not list has no series at all.
func TestSetSources_StampsOnlyAnsweredSources(t *testing.T) {
	m := New()
	at := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	m.SetSources([]SourceCycle{
		{Source: registry.DockerHub, Answered: true, Complete: true},
		{Source: registry.GHCR, Omitted: 4},
	}, at)

	assertLines(t, "SetSources", scrapeBody(t, m), []string{
		`registrystats_collect_complete{source="dockerhub"} 1`,
		`registrystats_collect_complete{source="ghcr"} 0`,
		`registrystats_image_details_omitted{source="ghcr"} 4`,
		`registrystats_collect_last_success_timestamp_seconds{source="dockerhub"} 1791331200`,
	}, []string{`registrystats_collect_last_success_timestamp_seconds{source="ghcr"}`})

	only := New()
	only.SetSources([]SourceCycle{{Source: registry.DockerHub, Answered: true, Complete: true}}, at)
	if body := scrapeBody(t, only); strings.Contains(body, `source="ghcr"`) {
		t.Errorf("SetSources(Docker Hub only) exposed a ghcr series:\n%s", body)
	}
}

func TestSetDetails_PublishesEachRegistrysShapeAndTheOldestRead(t *testing.T) {
	m := New()
	oldest := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	m.SetDetails([]DetailMetric{
		{Registry: registry.DockerHub, Owner: "o", Repo: "a", Detail: registry.Detail{Tagged: 341}},
		{Registry: registry.GHCR, Owner: "o", Repo: "a", Detail: registry.Detail{Tagged: 1002, Untagged: 2215}},
	}, []DetailAge{{Source: registry.GHCR, Oldest: oldest}})

	assertLines(t, "SetDetails", scrapeBody(t, m), []string{
		`registrystats_dockerhub_tags{owner="o",repo="a"} 341`,
		`registrystats_ghcr_versions{owner="o",repo="a",state="tagged"} 1002`,
		`registrystats_ghcr_versions{owner="o",repo="a",state="untagged"} 2215`,
		`registrystats_details_oldest_read_timestamp_seconds{source="ghcr"} 1791244800`,
	}, []string{`registrystats_details_oldest_read_timestamp_seconds{source="dockerhub"}`})

	m.SetDetails(nil, nil)
	assertLines(t, "SetDetails(nil)", scrapeBody(t, m), nil, []string{
		`registrystats_dockerhub_tags{`, `registrystats_ghcr_versions{`, `registrystats_details_oldest_read_timestamp_seconds{`,
	})
}
