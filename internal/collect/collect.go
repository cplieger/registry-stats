// Package collect is the registry-stats collection orchestrator: it invokes
// each registry Source in turn and returns the combined per-image records.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

// Source collects registry-specific statistics. Source must return a known
// registry.ID; registry.Collection defines the returned cycle accounting.
// Every ref is already canonical and distinct for the registry Source names --
// lower case, accepted by internal/config's urlsafe gate, and deduplicated on
// the registry.RepoRef key -- so Collect may interpolate Owner and Repo into a
// request URL without re-checking their shape.
// Collect still owns the shape of the request it builds: Repo is "*" for an
// owner-wide ref, which it expands into that owner's published repositories
// rather than requesting by name, and a GHCR Repo is the decoded package name,
// which may carry '/' path elements and needs url.PathEscape (see
// urlsafe.PackageName).
// A successful Collect returns entries whose Owner and Repo are non-empty: obs
// keys the published gauge on that pair, and every construction gate rejects
// the empty string.
type Source interface {
	Source() registry.ID
	Collect(ctx context.Context, refs []registry.RepoRef) registry.Collection
}

// SourceRefs binds a selected source to the canonical refs it will collect.
type SourceRefs struct {
	Source Source
	Refs   []registry.RepoRef
}

// Options configures a single Run. Metrics and Logger are required, and no two
// Sources may report the same registry.ID: Run keys the metric label on it. An
// empty Sources is a supported cycle: Run collects nothing and warns that no
// repos are configured.
type Options struct {
	Metrics *obs.Metrics
	Logger  *slog.Logger
	Sources []SourceRefs
}

// Run orchestrates a single collection cycle: it invokes each source's
// Collect, stamps each entry with its source's registry label, and returns the
// combined per-image records.
// The caller owns the health marker and derives it from the returned
// set. A cancelled cycle stops early and returns what it collected,
// with no error, so a caller that publishes the set must check
// ctx.Err() first.
func Run(ctx context.Context, opts Options) []obs.ImageMetric {
	var images []obs.ImageMetric

	logger := opts.Logger

	start := time.Now()
	logger.Info("starting collection")
	var degraded bool

	for _, sourceRefs := range opts.Sources {
		if ctx.Err() != nil {
			// Stop before invoking a further source on a dead context: an
			// advance in collects_total is the completed-cycle signal
			// RegistryStatsCollectStalled reads. A source cancelled after
			// this check still mints its sample; only a shutdown can do
			// that, so the stall rule's 6h window is unaffected.
			break
		}
		srcImages, srcHealthy := collectSource(ctx, opts.Metrics, logger, sourceRefs.Source, sourceRefs.Refs)
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

	opts.Metrics.ObserveCollectDuration(time.Since(start))

	if len(images) == 0 {
		switch {
		case len(opts.Sources) == 0:
			// RegistryStatsConfigRejected (alerts/logql.yaml) matches the text of the
			// "no repos configured" WARN; reword it there and here together.
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

// collectSource invokes one source's Collect and stamps its entries with the
// source's registry label regardless of health, so a degraded-but-nonempty
// source still contributes its data. A cancelled cycle is a stop rather than
// a collection failure: the source still mints its collects_total sample,
// but neither the error counter nor the unhealthy log line fires.
func collectSource(
	ctx context.Context,
	m *obs.Metrics,
	logger *slog.Logger,
	src Source,
	refs []registry.RepoRef,
) (images []obs.ImageMetric, srcHealthy bool) {
	collection := src.Collect(ctx, refs)
	// Unhealthy when a wildcard owner listing wholly failed, or when more
	// than half the fetch attempts yielded no entry.
	srcHealthy = !collection.ListingFailed && collection.Fetched*2 >= collection.Attempted
	source := src.Source()
	failed := !srcHealthy && ctx.Err() == nil
	if failed {
		logger.Warn("source reported unhealthy",
			"source", source.String(), "succeeded", collection.Fetched, "attempted", collection.Attempted,
			"listing_failed", collection.ListingFailed)
	}
	m.RecordCollect(source, failed)

	images = make([]obs.ImageMetric, 0, len(collection.Entries))
	for _, e := range collection.Entries {
		images = append(images, obs.ImageMetric{
			Registry: source,
			Owner:    e.Owner,
			Repo:     e.Repo,
			Pulls:    e.Pulls,
		})
	}
	return images, srcHealthy
}
