// Package metrics is a thin wrapper around github.com/cplieger/metrics/v2
// holding the registry-stats-specific metric instances and the SetImageMetrics
// adapter (which converts a per-cycle slice of ImageMetric records into the
// equivalent labeled-gauge state). The registry prefix ("registrystats") is
// applied to every metric name by the library.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	cm "github.com/cplieger/metrics/v2"
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
		cm.WithBuckets(cm.APIBuckets),
	)

	// Per-image gauges populated by SetImageMetrics. Two parallel labeled
	// gauges (pulls + tags) keyed on (registry, owner, repo). Exposed via
	// Reset+Set on each cycle so deleted images naturally disappear.
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

// SetImageMetrics replaces the current image gauge data atomically. Reset+Set
// rather than incremental update so images that disappear from the snapshot
// stop emitting.
func SetImageMetrics(images []ImageMetric) {
	imagePulls.Reset()
	imageTags.Reset()
	for _, m := range images {
		imagePulls.Set(float64(m.Pulls), m.Registry, m.Owner, m.Repo)
		if m.Tags > 0 {
			imageTags.Set(float64(m.Tags), m.Registry, m.Owner, m.Repo)
		}
	}
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
