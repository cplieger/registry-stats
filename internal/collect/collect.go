// Package collect is the registry-stats collection orchestrator: it loops
// over a set of Source implementations, stamps each source's flat
// []registry.Entry output with its registry label, and returns the
// combined per-image metric records.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

// Source collects registry-specific statistics for a list of refs.
// Implementations return flat per-image registry.Entry records; Run stamps
// each with the source's registry label for the metric surface. attempted
// counts refs the source tried to fetch (including failures), reported
// beside the served count when a source degrades. listingFailed reports a
// wildcard owner listing that failed without yielding any usable refs.
//
// Source() must return a known registry.ID, never Unknown, since Run
// derives the metric and log label from it; a successful Collect returns
// entries whose Owner and Repo label components are non-empty.
type Source interface {
	Source() registry.ID
	Collect(
		ctx context.Context,
		refs []registry.RepoRef,
	) (entries []registry.Entry, attempted int, listingFailed bool)
}

// Options configures a single Run. Metrics is required. RefsFor maps each
// source's registry ID to the []registry.RepoRef it should fetch. A nil Logger
// falls back to slog.Default.
type Options struct {
	Metrics *obs.Metrics
	Logger  *slog.Logger
	RefsFor func(registry.ID) []registry.RepoRef
	Sources []Source
}

// Run orchestrates a single collection cycle: invokes each source's
// Collect (skipping sources whose ref slice is empty), stamps each entry
// with its source's registry label, and returns the combined per-image
// records. The caller owns the health marker and derives it from the
// returned set (see main.runCollect).
func Run(ctx context.Context, opts Options) (images []obs.ImageMetric) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	start := time.Now()
	logger.Info("starting collection")
	degraded := false
	invokedAnySource := false

	for _, src := range opts.Sources {
		if ctx.Err() != nil {
			// A dead context must not mint a per-source cycle that performs
			// no work: collects_total is the denominator RegistryStatsCollectStalled
			// reads.
			break
		}
		refs := refsFor(opts, src.Source())
		if len(refs) == 0 {
			continue
		}
		invokedAnySource = true
		srcImages, srcHealthy := collectSource(ctx, opts.Metrics, logger, src, refs)
		if !srcHealthy {
			degraded = true
		}
		images = append(images, srcImages...)
	}

	if ctx.Err() != nil {
		logger.Warn("collection interrupted",
			"error", ctx.Err(), "images", len(images))
		return images
	}

	if len(images) == 0 {
		switch {
		case !invokedAnySource:
			logger.Warn("no repos configured")
		case degraded:
			logger.Error("no images collected, at least one source failed")
		default:
			logger.Warn("no images found for the configured refs")
		}
		return images
	}

	if degraded {
		logger.Warn("partial collection failure", "images", len(images))
	}

	logger.Info("collection complete",
		"images", len(images),
		"duration", time.Since(start).Round(time.Millisecond))

	return images
}

// collectSource invokes a single source's Collect and stamps its entries
// with the source's registry label. An unhealthy source bumps the
// per-source error metric; entries are stamped regardless of health so a
// degraded-but-nonempty source still contributes its data. A cancelled
// cycle is a stop rather than a collection failure, so it moves neither
// the error counter nor the record.
func collectSource(
	ctx context.Context,
	m *obs.Metrics,
	logger *slog.Logger,
	src Source,
	refs []registry.RepoRef,
) (images []obs.ImageMetric, srcHealthy bool) {
	entries, attempted, listingFailed := src.Collect(ctx, refs)
	srcHealthy = !listingFailed && len(entries)*2 >= attempted
	label := src.Source().String()
	failed := !srcHealthy && ctx.Err() == nil
	if failed {
		logger.Warn("source reported unhealthy",
			"source", label, "succeeded", len(entries), "attempted", attempted)
	}
	m.RecordCollect(label, failed)

	images = make([]obs.ImageMetric, 0, len(entries))
	for _, e := range entries {
		images = append(images, obs.ImageMetric{
			Registry: label,
			Owner:    e.Owner,
			Repo:     e.Repo,
			Pulls:    e.Pulls,
			Tags:     e.TagCount,
		})
	}
	return images, srcHealthy
}

// refsFor resolves refs for a given source, returning nil when
// opts.RefsFor is unset.
func refsFor(opts Options, source registry.ID) []registry.RepoRef {
	if opts.RefsFor == nil {
		return nil
	}
	return opts.RefsFor(source)
}
