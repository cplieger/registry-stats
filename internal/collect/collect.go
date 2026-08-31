// Package collect is the registry-stats collection orchestrator. It
// loops over a set of Source implementations, stamps each
// source's flat []registry.Entry output with its registry label,
// and returns the combined per-image metric records.
//
// The orchestrator is deliberately tiny: each source owns its own
// *http.Client, retry options, logging, and pacing. Run's job is the
// orchestration layer above that — invoke each configured source, keep
// the per-source health accounting, and surface partial-degradation
// warnings without failing the whole cycle.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/registry-stats/v2/internal/obs"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

// Source collects registry-specific statistics for a list of refs — the
// one seam this orchestrator drives, declared here at its consumer.
// Implementations return flat per-image registry.Entry records; Run stamps
// each with the source's registry label for the metric surface. attempted
// counts refs the source tried to fetch (including failures), which Run
// reports beside the served count when a source degrades; healthy is true
// only when the source met its per-source health criteria.
//
// Two obligations an implementation carries, decided here because this is
// where they are consumed: Source() returns a known registry.ID, never
// Unknown, since Run derives the metric and log label from it; and a
// successful Collect returns entries whose Owner and Repo label components
// are non-empty.
type Source interface {
	Source() registry.ID
	Collect(
		ctx context.Context,
		refs []registry.RepoRef,
	) (entries []registry.Entry, attempted int, healthy bool)
}

// Options configures a single Run. Sources are the registry clients
// that actually fetch data; RefsFor maps each source's on-wire name
// (Source().String()) to the
// []registry.RepoRef it should fetch, allowing the orchestrator to stay
// agnostic of the per-registry config slice layout. A nil Logger falls
// back to slog.Default.
type Options struct {
	Logger  *slog.Logger
	RefsFor func(name string) []registry.RepoRef
	Sources []Source
}

// Run orchestrates a single collection cycle: it invokes each source's
// Collect (skipping sources whose ref slice is empty so empty-config paths
// stay zero-cost), stamps each entry with its source's registry label, and
// returns the combined per-image records. The caller owns the health
// marker and derives it from the returned set (see main.runCollect).
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
		refs := refsFor(opts, src.Source().String())
		if len(refs) == 0 {
			continue
		}
		invokedAnySource = true
		srcImages, srcHealthy := collectSource(ctx, logger, src, refs)
		if !srcHealthy {
			degraded = true
		}
		images = append(images, srcImages...)
	}

	if len(images) == 0 {
		// Cancellation first: a cycle can be both cancelled and degraded, and
		// the interruption is the fact that explains the rest.
		switch {
		case ctx.Err() != nil:
			logger.Warn("collection interrupted", "error", ctx.Err())
		case !invokedAnySource:
			logger.Warn("no repos configured")
		case degraded:
			logger.Error("all collections failed")
		default:
			logger.Warn("no images found for the configured refs")
		}
		return images
	}

	if degraded {
		logger.Warn("partial collection failure, serving available data",
			"images", len(images))
	}

	logger.Info("collection complete",
		"images", len(images),
		"duration", time.Since(start).Round(time.Millisecond))

	return images
}

// collectSource invokes a single source's Collect and stamps its entries
// with the source's registry label, reporting whether the source met its
// health threshold. An unhealthy source gets a shortfall record under its
// own registry label and bumps the per-source error metric. Entries are
// stamped regardless of health so a degraded-but-nonempty source still
// contributes its data. A cancelled cycle is a stop rather than a
// collection failure, so it moves neither the error counter nor the record.
func collectSource(
	ctx context.Context,
	logger *slog.Logger,
	src Source,
	refs []registry.RepoRef,
) (images []obs.ImageMetric, srcHealthy bool) {
	entries, attempted, srcHealthy := src.Collect(ctx, refs)
	label := src.Source().String()
	// Count every invoked source as a collect run; collect_errors_total below is
	// the failed subset, so collect_errors_total / collects_total is a valid
	// per-source failure ratio.
	obs.CollectsTotal.Inc(label)
	if !srcHealthy && ctx.Err() == nil {
		logger.Warn("source reported unhealthy",
			"source", label, "succeeded", len(entries), "attempted", attempted)
		obs.CollectErrors.Inc(label)
	}

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

// refsFor resolves refs for a given source name, returning nil when
// opts.RefsFor is unset or the source has no refs. A nil RefsFor is
// equivalent to "no refs for any source", which short-circuits the
// whole loop — useful for orchestrator-only tests that pass canned
// entries via fake sources with baked-in state.
func refsFor(opts Options, name string) []registry.RepoRef {
	if opts.RefsFor == nil {
		return nil
	}
	return opts.RefsFor(name)
}
