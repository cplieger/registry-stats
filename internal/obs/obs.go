// Package obs holds registry-stats' observability surface.
package obs

import (
	"net/http"
	"time"

	"github.com/cplieger/metrics/v4"
	"github.com/cplieger/registry-stats/internal/registry"
)

// Metrics records and serves registry-stats metrics. Construct via New; the
// zero value is not usable. Recording and serving are safe for concurrent use:
// the metric values synchronize themselves. The Set methods are the exception
// -- they own the per-cycle label bookkeeping and take a single writer, which
// is the collect loop.
type Metrics struct {
	registry        *metrics.Registry
	collectsTotal   *metrics.LabeledCounter
	collectErrors   *metrics.LabeledCounter
	collectDuration *metrics.Histogram
	imagePulls      *metrics.LabeledGauge
	lastPushed      *metrics.LabeledGauge
	repoUpdated     *metrics.LabeledGauge
	present         *metrics.LabeledGauge
	lastSuccess     *metrics.LabeledGauge
	complete        *metrics.LabeledGauge
	omitted         *metrics.LabeledGauge
	prevPulls       map[imageKey]bool
	prevPushed      map[imageKey]bool
	prevUpdated     map[imageKey]bool
	prevPresent     map[imageKey]bool
}

// imageKey is a per-image series identity: the registry label, owner, repo.
type imageKey [3]string

// Label names the per-image and per-source families share.
const (
	labelSource   = "source"
	labelRegistry = "registry"
	labelOwner    = "owner"
	labelRepo     = "repo"
)

// New constructs an isolated metrics registry.
// The names below are the series every shipped document names as literal
// text; TestMetricsHandler_publishesSeriesUsedByShippedConsumers fails on
// a rename that strands one.
func New() *Metrics {
	m := &Metrics{
		registry: metrics.NewRegistry("registrystats"),
		collectsTotal: metrics.NewLabeledCounter(
			"collects_total",
			"Total collection runs by source",
			[]string{labelSource},
		),
		collectErrors: metrics.NewLabeledCounter(
			"collect_errors_total",
			"Failed collection runs by source",
			[]string{labelSource},
		),
		// Large owner/* walks can legitimately reach the top buckets.
		collectDuration: metrics.NewHistogram(
			"collect_duration_seconds",
			"Collection cycle duration",
			metrics.WithBuckets([]float64{0.5, 1, 5, 15, 60, 300, 900, 3600, 10800, 18000}),
		),
		// _total on a gauge is a deliberate deviation: the name is the published
		// series, and the mirrored registry total can be restated downward, which
		// no counter may do (RegistryStatsPullCountRegressed alerts on it).
		imagePulls: metrics.NewLabeledGauge(
			"image_pulls_total",
			"Total pull count per image",
			[]string{labelRegistry, labelOwner, labelRepo},
		),
		lastPushed: metrics.NewLabeledGauge(
			"image_last_pushed_timestamp_seconds",
			"Last push time per image as the registry reports it, GHCR only",
			[]string{labelRegistry, labelOwner, labelRepo},
		),
		repoUpdated: metrics.NewLabeledGauge(
			"dockerhub_repository_updated_timestamp_seconds",
			"Docker Hub repository last_updated time",
			[]string{labelOwner, labelRepo},
		),
		present: metrics.NewLabeledGauge(
			"image_present",
			"1 when the source read the image this cycle, 0 when it definitively reported it absent",
			[]string{labelSource, labelOwner, labelRepo},
		),
		lastSuccess: metrics.NewLabeledGauge(
			"collect_last_success_timestamp_seconds",
			"Time of the last collection per source in which most reads got an answer",
			[]string{labelSource},
		),
		complete: metrics.NewLabeledGauge(
			"collect_complete",
			"1 when every listing and image read of the source's last cycle was definitive",
			[]string{labelSource},
		),
		omitted: metrics.NewLabeledGauge(
			"image_details_omitted",
			"Images past the detail cap, which get no push time or presence series",
			[]string{labelSource},
		),
	}
	m.registry.MustRegister(
		m.collectsTotal,
		m.collectErrors,
		m.imagePulls,
		m.collectDuration,
		m.lastPushed,
		m.repoUpdated,
		m.present,
		m.lastSuccess,
		m.complete,
		m.omitted,
	)
	return m
}

// MintCollectSources pre-mints the two per-source collect counters at zero, so
// each configured source has a series from process start: a scrape landing
// before that source's first failure records the zero, giving increase() an
// earlier sample. A first failure ahead of the first scrape has none, which is
// why RegistryStatsSourceDegraded carries its process_start_time_seconds arm.
func (m *Metrics) MintCollectSources(sources []registry.ID) {
	for _, source := range sources {
		label := source.String()
		m.collectsTotal.Add(0, label)
		m.collectErrors.Add(0, label)
	}
}

