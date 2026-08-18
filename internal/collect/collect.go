// Package collect is the registry-stats collection orchestrator. It
// loops over a set of Source implementations, stamps each
// source's flat []registry.Entry output with its registry label,
// and returns the combined per-image metric records plus a healthy flag
// so the caller can flip its healthcheck marker accordingly.
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
// one seam this orchestrator drives, declared here at its consumer (the
// old internal/api hub held it at arm's length from everyone who used it).
// Implementations return flat per-image registry.Entry records; Run stamps
// each with the source's registry label for the metric surface. attempted
// counts refs the source tried to fetch (including failures) so degraded
// detection can see the shortfall; healthy is true only when the source met
// its per-source health criteria.
//
// Source() returns the typed registry.ID — Run routes entries by it without
// a string compare, and derives the lowercase on-wire log label via its
// String(). The old interface also carried Name() with a prose invariant
// that Name() == Source().String(); both implementations defined Name as
// exactly that call, so the method carried zero information and the
// compiler could not enforce the invariant. Deriving the label from the one
// authoritative method deletes the drift surface (C11 side finding).
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
// back to slog.Default. Now is the clock used for the cycle-duration
// log (tests inject a deterministic clock; production passes time.Now).
type Options struct {
	Logger  *slog.Logger
	Now     func() time.Time
	RefsFor func(name string) []registry.RepoRef
	Sources []Source
}

// Run orchestrates a single collection cycle. It invokes each source's
// Collect (skipping sources whose ref slice is empty so empty-config
// paths stay zero-cost), stamps each entry with its source's registry
// label, and returns the combined per-image records plus a healthy flag.
//
// Return contract:
//   - (images, true)  -- healthy cycle: every invoked source met its per-source threshold.
//   - (images, false) -- degraded/empty cycle: an invoked source was unhealthy, or no
//     invoked source produced an entry.
//
// registry-stats is stateless: Run never persists the result. The healthy flag drives
// only the partial-failure WARN below; the caller derives its health marker separately
// (see main.runCollect).
func Run(ctx context.Context, opts Options) (images []obs.ImageMetric, healthy bool) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	start := now()
	logger.Info("starting collection")
	degraded := false
	invokedAnySource := false
	// Per-source counts tracked separately so the summary logs keep the
	// docker_hub/ghcr keys Loki dashboards filter on.
	var dockerHubCount, ghcrCount int

	for _, src := range opts.Sources {
		refs := refsFor(opts, src.Source().String())
		if len(refs) == 0 {
			continue
		}
		invokedAnySource = true
		srcImages, srcHealthy := collectSource(ctx, logger, src, refs)
		if !srcHealthy {
			degraded = true
		}
		switch src.Source() {
		case registry.DockerHub:
			dockerHubCount = len(srcImages)
		case registry.GHCR:
			ghcrCount = len(srcImages)
		}
		images = append(images, srcImages...)
	}

	// An all-empty cycle has nothing to expose; return healthy=false so the caller marks unhealthy.
	if len(images) == 0 {
		if !invokedAnySource {
			logger.Warn("no repos configured")
		} else {
			logger.Error("all collections failed")
		}
		return images, false
	}

	if degraded {
		logger.Warn("partial collection failure, serving available data",
			"docker_hub", dockerHubCount, "ghcr", ghcrCount)
	}

	logger.Info("collection complete",
		"docker_hub", dockerHubCount,
		"ghcr", ghcrCount,
		"duration", now().Sub(start).Round(time.Millisecond))

	return images, !degraded
}

// collectSource invokes a single source's Collect and stamps its entries
// with the source's registry label, reporting whether the source met its
// health threshold. A DockerHub source that returns partial data while
// flagging unhealthy gets the severe-degradation warn (matching
// main.go's pre-refactor phrasing); any unhealthy source bumps the
// per-source error metric. Entries are stamped regardless of health so
// a degraded-but-nonempty source still contributes its data. A source
// whose Source() is unknown has no registry label to stamp, so its
// entries are dropped (only its health accounting is kept).
func collectSource(
	ctx context.Context,
	logger *slog.Logger,
	src Source,
	refs []registry.RepoRef,
) (images []obs.ImageMetric, srcHealthy bool) {
	entries, attempted, srcHealthy := src.Collect(ctx, refs)
	// Count every invoked source as a collect run; collect_errors_total below is
	// the failed subset, so collect_errors_total / collects_total is a valid
	// per-source failure ratio.
	obs.CollectsTotal.Inc(src.Source().String())
	if !srcHealthy {
		if src.Source() == registry.DockerHub && len(entries) > 0 {
			logger.Warn("docker hub collection severely degraded",
				"succeeded", len(entries), "attempted", attempted)
		}
		obs.CollectErrors.Inc(src.Source().String())
	}

	label := src.Source().String()
	if label == "" {
		return nil, srcHealthy
	}
	images = make([]obs.ImageMetric, 0, len(entries))
	for _, e := range entries {
		// Defensive: a zero-value entry would emit a broken {owner,repo}
		// label pair. The registry clients always populate Repo, so this
		// only strips entries a future source regression could produce.
		if e.Repo == "" {
			continue
		}
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
