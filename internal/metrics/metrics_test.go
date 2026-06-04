package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsHandler(t *testing.T) {
	HTTPRequests.Inc("GET", "/metrics", "200")
	CollectsTotal.Inc("dockerhub")
	CollectErrors.Inc("ghcr")
	HTTPDuration.Observe(0.013)
	CollectDuration.Observe(1.42)
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "cplieger", Repo: "subflux", Pulls: 1234, Tags: 8},
		{Registry: "ghcr", Owner: "cplieger", Repo: "vibekit", Pulls: 56, Tags: 0},
	})

	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	Handler()(w, r)
	body := w.Body.String()

	want := []string{
		`registrystats_http_requests_total{method="GET",path="/metrics",status="200"} 1`,
		`registrystats_collects_total{source="dockerhub"} 1`,
		`registrystats_collect_errors_total{source="ghcr"} 1`,
		`registrystats_image_pulls_total{owner="cplieger",registry="dockerhub",repo="subflux"} 1234`,
		`registrystats_image_pulls_total{owner="cplieger",registry="ghcr",repo="vibekit"} 56`,
		`registrystats_image_tags{owner="cplieger",registry="dockerhub",repo="subflux"} 8`,
		`registrystats_http_request_duration_seconds_bucket{le="0.025"}`,
		`registrystats_collect_duration_seconds_count`,
		`process_goroutines`,
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

func TestSetImageMetricsResets(t *testing.T) {
	// First call sets two images.
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 1, Tags: 1},
		{Registry: "dockerhub", Owner: "a", Repo: "y", Pulls: 2, Tags: 2},
	})

	// Second call replaces with a single image — the dropped one should
	// disappear from the next handler output.
	SetImageMetrics([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "z", Pulls: 3, Tags: 3},
	})

	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	Handler()(w, r)
	body := w.Body.String()

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="z"} 3`) {
		t.Error("new image not present")
	}
	if strings.Contains(body, `repo="x"`) || strings.Contains(body, `repo="y"`) {
		t.Error("dropped images still present after Reset")
	}
}
