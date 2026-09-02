// Package obs holds registry-stats' observability surface.
package obs

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/metrics/v4"
	"github.com/cplieger/webhttp/v2"
)

// Metrics records and serves registry-stats metrics. Every method is safe for
// concurrent use: the metric values synchronize themselves and setMu covers the
// per-cycle label bookkeeping, so the HTTP goroutines and the collect loop
// share one value.
type Metrics struct {
	registry        *metrics.Registry
	httpRequests    *metrics.LabeledCounter
	collectsTotal   *metrics.LabeledCounter
	collectErrors   *metrics.LabeledCounter
	httpDuration    *metrics.Histogram
	collectDuration *metrics.Histogram
	imagePulls      *metrics.LabeledGauge
	imageTags       *metrics.LabeledGauge
	prevPulls       map[[3]string]bool
	prevTags        map[[3]string]bool
	setMu           sync.Mutex
}

// New constructs an isolated metrics registry.
func New() *Metrics {
	m := &Metrics{
		registry: metrics.NewRegistry("registrystats"),
		httpRequests: metrics.NewLabeledCounter(
			"http_requests_total",
			"Total HTTP requests",
			[]string{"method", "path", "status"},
		),
		collectsTotal: metrics.NewLabeledCounter(
			"collects_total",
			"Total collection runs by source",
			[]string{"source"},
		),
		collectErrors: metrics.NewLabeledCounter(
			"collect_errors_total",
			"Total collection errors by source",
			[]string{"source"},
		),
		httpDuration: metrics.NewHistogram(
			"http_request_duration_seconds",
			"HTTP request latency",
		),
		// APIBuckets, not the default: a cycle polling two registries behind a
		// 30s per-request timeout would land every sample in DefaultBuckets' +Inf.
		collectDuration: metrics.NewHistogram(
			"collect_duration_seconds",
			"Collection cycle duration",
			metrics.WithBuckets(metrics.APIBuckets()),
		),
		// _total on a gauge is a deliberate deviation: the name is the published
		// series, and metrics/v4's Counter has no Set, so the per-cycle
		// replacement SetImage performs cannot be a counter.
		imagePulls: metrics.NewLabeledGauge(
			"image_pulls_total",
			"Total pull count per image",
			[]string{"registry", "owner", "repo"},
		),
		imageTags: metrics.NewLabeledGauge(
			"image_tags",
			"Number of tags per image",
			[]string{"registry", "owner", "repo"},
		),
	}
	m.registry.MustRegister(
		m.httpRequests,
		m.collectsTotal,
		m.collectErrors,
		m.imagePulls,
		m.imageTags,
		m.httpDuration,
		m.collectDuration,
	)
	return m
}

// MintCollectSources pre-mints the two per-source collect counters at zero.
func (m *Metrics) MintCollectSources(sources []string) {
	for _, source := range sources {
		m.collectsTotal.Add(0, source)
		m.collectErrors.Add(0, source)
	}
}

// RecordCollect records an invoked source and whether it failed.
func (m *Metrics) RecordCollect(source string, failed bool) {
	m.collectsTotal.Inc(source)
	if failed {
		m.collectErrors.Inc(source)
	}
}

// ObserveCollectDuration records one collection cycle duration.
func (m *Metrics) ObserveCollectDuration(d time.Duration) {
	m.collectDuration.Observe(d.Seconds())
}

// ImageMetric holds per-image gauge data set after each collect cycle. A nil
// Tags means no tag count was measured this cycle and clears any tags series
// the image had; a pointed-to zero is a measured zero and is emitted.
type ImageMetric struct {
	Registry string // "dockerhub" or "ghcr"
	Owner    string
	Repo     string
	Pulls    int64
	Tags     *int
}

// SetImage replaces the image gauge data for one collect cycle. Current values
// are Set in place and departed series dropped one by one rather than Reset+Set,
// so a series present in both this cycle and the last is never missing from a
// concurrent scrape: it reads either value. A scrape overlapping the update may
// still straddle it, reading some series one cycle stale, and where a cycle
// replaces the whole set it can miss the family entirely.
func (m *Metrics) SetImage(images []ImageMetric) {
	m.setMu.Lock()
	defer m.setMu.Unlock()

	pulls := make(map[[3]string]bool, len(images))
	tags := make(map[[3]string]bool, len(images))
	for _, image := range images {
		key := [3]string{image.Registry, image.Owner, image.Repo}
		m.imagePulls.Set(float64(image.Pulls), image.Registry, image.Owner, image.Repo)
		pulls[key] = true
		if image.Tags != nil {
			m.imageTags.Set(float64(*image.Tags), image.Registry, image.Owner, image.Repo)
			tags[key] = true
		}
	}
	for key := range m.prevPulls {
		if !pulls[key] {
			m.imagePulls.Delete(key[0], key[1], key[2])
		}
	}
	for key := range m.prevTags {
		if !tags[key] {
			m.imageTags.Delete(key[0], key[1], key[2])
		}
	}
	m.prevPulls, m.prevTags = pulls, tags
}

// RecordHTTP records one HTTP request.
func (m *Metrics) RecordHTTP(rm webhttp.RequestMetric) {
	metrics.RecordHTTP(m.httpRequests, m.httpDuration, rm.Latency, rm.Method, rm.Path, strconv.Itoa(rm.Status))
}

// Handler returns an HTTP handler serving Prometheus text format.
func (m *Metrics) Handler() http.HandlerFunc {
	return m.registry.Handler()
}
