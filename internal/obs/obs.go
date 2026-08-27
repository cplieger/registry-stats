// Package obs holds registry-stats' observability surface: the app-specific
// metric instances built on github.com/cplieger/metrics/v4 and the SetImage
// adapter (which converts a per-cycle slice of ImageMetric records into the
// equivalent labeled-gauge state). The registry prefix ("registrystats") is
// applied to every metric name by the library. Named obs, not metrics: the
// library owns that name, and the old collision forced a two-letter cm
// alias at every meeting point.
package obs

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/cplieger/metrics/v4"
	"github.com/cplieger/webhttp/v2"
)

// reg holds every metric this package exposes. The "registrystats" prefix
// is prepended to each registered name by the library.
var reg = metrics.NewRegistry("registrystats")

// Exported metric instances (names auto-prefixed with "registrystats_").
var (
	HTTPRequests = metrics.NewLabeledCounter(
		"http_requests_total",
		"Total HTTP requests",
		[]string{"method", "path", "status"},
	)
	CollectsTotal = metrics.NewLabeledCounter(
		"collects_total",
		"Total collection runs by source",
		[]string{"source"},
	)
	CollectErrors = metrics.NewLabeledCounter(
		"collect_errors_total",
		"Total collection errors by source",
		[]string{"source"},
	)
	HTTPDuration = metrics.NewHistogram(
		"http_request_duration_seconds",
		"HTTP request latency",
	)
	// CollectDuration uses APIBuckets (coarse, to 30s): a full Docker Hub +
	// GHCR collect cycle routinely exceeds 1s, which DefaultBuckets (max 1.0s)
	// would dump entirely into +Inf.
	CollectDuration = metrics.NewHistogram(
		"collect_duration_seconds",
		"Collection cycle duration",
		metrics.WithBuckets(metrics.APIBuckets()),
	)

	// Per-image gauges populated by SetImage. Two parallel labeled
	// gauges (pulls + tags) keyed on (registry, owner, repo). Updated via
	// Set-then-delete-stale on each cycle so deleted images disappear
	// without the gauges ever being observably empty mid-update.
	imagePulls = metrics.NewLabeledGauge(
		"image_pulls_total",
		"Total pull count per image",
		[]string{"registry", "owner", "repo"},
	)
	imageTags = metrics.NewLabeledGauge(
		"image_tags",
		"Number of tags per image",
		[]string{"registry", "owner", "repo"},
	)
)

func init() {
	// MustRegister surfaces construction-time validation (metrics v4 captures
	// a malformed name or bucket layout into the metric value, client_golang
	// style) at process start: a bad definition fails the first test run and
	// the container boot, never the scrape path.
	reg.MustRegister(
		HTTPRequests,
		CollectsTotal,
		CollectErrors,
		imagePulls,
		imageTags,
		HTTPDuration,
		CollectDuration,
	)
}

// MintCollectSources pre-mints the two per-source collect counters at zero for
// every source in sources, so increase() has an earlier sample to subtract from
// and the FIRST failure of a source fires a windowed alert. Pass only sources
// the process will poll: a series for an unpolled source claims it is polled.
func MintCollectSources(sources []string) {
	for _, source := range sources {
		CollectsTotal.Add(0, source)
		CollectErrors.Add(0, source)
	}
}

// ImageMetric holds per-image gauge data set after each collect cycle.
// Image metrics are registry-stats-local (the library deliberately does not
// expose an image-metrics feature); this is layered on labeled gauges.
type ImageMetric struct {
	Registry string // "dockerhub" or "ghcr"
	Owner    string
	Repo     string
	Pulls    int64
	Tags     int
}

// setMu serializes SetImage passes, because each pass reads and replaces the
// previous cycle's label sets below. The collect loop is sequential
// (scheduler.RunLoop runs one cycle at a time and fires the startup cycle as
// its own first iteration), so there is exactly one caller today and the lock
// is uncontended; it is kept so this exported function is safe on its own terms
// rather than only while its caller happens to be single-threaded.
var setMu sync.Mutex

// prevPulls and prevTags hold the label sets the previous SetImage
// pass emitted, so the next pass can delete exactly the series that
// disappeared instead of resetting the whole gauge. Guarded by setMu.
var prevPulls, prevTags map[[3]string]bool

// SetImage replaces the current image gauge data for one collect
// cycle. Current values are Set in place first, then series absent from
// this cycle are Deleted (diffed against the previous pass), so images
// that disappear stop emitting. Unlike a Reset+Set pass, the gauges are
// never observably empty mid-update: a concurrent /metrics scrape sees
// every series with either its previous or its current cycle's value,
// never a partially-populated set — so a scrape landing mid-update
// cannot fake a pull-count regression to downstream alerting. (A scrape
// may still straddle the per-series updates themselves — some series
// fresh, some one cycle stale — which is benign for cumulative counts.)
func SetImage(images []ImageMetric) {
	setMu.Lock()
	defer setMu.Unlock()
	pulls := make(map[[3]string]bool, len(images))
	tags := make(map[[3]string]bool, len(images))
	for _, m := range images {
		key := [3]string{m.Registry, m.Owner, m.Repo}
		imagePulls.Set(float64(m.Pulls), m.Registry, m.Owner, m.Repo)
		pulls[key] = true
		// Emit a tags series only for a positive count: GHCR entries carry
		// no tag count, and a Docker Hub count-fetch failure leaves 0, so
		// image_tags=0 would be misleading; a 0 pull count is a real value.
		if m.Tags > 0 {
			imageTags.Set(float64(m.Tags), m.Registry, m.Owner, m.Repo)
			tags[key] = true
		}
	}
	for key := range prevPulls {
		if !pulls[key] {
			imagePulls.Delete(key[0], key[1], key[2])
		}
	}
	for key := range prevTags {
		if !tags[key] {
			imageTags.Delete(key[0], key[1], key[2])
		}
	}
	prevPulls, prevTags = pulls, tags
}

// RecordHTTP records one HTTP request into the package HTTP metrics via the
// library helper (caller-owned {method,path,status} label set). It takes
// webhttp's RequestMetric, so the two string labels arrive named rather than
// positional and this sink cannot transpose them.
func RecordHTTP(m webhttp.RequestMetric) {
	metrics.RecordHTTP(HTTPRequests, HTTPDuration, m.Latency, m.Method, m.Path, strconv.Itoa(m.Status))
}

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc {
	return reg.Handler()
}
