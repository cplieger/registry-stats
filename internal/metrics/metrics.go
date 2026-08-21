// Package metrics is a thin wrapper around github.com/cplieger/metrics/v3
// holding the registry-stats-specific metric instances and the SetImageMetrics
// adapter (which converts a per-cycle slice of ImageMetric records into the
// equivalent labeled-gauge state). The registry prefix ("registrystats") is
// applied to every metric name by the library.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	cm "github.com/cplieger/metrics/v4"
)

// registry holds every metric this package exposes. The "registrystats" prefix
// is prepended to each registered name by the library.
var registry = cm.NewRegistry("registrystats")

// Exported metric instances (names auto-prefixed with "registrystats_").
var (
	HTTPRequests = cm.NewLabeledCounter(
		"http_requests_total",
		"Total HTTP requests",
		[]string{"method", "path", "status"},
	)
	CollectsTotal = cm.NewLabeledCounter(
		"collects_total",
		"Total collection runs by source",
		[]string{"source"},
	)
	CollectErrors = cm.NewLabeledCounter(
		"collect_errors_total",
		"Total collection errors by source",
		[]string{"source"},
	)
	HTTPDuration = cm.NewHistogram(
		"http_request_duration_seconds",
		"HTTP request latency",
	)
	// CollectDuration uses APIBuckets (coarse, to 30s): a full Docker Hub +
	// GHCR collect cycle routinely exceeds 1s, which DefaultBuckets (max 1.0s)
	// would dump entirely into +Inf.
	CollectDuration = cm.NewHistogram(
		"collect_duration_seconds",
		"Collection cycle duration",
		cm.WithBuckets(cm.APIBuckets()),
	)

	// Per-image gauges populated by SetImageMetrics. Two parallel labeled
	// gauges (pulls + tags) keyed on (registry, owner, repo). Updated via
	// Set-then-delete-stale on each cycle so deleted images disappear
	// without the gauges ever being observably empty mid-update.
	imagePulls = cm.NewLabeledGauge(
		"image_pulls_total",
		"Total pull count per image",
		[]string{"registry", "owner", "repo"},
	)
	imageTags = cm.NewLabeledGauge(
		"image_tags",
		"Number of tags per image",
		[]string{"registry", "owner", "repo"},
	)
)

func init() {
	registry.RegisterLabeledCounter(HTTPRequests)
	registry.RegisterLabeledCounter(CollectsTotal)
	registry.RegisterLabeledCounter(CollectErrors)
	registry.RegisterLabeledGauge(imagePulls)
	registry.RegisterLabeledGauge(imageTags)
	registry.RegisterHistogram(HTTPDuration)
	registry.RegisterHistogram(CollectDuration)
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

// setMu serializes SetImageMetrics passes: the initial and the first
// scheduled collect can overlap (see main.go), and each pass reads and
// replaces the previous cycle's label sets below.
var setMu sync.Mutex

// prevPulls and prevTags hold the label sets the previous SetImageMetrics
// pass emitted, so the next pass can delete exactly the series that
// disappeared instead of resetting the whole gauge. Guarded by setMu.
var prevPulls, prevTags map[[3]string]bool

// SetImageMetrics replaces the current image gauge data for one collect
// cycle. Current values are Set in place first, then series absent from
// this cycle are Deleted (diffed against the previous pass), so images
// that disappear stop emitting. Unlike a Reset+Set pass, the gauges are
// never observably empty mid-update: a concurrent /metrics scrape sees
// every series with either its previous or its current cycle's value,
// never a partially-populated set — so a scrape landing mid-update
// cannot fake a pull-count regression to downstream alerting. (A scrape
// may still straddle the per-series updates themselves — some series
// fresh, some one cycle stale — which is benign for cumulative counts.)
func SetImageMetrics(images []ImageMetric) {
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
// library helper (caller-owned {method,path,status} label set).
func RecordHTTP(method, path string, status int, d time.Duration) {
	cm.RecordHTTP(HTTPRequests, HTTPDuration, d, method, path, strconv.Itoa(status))
}

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc {
	return registry.Handler()
}
