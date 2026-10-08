package collect_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/internal/collect"
	"github.com/cplieger/registry-stats/internal/obs"
	"github.com/cplieger/registry-stats/internal/registry"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("Setup: parse %q: %v", s, err)
	}
	return v
}

func hubImages(n int, updated time.Time) []obs.ImageMetric {
	images := make([]obs.ImageMetric, 0, n)
	for i := range n {
		images = append(images, obs.ImageMetric{Registry: registry.DockerHub, Owner: "o", Repo: fmt.Sprintf("r%02d", i), Updated: updated, Detailed: true})
	}
	return images
}

func readRepos(f *fakeSource) []string {
	var out []string
	for _, r := range f.detailReads {
		out = append(out, r.Repo)
	}
	f.detailReads = nil
	return out
}

// TestDetails_SpreadsAColdStartOverADay pins the cold-start budget: at most
// ceil(images/24) never-read counts per cycle, longest-waiting first, and only
// those images publish a count.
func TestDetails_SpreadsAColdStartOverADay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dh := newFakeDockerHub()
		d := collect.NewDetails(slog.New(slog.DiscardHandler), time.Hour, dh)
		cycle := collect.Cycle{Images: hubImages(30, time.Time{})}

		values, _ := d.Refresh(t.Context(), &cycle)
		if got := readRepos(dh); !slices.Equal(got, []string{"r00", "r01"}) {
			t.Errorf("first Refresh read %v, want [r00 r01] (ceil(30/24) = 2)", got)
		}
		if len(values) != 2 {
			t.Errorf("first Refresh published %d counts, want 2: an unread image has no series", len(values))
		}

		time.Sleep(time.Hour)
		d.Refresh(t.Context(), &cycle)
		if got := readRepos(dh); !slices.Equal(got, []string{"r02", "r03"}) {
			t.Errorf("second Refresh read %v, want [r02 r03]", got)
		}
	})
}

// TestDetails_FillsAColdStartWithinADayAtAnyPollInterval pins the cold-start
// budget against the poll interval: every never-read count is read by the last
// cycle that starts within 24 hours, at most an even share per cycle, and a
// one-shot run reads them all in its one cycle.
func TestDetails_FillsAColdStartWithinADayAtAnyPollInterval(t *testing.T) {
	tests := []struct {
		name      string
		poll      time.Duration
		firstRead int
		cycles    int
	}{
		{"one-shot", 0, 30, 1},
		{"hourly", time.Hour, 2, 24},
		{"every 2 hours", 2 * time.Hour, 3, 12},
		{"every 5 hours", 5 * time.Hour, 6, 5},
		{"daily", 24 * time.Hour, 30, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dh := newFakeDockerHub()
				d := collect.NewDetails(slog.New(slog.DiscardHandler), tt.poll, dh)
				cycle := collect.Cycle{Images: hubImages(30, time.Time{})}

				d.Refresh(t.Context(), &cycle)
				if got := len(readRepos(dh)); got != tt.firstRead {
					t.Errorf("NewDetails(poll %v) first Refresh read %d counts, want %d", tt.poll, got, tt.firstRead)
				}
				var values []obs.DetailMetric
				for range tt.cycles - 1 {
					time.Sleep(tt.poll)
					values, _ = d.Refresh(t.Context(), &cycle)
				}
				if tt.cycles == 1 {
					values, _ = d.Refresh(t.Context(), &cycle)
				}
				if len(values) != 30 {
					t.Errorf("NewDetails(poll %v) published %d of 30 counts after %d cycles within a day, want all", tt.poll, len(values), tt.cycles)
				}
			})
		})
	}
}

// TestDetails_RereadsOnAPushOrAfterADay pins the refetch rule: a cached count is
// read again when the image's push time moved, or once it is 24 hours old, and
// not otherwise.
func TestDetails_RereadsOnAPushOrAfterADay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := newFakeGHCR()
		d := collect.NewDetails(slog.New(slog.DiscardHandler), time.Hour, gh)
		pushed := mustTime(t, "2026-10-01T00:00:00Z")
		image := obs.ImageMetric{Registry: registry.GHCR, Owner: "o", Repo: "a", LastPushed: pushed, Detailed: true}
		cycle := collect.Cycle{Images: []obs.ImageMetric{image}}

		d.Refresh(t.Context(), &cycle)
		readRepos(gh)

		time.Sleep(time.Hour)
		d.Refresh(t.Context(), &cycle)
		if got := readRepos(gh); len(got) != 0 {
			t.Errorf("Refresh an hour later read %v, want nothing (same push, fresh count)", got)
		}

		image.LastPushed = pushed.Add(time.Minute)
		d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{image}})
		if got := readRepos(gh); !slices.Equal(got, []string{"a"}) {
			t.Errorf("Refresh after a push read %v, want [a]", got)
		}

		time.Sleep(24 * time.Hour)
		d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{image}})
		if got := readRepos(gh); !slices.Equal(got, []string{"a"}) {
			t.Errorf("Refresh a day later read %v, want [a]", got)
		}
	})
}

