package obs

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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
		{Registry: "dockerhub", Owner: "cplieger", Repo: "subflux", Pulls: 1234},
		{Registry: "ghcr", Owner: "cplieger", Repo: "vibekit", Pulls: 56},
	})

	body := scrapeBody(t, m)

	want := []string{
		`registrystats_http_requests_total{method="GET",path="/metrics",status="200"} 1`,
		`registrystats_collects_total{source="dockerhub"} 1`,
		`registrystats_collect_errors_total{source="ghcr"} 1`,
		`registrystats_image_pulls_total{owner="cplieger",registry="dockerhub",repo="subflux"} 1234`,
		`registrystats_image_pulls_total{owner="cplieger",registry="ghcr",repo="vibekit"} 56`,
		`registrystats_http_request_duration_seconds_bucket{le="0.025"}`,
		`registrystats_collect_duration_seconds_bucket{le="5"} 1`,
		`registrystats_collect_duration_seconds_count`,
		`go_goroutines`,
		`process_uptime_seconds`,
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("missing line: %s", line)
		}
	}
}

func TestObserveCollectDuration_recordsSeconds(t *testing.T) {
	m := New()
	m.ObserveCollectDuration(1420 * time.Millisecond)

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_collect_duration_seconds_sum 1.42`) {
		t.Errorf("ObserveCollectDuration(1420ms) sum missing or not in seconds:\n%s", body)
	}
}

func TestRecordHTTP_recordsLatencyInSeconds(t *testing.T) {
	m := New()
	m.RecordHTTP(webhttp.RequestMetric{
		Method:  http.MethodGet,
		Path:    "/metrics",
		Status:  http.StatusOK,
		Latency: 13 * time.Millisecond,
	})

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_http_request_duration_seconds_count 1`) {
		t.Errorf("RecordHTTP() duration count missing:\n%s", body)
	}
	if !strings.Contains(body, `registrystats_http_request_duration_seconds_sum 0.013`) {
		t.Errorf("RecordHTTP() duration sum missing or not in seconds:\n%s", body)
	}
}

// TestSetImage_replacesSeriesSet pins the per-cycle replacement contract.
func TestSetImage_replacesSeriesSet(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "x", Pulls: 1},
		{Registry: "dockerhub", Owner: "a", Repo: "y", Pulls: 2},
	})
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "z", Pulls: 3},
	})

	body := scrapeBody(t, m)

	if !strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="z"} 3`) {
		t.Error("new image not present")
	}
	if strings.Contains(body, `repo="x"`) || strings.Contains(body, `repo="y"`) {
		t.Error("dropped images still present after replacement")
	}
}

// TestSetImage_emptyCycleClearsAll pins the all-failed edge.
func TestSetImage_emptyCycleClearsAll(t *testing.T) {
	m := New()
	m.SetImage([]ImageMetric{
		{Registry: "dockerhub", Owner: "a", Repo: "gone", Pulls: 5},
	})
	m.SetImage(nil)

	body := scrapeBody(t, m)

	if strings.Contains(body, `repo="gone"`) {
		t.Error("series survived an empty cycle, want all image series cleared")
	}
}

func TestSetImage_preservesSurvivingSeriesDuringConcurrentScrape(t *testing.T) {
	const distractors = 64
	sets := [2][]ImageMetric{
		make([]ImageMetric, distractors+1),
		make([]ImageMetric, distractors+1),
	}
	for i := range distractors {
		sets[0][i] = ImageMetric{Registry: "dockerhub", Owner: "other", Repo: strings.Repeat("a", i+1), Pulls: int64(i)}
		sets[1][i] = ImageMetric{Registry: "dockerhub", Owner: "other", Repo: strings.Repeat("b", i+1), Pulls: int64(i)}
	}
	sets[0][distractors] = ImageMetric{Registry: "dockerhub", Owner: "a", Repo: "stable", Pulls: 1}
	sets[1][distractors] = ImageMetric{Registry: "dockerhub", Owner: "a", Repo: "stable", Pulls: 2}

	m := New()
	m.SetImage(sets[0])
	stop := make(chan struct{})
	started := make(chan struct{})
	updates := make(chan int, 1)
	go func() {
		count := 0
		for {
			select {
			case <-stop:
				updates <- count
				return
			default:
			}
			m.SetImage(sets[count%len(sets)])
			count++
			if count == 1 {
				close(started)
			}
		}
	}()
	<-started

	missing := false
	for range 2000 {
		body := scrapeBody(t, m)
		hasOld := strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="stable"} 1`)
		hasNew := strings.Contains(body, `registrystats_image_pulls_total{owner="a",registry="dockerhub",repo="stable"} 2`)
		if !hasOld && !hasNew {
			missing = true
			break
		}
	}
	close(stop)
	updateCount := <-updates

	if updateCount == 0 {
		t.Fatal("SetImage() completed no concurrent updates")
	}
	if missing {
		t.Error("SetImage() omitted the surviving image_pulls series during a concurrent scrape")
	}
}

func TestMetricsHandler_publishesSeriesUsedByShippedConsumers(t *testing.T) {
	m := New()
	m.MintCollectSources([]string{"dockerhub"})
	m.SetImage([]ImageMetric{{Registry: "dockerhub", Owner: "owner", Repo: "repo", Pulls: 1}})
	body := scrapeBody(t, m)

	metricName := regexp.MustCompile(`registrystats_[a-z_]+`)
	consumerSeries := make(map[string]bool)
	for _, path := range []string{"../../grafana-dashboard.json", "../../alerts/promql.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Setup: read shipped metric consumer %s: %v", path, err)
		}
		for _, name := range metricName.FindAllString(string(data), -1) {
			consumerSeries[name] = true
		}
	}
	for name := range consumerSeries {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("shipped consumer series %q is absent from /metrics", name)
		}
	}
}
