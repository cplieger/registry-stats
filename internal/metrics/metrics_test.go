package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrapeBody renders the current /metrics output. SetImageMetrics
// mutates process-global gauges, so tests here must stay serial (no
// t.Parallel) and each pins only its own final state.
func scrapeBody(t *testing.T) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	Handler()(w, r)
	return w.Body.String()
}

func TestMetricsHandler(t *testing.T) {
	HTTPRequests.Inc(http.MethodGet, "/metrics", "200")
	CollectsTotal.Inc("dockerhub")
	CollectErrors.Inc("ghcr")
	HTTPDuration.Observe(0.013)
	CollectDuration.Observe(1.42)
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "cplieger", Repo: "subflux", Pulls: 1234, Tags: 8},
		{Registry: "ghcr", Owner: "cplieger", Repo: "vibekit", Pulls: 56, Tags: 0},
	})

	body := scrapeBody(t)

	want := []string{
		`registrystats_http_requests_total{method="GET",path="/metrics",status="200"} 1`,
		`registrystats_collects_total{source="dockerhub"} 1`,
		`registrystats_collect_errors_total{source="ghcr"} 1`,
		`registrystats_image_pulls_total{owner="cplieger",registry="dockerhub",repo="subflux"} 1234`,
		`registrystats_image_pulls_total{owner="cplieger",registry="ghcr",repo="vibekit"} 56`,
		`registrystats_image_tags{owner="cplieger",registry="dockerhub",repo="subflux"} 8`,
		`registrystats_http_request_duration_seconds_bucket{le="0.025"}`,
		`registrystats_collect_duration_seconds_count`,
		`go_goroutines`,
		`process_uptime_seconds`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("missing line: %s", line)
		}
	}

	// vibekit had Tags=0 — should NOT appear in image_tags output (only positive tag counts emit)
	if strings.Contains(body, `registrystats_image_tags{owner="cplieger",registry="ghcr",repo="vibekit"}`) {
		t.Error("zero-tag image should not emit a tags gauge")
	}
}

// TestSetImageMetrics_replacesSeriesSet pins the per-cycle replacement
// contract: images absent from the new cycle disappear from the output,
// and images present in both cycles carry the new values.
func TestSetImageMetrics_replacesSeriesSet(t *testing.T) {
	// First call sets two images.
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 1, Tags: 1},
		{Registry: "dockerhub", Owner: "a", Repo: "y", Pulls: 2, Tags: 2},
	})

	// Second call replaces with a single image — the dropped ones should
	// disappear from the next handler output.
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "z", Pulls: 3, Tags: 3},
	})

	body := scrapeBody(t)

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="z"} 3`) {
		t.Error("new image not present")
	}
	if strings.Contains(body, `repo="x"`) || strings.Contains(body, `repo="y"`) {
		t.Error("dropped images still present after replacement")
	}
}

// TestSetImageMetrics_tagsDroppingToZeroRemovesSeries pins the
// stale-series diff on the tags gauge specifically: an image whose tag
// count goes from positive to 0 (count fetch failed, or the repo now has
// no tags) keeps its pulls series with the fresh value but loses its
// image_tags series — a stale count must not linger from the prior cycle.
func TestSetImageMetrics_tagsDroppingToZeroRemovesSeries(t *testing.T) {
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 10, Tags: 4},
	})
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 11, Tags: 0},
	})

	body := scrapeBody(t)

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="x"} 11`) {
		t.Error("pulls series missing or stale after the second cycle")
	}
	if strings.Contains(body, `registrystats_image_tags{owner="a",registry="dockerhub",repo="x"}`) {
		t.Error("image_tags series lingered after the tag count dropped to 0")
	}
}

// TestSetImageMetrics_emptyCycleClearsAll pins the all-failed edge: an
// empty update removes every previously-emitted image series (matching
// the old Reset semantics for a cycle that collected nothing).
func TestSetImageMetrics_emptyCycleClearsAll(t *testing.T) {
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "gone", Pulls: 5, Tags: 1},
	})
	SetImageMetrics(nil)

	body := scrapeBody(t)

	if strings.Contains(body, `repo="gone"`) {
		t.Error("series survived an empty cycle, want all image series cleared")
	}
}
