// Package collect is the registry-stats collection orchestrator. It
// loops over a set of api.RegistrySource implementations, assembles a
// single *model.Snapshot from their per-source []model.RegistryEntry
// outputs, and returns a healthy flag so the caller can flip its
// healthcheck marker accordingly.
//
// The orchestrator is deliberately tiny: each source owns its own
// *http.Client, retry options, logging, and pacing. Run's job is the
// orchestration layer above that — route each source's entries back
// into the right typed slice, and surface partial-degradation warnings
// without failing the whole cycle.
package collect

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/metrics"
	"github.com/cplieger/registry-stats/internal/model"
)

// Options configures a single Run. Sources are the registry clients
// that actually fetch data; RefsFor maps each source's Name() to the
// []model.RepoRef it should fetch, allowing the orchestrator to stay
// agnostic of the per-registry config slice layout. A nil Logger falls
// back to slog.Default. Now is the clock used for the snapshot timestamp
// (tests inject a deterministic clock; production passes time.Now).
type Options struct {
	Logger  *slog.Logger
	Now     func() time.Time
	RefsFor func(name string) []model.RepoRef
	Sources []api.RegistrySource
}

// Run orchestrates a single collection cycle. It invokes each source's
// Collect (skipping sources whose ref slice is empty so empty-config
// paths stay zero-cost), assembles the result into a *model.Snapshot,
// and returns the assembled snapshot plus a healthy flag.
//
// Return contract:
//   - (snap, true)  -- healthy cycle: every invoked source met its per-source threshold.
//   - (snap, false) -- degraded/empty cycle: an invoked source was unhealthy, or no
//     invoked source produced an entry.
//
// registry-stats is stateless: Run never persists the snapshot. The healthy flag drives
// only the partial-failure WARN below; the caller derives its health marker separately
// (see main.runCollect).
func Run(ctx context.Context, opts Options) (snap *model.Snapshot, healthy bool) {
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
	snap = &model.Snapshot{Timestamp: start.UTC()}
	degraded := false
	invokedAnySource := false

	for _, src := range opts.Sources {
		refs := refsFor(opts, src.Name())
		if len(refs) == 0 {
			continue
		}
		invokedAnySource = true
		if !collectSource(ctx, logger, src, refs, snap) {
			degraded = true
		}
	}

	// An all-empty cycle has nothing to expose; return healthy=false so the caller marks unhealthy.
	if len(snap.DockerHub) == 0 && len(snap.GHCR) == 0 {
		if !invokedAnySource {
			logger.Warn("no repos configured")
		} else {
			logger.Error("all collections failed")
		}
		return snap, false
	}

	if degraded {
		logger.Warn("partial collection failure, serving available data",
			"docker_hub", len(snap.DockerHub), "ghcr", len(snap.GHCR))
	}

	logger.Info("collection complete",
		"docker_hub", len(snap.DockerHub),
		"ghcr", len(snap.GHCR),
		"duration", now().Sub(start).Round(time.Millisecond))

	return snap, !degraded
}

// collectSource invokes a single source's Collect, routes its entries
// into the matching typed slice on snap, and reports whether the source
// met its health threshold. A DockerHub source that returns partial
// data while flagging unhealthy gets the severe-degradation warn
// (matching main.go's pre-refactor phrasing); any unhealthy source bumps
// the per-source error metric. Routing happens regardless of health so a
// degraded-but-nonempty source still contributes its entries.
func collectSource(
	ctx context.Context,
	logger *slog.Logger,
	src api.RegistrySource,
	refs []model.RepoRef,
	snap *model.Snapshot,
) (srcHealthy bool) {
	entries, attempted, srcHealthy := src.Collect(ctx, refs)
	// Count every invoked source as a collect run; collect_errors_total below is
	// the failed subset, so collect_errors_total / collects_total is a valid
	// per-source failure ratio.
	metrics.CollectsTotal.Inc(src.Name())
	if !srcHealthy {
		if src.Source() == model.SourceDockerHub && len(entries) > 0 {
			logger.Warn("docker hub collection severely degraded",
				"succeeded", len(entries), "attempted", attempted)
		}
		metrics.CollectErrors.Inc(src.Name())
	}

	switch src.Source() {
	case model.SourceDockerHub:
		snap.DockerHub = entriesToDockerHub(entries)
	case model.SourceGHCR:
		snap.GHCR = entriesToGHCR(entries)
	}
	return srcHealthy
}

// refsFor resolves refs for a given source name, returning nil when
// opts.RefsFor is unset or the source has no refs. A nil RefsFor is
// equivalent to "no refs for any source", which short-circuits the
// whole loop — useful for orchestrator-only tests that pass canned
// entries via fake sources with baked-in state.
func refsFor(opts Options, name string) []model.RepoRef {
	if opts.RefsFor == nil {
		return nil
	}
	return opts.RefsFor(name)
}

// entriesToDockerHub maps the registry-agnostic []model.RegistryEntry
// back into the typed []model.RepoStats slice the snapshot's
// docker_hub field carries. The per-source dockerhub.Client
// populates Name/LastUpdated/PullCount/Tags from its Collect call;
// this helper copies them into the destination shape field-for-field.
//
// Entries with empty Name are skipped defensively — the dockerhub
// client always populates Name, but stripping empty entries keeps the
// snapshot shape clean if a future source regression produces a zero
// value.
func entriesToDockerHub(entries []model.RegistryEntry) []model.RepoStats {
	if len(entries) == 0 {
		return nil
	}
	out := make([]model.RepoStats, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		out = append(out, model.RepoStats{
			Repo:        e.Name,
			LastUpdated: e.LastUpdated,
			PullCount:   e.PullCount,
			Tags:        e.Tags,
		})
	}
	return out
}

// entriesToGHCR maps the registry-agnostic []model.RegistryEntry back
// into the typed []model.GhcrStats slice. The per-source ghcr.Client
// populates Name and DownloadCount; this helper copies them across.
// Zero-download entries are NOT filtered here — ghcr.Client already
// skips scrape failures so any entry that reaches this mapper is a
// real observed count.
func entriesToGHCR(entries []model.RegistryEntry) []model.GhcrStats {
	if len(entries) == 0 {
		return nil
	}
	out := make([]model.GhcrStats, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		out = append(out, model.GhcrStats{
			Package:       e.Name,
			DownloadCount: e.DownloadCount,
		})
	}
	return out
}
