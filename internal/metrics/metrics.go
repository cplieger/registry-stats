// Package metrics is a thin wrapper around github.com/cplieger/metrics
// holding the registry-stats-specific metric instances and the SetImageMetrics
// adapter (which converts a per-cycle slice of ImageMetric records into
// the equivalent labeled-gauge state).
//
// All the Prometheus text encoding, atomic counter/histogram/gauge
// implementations, and process metrics (goroutines, heap, gc, uptime) are
// provided by the library. This file is just the wiring.
package metrics

import (
	"net/http"

	cm "github.com/cplieger/metrics"
)

// registry holds every metric this package exposes. Registered in init() so
// the order is canonical (matches the library's emission order: labeled
// counters → counters → labeled gauges → gauges → histograms → labeled
// histograms → process metrics).
var registry = cm.NewRegistry("registrystats")

// Exported metric instances. Same names + label sets as the previous
// hand-rolled implementation so the existing call sites and Prometheus
// scrape output are unchanged.
var (
	HTTPRequests = cm.NewLabeledCounter(
		"registrystats_http_requests_total",
		"Total HTTP requests",
		[]string{"method", "path", "status"},
	)
	CollectsTotal = cm.NewLabeledCounter(
		"registrystats_collects_total",
		"Total collection runs by source",
		[]string{"source"},
	)
	CollectErrors = cm.NewLabeledCounter(
		"registrystats_collect_errors_total",
		"Total collection errors by source",
		[]string{"source"},
	)
	HTTPDuration = cm.NewHistogram(
		"registrystats_http_request_duration_seconds",
		"HTTP request latency",
	)
	CollectDuration = cm.NewHistogram(
		"registrystats_collect_duration_seconds",
		"Collection cycle duration",
	)

	// Per-image gauges populated by SetImageMetrics. Two parallel labeled
	// gauges (pulls + tags) keyed on (registry, owner, repo). Exposed
	// via Reset+Set on each cycle so deleted images naturally disappear.
	imagePulls = cm.NewLabeledGauge(
		"registrystats_image_pulls_total",
		"Total pull count per image",
		[]string{"registry", "owner", "repo"},
	)
	imageTags = cm.NewLabeledGauge(
		"registrystats_image_tags",
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
// The shape is unchanged from the pre-migration version so main.go's
// ImageMetric construction continues to compile without edits.
type ImageMetric struct {
	Registry string // "dockerhub" or "ghcr"
	Owner    string
	Repo     string
	Pulls    int64
	Tags     int
}

// SetImageMetrics replaces the current image gauge data atomically.
// Reset+Set rather than incremental update so images that disappear from
// the snapshot stop emitting (matches the previous slice-replacement
// semantics).
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

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc {
	return registry.Handler()
}