// TestDetails_RateLimitStopsTheSourceAndKeepsItsCounts pins the 429 rule: the
// source's remaining reads stop for the cycle with one WARN, every cached count
// stays published, and the next cycle reads again.
func TestDetails_RateLimitStopsTheSourceAndKeepsItsCounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		dh := newFakeDockerHub()
		dh.details = map[registry.RepoRef]registry.Detail{ref("o", "r00"): {Tagged: 7}}
		d := collect.NewDetails(slog.New(slog.NewTextHandler(&buf, nil)), time.Hour, dh)
		images := hubImages(25, mustTime(t, "2026-10-01T00:00:00Z"))
		cycle := collect.Cycle{Images: images}
		d.Refresh(t.Context(), &cycle) // reads r00, r01
		readRepos(dh)

		time.Sleep(25 * time.Hour)
		dh.detailErrs = map[registry.RepoRef]error{ref("o", "r00"): &httpx.StatusError{Code: 429}}
		values, _ := d.Refresh(t.Context(), &cycle)

		if got := readRepos(dh); !slices.Equal(got, []string{"r00"}) {
			t.Errorf("Refresh under a rate limit read %v, want only [r00]: the 429 stops the rest", got)
		}
		if len(values) != 2 || values[0].Detail.Tagged != 7 {
			t.Errorf("Refresh under a rate limit published %+v, want both cached counts kept", values)
		}
		if n := strings.Count(buf.String(), `level=WARN msg="detail reads stopped" source=dockerhub reason=rate_limited`); n != 1 {
			t.Errorf("Refresh under a rate limit logged the stop %d times, want once; logs:\n%s", n, buf.String())
		}
	})
}

func TestDetails_AFailedReadUnpublishesTheCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dh := newFakeDockerHub()
		d := collect.NewDetails(slog.New(slog.DiscardHandler), time.Hour, dh)
		cycle := collect.Cycle{Images: hubImages(1, mustTime(t, "2026-10-01T00:00:00Z"))}
		if values, _ := d.Refresh(t.Context(), &cycle); len(values) != 1 {
			t.Fatalf("Setup: first Refresh published %d counts, want 1", len(values))
		}

		time.Sleep(25 * time.Hour)
		dh.detailErrs = map[registry.RepoRef]error{ref("o", "r00"): errors.New("format changed")}
		values, ages := d.Refresh(t.Context(), &cycle)

		if len(values) != 0 || len(ages) != 0 {
			t.Errorf("Refresh after a failed read = (%+v, %+v), want no count and no age: a count that may be wrong is not kept", values, ages)
		}
	})
}

// TestDetails_AFailingImageDoesNotStarveTheRest pins the retry order under a
// one-read cold-start budget: an image whose read keeps failing goes behind every
// image not yet tried, so the others are read and published, and it is retried
// once they have been.
func TestDetails_AFailingImageDoesNotStarveTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dh := newFakeDockerHub()
		dh.detailErrs = map[registry.RepoRef]error{ref("o", "r00"): errors.New("format changed")}
		d := collect.NewDetails(slog.New(slog.DiscardHandler), time.Hour, dh)
		// 24 images at an hourly poll: a cold-start budget of one read a cycle.
		cycle := collect.Cycle{Images: hubImages(24, time.Time{})}

		var reads []string
		var values []obs.DetailMetric
		for range 25 {
			values, _ = d.Refresh(t.Context(), &cycle)
			reads = append(reads, readRepos(dh)...)
			time.Sleep(time.Hour)
		}

		want := []string{"r00"}
		for i := 1; i < 24; i++ {
			want = append(want, fmt.Sprintf("r%02d", i))
		}
		want = append(want, "r00")
		if !slices.Equal(reads, want) {
			t.Errorf("25 hourly Refresh cycles with r00 failing read %v, want %v: a failed image retries after the untried ones", reads, want)
		}
		if len(values) != 23 || slices.ContainsFunc(values, func(v obs.DetailMetric) bool { return v.Repo == "r00" }) {
			t.Errorf("Refresh with r00 failing published %d counts %+v, want the 23 other images", len(values), values)
		}
	})
}

