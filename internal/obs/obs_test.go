package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/webhttp/v2"
)

func scrapeBody(t *testing.T, m *Metrics) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler()(w, r)
	return w.Body.String()
}

func TestMetricsHandler(t *testing.T) {
	m := New()
	m.RecordHTTP(webhttp.RequestMetric{
		Method:  http.MethodGet,
		Path:    "/metrics",
		Status:  http.StatusOK,
		Latency: 13 * time.Millisecond,
	})
	m.RecordCollect("dockerhub", false)
	m.RecordCollect("ghcr", true)
	m.ObserveCollectDuration(1420 * time.Millisecond)
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "cplieger", Repo: "subflux", Pulls: 1234, Tags: new(8)},
		{Registry: "ghcr", Owner: "cplieger", Repo: "vibekit", Pulls: 56},
	})

	body := scrapeBody(t, m)

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

	if strings.Contains(body, `registrystats_image_tags{owner="cplieger",registry="ghcr",repo="vibekit"}`) {
		t.Error("image without a tag count should not emit a tags gauge")
	}
}

// TestSetImage_replacesSeriesSet pins the per-cycle replacement contract.
func TestSetImage_replacesSeriesSet(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 1, Tags: new(1)},
		{Registry: "dockerhub", Owner: "a", Repo: "y", Pulls: 2, Tags: new(2)},
	})
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "z", Pulls: 3, Tags: new(3)},
	})

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="z"} 3`) {
		t.Error("new image not present")
	}
	if strings.Contains(body, `repo="x"`) || strings.Contains(body, `repo="y"`) {
		t.Error("dropped images still present after replacement")
	}
}

// TestSetImage_missingTagsRemovesSeries pins the stale-series diff.
func TestSetImage_missingTagsRemovesSeries(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 10, Tags: new(4)},
	})
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 11},
	})

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="x"} 11`) {
		t.Error("pulls series missing or stale after the second cycle")
	}
	if strings.Contains(body, `registrystats_image_tags{owner="a",registry="dockerhub",repo="x"}`) {
		t.Error("image_tags series lingered after the tag count became unavailable")
	}
}

// TestSetImage_measuredZeroEmitsSeries pins zero as a measured value.
func TestSetImage_measuredZeroEmitsSeries(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 10, Tags: new(0)},
	})

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_image_tags{owner="a",registry="dockerhub",repo="x"} 0`) {
		t.Errorf("measured zero tag count missing from image_tags:\n%s", body)
	}
}

// TestSetImage_emptyCycleClearsAll pins the all-failed edge.
func TestSetImage_emptyCycleClearsAll(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "gone", Pulls: 5, Tags: new(1)},
	})
	m.SetImage(nil)

	body := scrapeBody(t, m)

	if strings.Contains(body, `repo="gone"`) {
		t.Error("series survived an empty cycle, want all image series cleared")
	}
}
