// Package obs holds registry-stats' observability surface.
package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/cplieger/metrics/v4"
	"github.com/cplieger/webhttp/v2"
)

// Metrics records and serves registry-stats metrics. Construct via New; the
// zero value is not usable. Recording and serving are safe for concurrent use:
// the metric values synchronize themselves. SetImage is the one exception -- it
// owns the per-cycle label bookkeeping and takes a single writer, which is the
// collect loop.
type Metrics struct {
	registry        *metrics.Registry
	httpRequests    *metrics.LabeledCounter
	collectsTotal   *metrics.LabeledCounter
	collectErrors   *metrics.LabeledCounter
	httpDuration    *metrics.Histogram
	collectDuration *metrics.Histogram
	imagePulls      *metrics.LabeledGauge
	prevPulls       map[[3]string]bool
}

// New constructs an isolated metrics registry.
// The names below are the published series, read as literal text by every
// shipped consumer TestMetricsHandler_publishesSeriesUsedByShippedConsumers
// enumerates; it fails on a rename that strands one. CONTRIBUTING.md carries
// only the rename-proof registrystats_* glob and stays outside that list.
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
			"Failed collection runs by source",
			[]string{"source"},
		),
		httpDuration: metrics.NewHistogram(
			"http_request_duration_seconds",
			"HTTP request latency",
		),
		// GHCR requests are paced 2-5s apart and both registry readers are sequential,
		// so these bounds span sub-second cycles through one default poll interval.
		collectDuration: metrics.NewHistogram(
			"collect_duration_seconds",
			"Collection cycle duration",
			metrics.WithBuckets([]float64{0.5, 1, 5, 15, 60, 300, 900, 3600}),
		),
		// _total on a gauge is a deliberate deviation: the name is the published
		// series, and the mirrored registry total can be restated downward, which
		// no counter may do (RegistryStatsPullCountRegressed alerts on it).
		imagePulls: metrics.NewLabeledGauge(
			"image_pulls_total",
			"Total pull count per image",
			[]string{"registry", "owner", "repo"},
		),
	}
	m.registry.MustRegister(
		m.httpRequests,
		m.collectsTotal,
		m.collectErrors,
		m.imagePulls,
		m.httpDuration,
		m.collectDuration,
	)
	return m
}

// MintCollectSources pre-mints the two per-source collect counters at zero, so
// each configured source has a series from process start and a PromQL
// increase() over its first failure has an earlier sample to subtract from.
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

// ImageMetric holds per-image gauge data set after each collect cycle.
// Registry, Owner and Repo must be distinct after Prometheus label
// sanitization: metrics/v4 replaces invalid UTF-8 in a label value,
// so two triples that differ only in bytes that are not valid UTF-8 would
// share one emitted series and the per-cycle diff would delete it.
// Producers guarantee this via urlsafe.
type ImageMetric struct {
	Registry string // "dockerhub" or "ghcr"
	Owner    string
	Repo     string
	Pulls    int64
}

// SetImage replaces the image gauge data for one collect cycle.
// images is the whole population this cycle MEASURED: every key absent from it is retired, so an absent series means
// this cycle did not measure that image - either it is gone, or the source could not finish enumerating its population.
// Current values are Set in place and departed series Deleted one by one rather than Reset+Set, so a series present in
// both cycles is never missing from a concurrent scrape; an overlapping scrape may still read one cycle stale.
func (m *Metrics) SetImage(images []ImageMetric) {
	pulls := make(map[[3]string]bool, len(images))
	for _, image := range images {
		key := [3]string{image.Registry, image.Owner, image.Repo}
		m.imagePulls.Set(float64(image.Pulls), image.Registry, image.Owner, image.Repo)
		pulls[key] = true
	}
	for key := range m.prevPulls {
		if !pulls[key] {
			m.imagePulls.Delete(key[0], key[1], key[2])
		}
	}
	m.prevPulls = pulls
}

// RecordHTTP records one HTTP request.
func (m *Metrics) RecordHTTP(rm webhttp.RequestMetric) {
	metrics.RecordHTTP(m.httpRequests, m.httpDuration, rm.Latency, rm.Method, rm.Path, strconv.Itoa(rm.Status))
}

// Handler returns an HTTP handler serving Prometheus text format.
func (m *Metrics) Handler() http.HandlerFunc {
	return m.registry.Handler()
}
