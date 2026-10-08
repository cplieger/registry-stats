// Package collect is the registry-stats collection orchestrator: it invokes
// each registry Source in turn and returns the combined per-image records.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/registry-stats/internal/obs"
	"github.com/cplieger/registry-stats/internal/registry"
)

// Source is one registry, named by a known registry.ID. Every ref it receives is
// canonical and distinct (lower case, passed by internal/config's urlsafe gate,
// deduplicated on registry.RepoRef), so it may interpolate Owner and Repo into a
// URL unchecked; Repo "*" is an owner-wide ref to expand, and a GHCR Repo is a
// decoded package name that needs url.PathEscape (see urlsafe.PackageName).
// Collect returns entries with non-empty Owner and Repo, the pair obs keys on.
// ReadDetail logs its own failures; an error matching httpx.ErrRateLimited stops
// the source's detail reads for the cycle.
type Source interface {
	Source() registry.ID
	Collect(ctx context.Context, refs []registry.RepoRef) registry.Collection
	ReadDetail(ctx context.Context, ref registry.RepoRef) (registry.Detail, error)
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

// Cycle is one collection cycle's outcome: the per-image records, the
// presence of every detailed name on each source that covers it, and each
// invoked source's verdict.
type Cycle struct {
	Finished time.Time
	Images   []obs.ImageMetric
	Presence []obs.Presence
	Sources  []obs.SourceCycle
}

// Run orchestrates a single collection cycle: it invokes each source's
// Collect, stamps each entry with its source's registry label, and returns the
// combined per-image records with the cycle's accounting.
// The caller owns the health marker and derives it from the returned
// images. A cancelled cycle stops early and returns what it collected,
// with no error, so a caller that publishes the cycle must check
// ctx.Err() first.
func Run(ctx context.Context, opts Options) Cycle {
	var (
		images  []obs.ImageMetric
		results []sourceResult
	)

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
		result := collectSource(ctx, opts.Metrics, logger, sourceRefs.Source, sourceRefs.Refs)
		if !result.healthy {
			degraded = true
		}
		results = append(results, result)
		images = append(images, result.images...)
	}

	if ctx.Err() != nil {
		logger.Warn("collection interrupted",
			"error", ctx.Err(), "images", len(images))
		return Cycle{Images: images}
	}

	cycle := account(results)
	cycle.Finished = time.Now()

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
		return cycle
	}

	if degraded {
		logger.Warn("partial collection failure", "images", len(images))
	}

	logger.Info("collection complete",
		"images", len(images),
		"duration", time.Since(start).Round(time.Millisecond))

	return cycle
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
) sourceResult {
	collection := src.Collect(ctx, refs)
	// Unhealthy when a wildcard owner listing wholly failed, or when more
	// than half the fetch attempts yielded no entry.
	srcHealthy := !collection.ListingFailed && collection.Fetched*2 >= collection.Attempted
	// A 404 produced no entry, so health counts it as a miss; it still answered
	// the read, so it stamps the last success.
	answered := !collection.ListingFailed && collection.Definitive*2 >= collection.Attempted
	source := src.Source()
	failed := !srcHealthy && ctx.Err() == nil
	if failed {
		logger.Warn("source reported unhealthy",
			"source", source.String(), "succeeded", collection.Fetched, "attempted", collection.Attempted,
			"listing_failed", collection.ListingFailed)
	}
	m.RecordCollect(source, failed)

	images := make([]obs.ImageMetric, 0, len(collection.Entries))
	for _, e := range collection.Entries {
		images = append(images, obs.ImageMetric{
			Registry:   source,
			Owner:      e.Owner,
			Repo:       e.Repo,
			Pulls:      e.Pulls,
			LastPushed: e.LastPushed,
			Updated:    e.Updated,
		})
	}
	return sourceResult{
		refs:       refs,
		collection: collection,
		images:     images,
		source:     source,
		healthy:    srcHealthy,
		answered:   answered,
	}
}