// RecordCollect records an invoked source and whether it failed.
func (m *Metrics) RecordCollect(source registry.ID, failed bool) {
	label := source.String()
	m.collectsTotal.Inc(label)
	if failed {
		m.collectErrors.Inc(label)
	}
}

// ObserveCollectDuration records one collection cycle duration.
func (m *Metrics) ObserveCollectDuration(d time.Duration) {
	m.collectDuration.Observe(d.Seconds())
}

// ImageMetric holds per-image gauge data set after each collect cycle.
// Image label triples must remain distinct after metrics/v4 normalizes label values:
// metrics/v4 replaces invalid UTF-8 in a label value, so two triples that differ only in bytes that are not valid UTF-8 would
// share one emitted series and the per-cycle diff would delete it.
// Producers guarantee this via urlsafe.
// LastPushed and Updated publish only when Detailed is set and they are
// non-zero.
type ImageMetric struct {
	LastPushed time.Time
	Updated    time.Time
	Owner      string
	Repo       string
	Pulls      int64
	Registry   registry.ID
	// Detailed marks an image inside the detail cap.
	Detailed bool
}

// SetImage replaces the per-image gauges for one collect cycle. images is the
// whole population this cycle MEASURED: every key absent from it is retired with
// all of its series, so an absent series means
// this cycle did not measure that image (RegistryStatsCollectionIncomplete in
// alerts/logql.yaml names the causes). Values are Set in place and departed
// series Deleted one by one, so a series present in both cycles never vanishes
// from a concurrent scrape; an overlapping scrape may still read one cycle stale.
func (m *Metrics) SetImage(images []ImageMetric) {
	pulls := make(map[imageKey]bool, len(images))
	pushed := make(map[imageKey]bool)
	updated := make(map[imageKey]bool)
	for _, image := range images {
		label := image.Registry.String()
		key := imageKey{label, image.Owner, image.Repo}
		m.imagePulls.Set(float64(image.Pulls), label, image.Owner, image.Repo)
		pulls[key] = true
		if !image.Detailed {
			continue
		}
		if !image.LastPushed.IsZero() {
			m.lastPushed.Set(unixSeconds(image.LastPushed), label, image.Owner, image.Repo)
			pushed[key] = true
		}
		if image.Registry == registry.DockerHub && !image.Updated.IsZero() {
			m.repoUpdated.Set(unixSeconds(image.Updated), image.Owner, image.Repo)
			updated[key] = true
		}
	}
	retire(m.prevPulls, pulls, func(k imageKey) { m.imagePulls.Delete(k[0], k[1], k[2]) })
	retire(m.prevPushed, pushed, func(k imageKey) { m.lastPushed.Delete(k[0], k[1], k[2]) })
	retire(m.prevUpdated, updated, func(k imageKey) { m.repoUpdated.Delete(k[1], k[2]) })
	m.prevPulls, m.prevPushed, m.prevUpdated = pulls, pushed, updated
}

// Presence is one detailed image's presence on a source whose configuration
// covers it.
type Presence struct {
	Owner   string
	Repo    string
	Source  registry.ID
	Present bool
}

// SetPresence replaces the presence gauges for one cycle; a name missing from
// presence has no series, which means "not covered" or "not read definitively".
func (m *Metrics) SetPresence(presence []Presence) {
	current := make(map[imageKey]bool, len(presence))
	for _, p := range presence {
		label := p.Source.String()
		value := 0.0
		if p.Present {
			value = 1
		}
		m.present.Set(value, label, p.Owner, p.Repo)
		current[imageKey{label, p.Owner, p.Repo}] = true
	}
	retire(m.prevPresent, current, func(k imageKey) { m.present.Delete(k[0], k[1], k[2]) })
	m.prevPresent = current
}

// SourceCycle is one active source's verdict for a cycle.
type SourceCycle struct {
	// Omitted counts the source's measured images past the detail cap.
	Omitted int
	Source  registry.ID
	// Answered stamps the last success; collect.collectSource owns the rule.
	// Complete means every listing and image read of the cycle was definitive.
	Answered bool
	Complete bool
}

// SetSources publishes each active source's completeness and omitted count,
// and stamps at as the last success of every answered one. Callers pass only
// an uncancelled cycle.
func (m *Metrics) SetSources(sources []SourceCycle, at time.Time) {
	for _, s := range sources {
		label := s.Source.String()
		complete := 0.0
		if s.Complete {
			complete = 1
		}
		m.complete.Set(complete, label)
		m.omitted.Set(float64(s.Omitted), label)
		if s.Answered {
			m.lastSuccess.Set(unixSeconds(at), label)
		}
	}
}

// retire calls drop for every key in prev that current no longer holds.
func retire(prev, current map[imageKey]bool, drop func(imageKey)) {
	for key := range prev {
		if !current[key] {
			drop(key)
		}
	}
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}

// Handler returns an HTTP handler serving Prometheus text format.
func (m *Metrics) Handler() http.HandlerFunc {
	return m.registry.Handler()
}