func TestDetails_ReportsTheOldestReadAndDropsDepartedImages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gh := newFakeGHCR()
		d := collect.NewDetails(slog.New(slog.DiscardHandler), time.Hour, gh)
		a := obs.ImageMetric{Registry: registry.GHCR, Owner: "o", Repo: "a", Detailed: true}
		b := obs.ImageMetric{Registry: registry.GHCR, Owner: "o", Repo: "b", Detailed: true}
		capped := obs.ImageMetric{Registry: registry.GHCR, Owner: "z", Repo: "late"}
		done := []obs.SourceCycle{{Source: registry.GHCR, Complete: true}}
		d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{a}, Sources: done})
		first := time.Now()
		time.Sleep(time.Hour)
		if _, ages := d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{a, b, capped}, Sources: done}); len(ages) != 1 || !ages[0].Oldest.Equal(first) {
			t.Errorf("Refresh(a, b) ages = %+v, want o/a's earlier read %v", ages, first)
		}
		readRepos(gh)

		values, ages := d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{b, capped}, Sources: done})

		if len(values) != 1 || values[0].Repo != "b" {
			t.Errorf("Refresh published %+v, want only o/b: o/a left and z/late is past the cap", values)
		}
		if len(ages) != 1 || !ages[0].Oldest.Equal(first.Add(time.Hour)) {
			t.Errorf("Refresh ages = %+v, want o/b's read an hour after the first", ages)
		}
		if slices.Contains(readRepos(gh), "late") {
			t.Error("Refresh read an image past the detail cap")
		}

		// o/a left a complete cycle, so on its return its old count is gone and it is read again.
		d.Refresh(t.Context(), &collect.Cycle{Images: []obs.ImageMetric{a, b}, Sources: done})
		if got := readRepos(gh); !slices.Equal(got, []string{"a"}) {
			t.Errorf("Refresh after o/a returned read %v, want [a]", got)
		}
	})
}

// TestDetails_KeepsCountsThroughACycleThatDidNotReadThem pins the cache across an
// incomplete cycle: a failed listing publishes no count for the images it missed,
// and their cached counts publish again, unread, when the images come back.
func TestDetails_KeepsCountsThroughACycleThatDidNotReadThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dh := newFakeDockerHub()
		d := collect.NewDetails(slog.New(slog.DiscardHandler), 0, dh)
		images := hubImages(48, mustTime(t, "2026-10-01T00:00:00Z"))
		done := []obs.SourceCycle{{Source: registry.DockerHub, Complete: true}}
		if values, _ := d.Refresh(t.Context(), &collect.Cycle{Images: images, Sources: done}); len(values) != 48 {
			t.Fatalf("Setup: first Refresh published %d counts, want 48", len(values))
		}
		readRepos(dh)

		time.Sleep(time.Hour)
		failed := collect.Cycle{Sources: []obs.SourceCycle{{Source: registry.DockerHub}}}
		if values, _ := d.Refresh(t.Context(), &failed); len(values) != 0 {
			t.Errorf("Refresh(failed listing) published %d counts, want none for images the cycle did not measure", len(values))
		}

		time.Sleep(time.Hour)
		values, _ := d.Refresh(t.Context(), &collect.Cycle{Images: images, Sources: done})
		if len(values) != 48 {
			t.Errorf("Refresh after the images returned published %d counts, want all 48 kept", len(values))
		}
		if got := readRepos(dh); len(got) != 0 {
			t.Errorf("Refresh after the images returned read %v, want nothing: the cached counts are an hour old", got)
		}
	})
}

func TestDetails_WarnsOnceWhenImagesStartToMissTheCap(t *testing.T) {
	var buf bytes.Buffer
	d := collect.NewDetails(slog.New(slog.NewTextHandler(&buf, nil)), time.Hour)
	over := collect.Cycle{Sources: []obs.SourceCycle{{Source: registry.GHCR, Omitted: 50}}}
	under := collect.Cycle{Sources: []obs.SourceCycle{{Source: registry.GHCR}}}

	for _, cycle := range []collect.Cycle{over, over, under, over} {
		d.Refresh(t.Context(), &cycle)
	}

	const line = `level=WARN msg="image details omitted" source=ghcr omitted=50`
	if n := strings.Count(buf.String(), line); n != 2 {
		t.Errorf("Refresh logged %q %d times over over, over, under, over, want 2 (each onset); logs:\n%s", line, n, buf.String())
	}
}
