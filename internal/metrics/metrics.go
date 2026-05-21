// Package metrics provides hand-rolled Prometheus text format metrics.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var startTime = time.Now()

// LabeledCounter tracks counts per label combination.
type LabeledCounter struct {
	vals   map[string]*atomic.Int64
	name   string
	help   string
	labels []string
	mu     sync.RWMutex
}

func (lc *LabeledCounter) Inc(labelVals ...string) {
	key := strings.Join(labelVals, "\x00")
	lc.mu.RLock()
	v, ok := lc.vals[key]
	lc.mu.RUnlock()
	if ok {
		v.Add(1)
		return
	}
	lc.mu.Lock()
	if v, ok = lc.vals[key]; ok {
		lc.mu.Unlock()
		v.Add(1)
		return
	}
	v = &atomic.Int64{}
	v.Store(1)
	lc.vals[key] = v
	lc.mu.Unlock()
}

// Histogram tracks a distribution using cumulative buckets and atomic CAS for sum.
type Histogram struct {
	name    string
	help    string
	sumBits atomic.Uint64
	count   atomic.Int64
	buckets [len(defaultBuckets) + 1]atomic.Int64
}

var defaultBuckets = [8]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}

func (h *Histogram) Observe(seconds float64) {
	for {
		old := h.sumBits.Load()
		newF := math.Float64frombits(old) + seconds
		if h.sumBits.CompareAndSwap(old, math.Float64bits(newF)) {
			break
		}
	}
	h.count.Add(1)
	for i, bound := range defaultBuckets {
		if seconds <= bound {
			for j := i; j < len(defaultBuckets); j++ {
				h.buckets[j].Add(1)
			}
			break
		}
	}
	h.buckets[len(defaultBuckets)].Add(1)
}

// Exported metrics.
var (
	HTTPRequests = &LabeledCounter{
		name:   "registrystats_http_requests_total",
		help:   "Total HTTP requests",
		labels: []string{"method", "path", "status"},
		vals:   make(map[string]*atomic.Int64),
	}
	CollectsTotal = &LabeledCounter{
		name:   "registrystats_collects_total",
		help:   "Total collection runs by source",
		labels: []string{"source"},
		vals:   make(map[string]*atomic.Int64),
	}
	CollectErrors = &LabeledCounter{
		name:   "registrystats_collect_errors_total",
		help:   "Total collection errors by source",
		labels: []string{"source"},
		vals:   make(map[string]*atomic.Int64),
	}
	HTTPDuration = &Histogram{
		name: "registrystats_http_request_duration_seconds",
		help: "HTTP request latency",
	}
	CollectDuration = &Histogram{
		name: "registrystats_collect_duration_seconds",
		help: "Collection cycle duration",
	}
)

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		writeLabeledCounter(&b, HTTPRequests)
		writeLabeledCounter(&b, CollectsTotal)
		writeLabeledCounter(&b, CollectErrors)
		writeHistogram(&b, HTTPDuration)
		writeHistogram(&b, CollectDuration)
		writeImageMetrics(&b)
		writeProcessMetrics(&b)
		w.Write([]byte(b.String())) //nolint:errcheck // ResponseWriter writes never return an actionable error
	}
}

// ImageMetric holds per-image gauge data set after each collect cycle.
type ImageMetric struct {
	Registry string // "dockerhub" or "ghcr"
	Owner    string
	Repo     string
	Pulls    int64
	Tags     int
}

var (
	imageMetricsMu sync.RWMutex
	imageMetrics   []ImageMetric
)

// SetImageMetrics replaces the current image gauge data atomically.
func SetImageMetrics(images []ImageMetric) {
	imageMetricsMu.Lock()
	imageMetrics = images
	imageMetricsMu.Unlock()
}

func writeImageMetrics(b *strings.Builder) {
	imageMetricsMu.RLock()
	imgs := imageMetrics
	imageMetricsMu.RUnlock()
	if len(imgs) == 0 {
		return
	}
	b.WriteString("# HELP registrystats_image_pulls_total Total pull count per image\n# TYPE registrystats_image_pulls_total gauge\n")
	for _, m := range imgs {
		fmt.Fprintf(b, "registrystats_image_pulls_total{registry=%q,owner=%q,repo=%q} %d\n", m.Registry, m.Owner, m.Repo, m.Pulls)
	}
	hasTags := false
	for _, m := range imgs {
		if m.Tags > 0 {
			hasTags = true
			break
		}
	}
	if hasTags {
		b.WriteString("# HELP registrystats_image_tags Number of tags per image\n# TYPE registrystats_image_tags gauge\n")
		for _, m := range imgs {
			if m.Tags > 0 {
				fmt.Fprintf(b, "registrystats_image_tags{registry=%q,owner=%q,repo=%q} %d\n", m.Registry, m.Owner, m.Repo, m.Tags)
			}
		}
	}
}

func writeLabeledCounter(b *strings.Builder, lc *LabeledCounter) {
	lc.mu.RLock()
	keys := make([]string, 0, len(lc.vals))
	for k := range lc.vals {
		keys = append(keys, k)
	}
	lc.mu.RUnlock()
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", lc.name, lc.help, lc.name)
	for _, key := range keys {
		lc.mu.RLock()
		v := lc.vals[key]
		lc.mu.RUnlock()
		parts := strings.Split(key, "\x00")
		type lp struct {
			k, v string
		}
		pairs := make([]lp, len(lc.labels))
		for i, l := range lc.labels {
			pairs[i] = lp{l, parts[i]}
		}
		sort.Slice(pairs, func(a, c int) bool { return pairs[a].k < pairs[c].k })
		var labelStr strings.Builder
		for i, p := range pairs {
			if i > 0 {
				labelStr.WriteByte(',')
			}
			fmt.Fprintf(&labelStr, "%s=%q", p.k, p.v)
		}
		fmt.Fprintf(b, "%s{%s} %d\n", lc.name, labelStr.String(), v.Load())
	}
}

func writeHistogram(b *strings.Builder, h *Histogram) {
	sum := math.Float64frombits(h.sumBits.Load())
	count := h.count.Load()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	for i, bound := range defaultBuckets {
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", h.name, formatBound(bound), h.buckets[i].Load())
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", h.name, h.buckets[len(defaultBuckets)].Load())
	fmt.Fprintf(b, "%s_sum %.6f\n", h.name, sum)
	fmt.Fprintf(b, "%s_count %d\n", h.name, count)
}

func formatBound(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func writeProcessMetrics(b *strings.Builder) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Fprintf(b, "# HELP process_goroutines Number of goroutines\n# TYPE process_goroutines gauge\nprocess_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(b, "# HELP process_heap_bytes Heap memory in use\n# TYPE process_heap_bytes gauge\nprocess_heap_bytes %d\n", m.HeapAlloc)
	fmt.Fprintf(b, "# HELP process_gc_pause_seconds_total Total GC pause time\n# TYPE process_gc_pause_seconds_total counter\nprocess_gc_pause_seconds_total %.6f\n", float64(m.PauseTotalNs)/1e9)
	fmt.Fprintf(b, "# HELP process_uptime_seconds Process uptime\n# TYPE process_uptime_seconds gauge\nprocess_uptime_seconds %.3f\n", time.Since(startTime).Seconds())
}
