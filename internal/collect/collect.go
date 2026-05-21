// Package collect is the registry-stats collection orchestrator. It
// loops over a set of api.RegistrySource implementations, assembles a
// single *model.Snapshot from their per-source []model.RegistryEntry
// outputs, persists it via api.Store, and returns a healthy flag plus
// a "saved" flag so the caller can flip its healthcheck marker
// accordingly.
//
// The orchestrator is deliberately tiny: each source owns its own
// *http.Client, retry options, logging, and pacing. Run's job is the
// orchestration layer above that — route each source's entries back
// into the right on-disk slice, honor the "don't save empty snapshots"
// guard, and surface partial-degradation warnings without failing the
// whole cycle.
//
// The on-disk contract (model.Snapshot.DockerHub: []RepoStats,
// model.Snapshot.GHCR: []GhcrStats) is unchanged. Sources carry a
// stable Name() that the orchestrator matches ("dockerhub", "ghcr") to
// route entries back into each typed slice — this is the reverse of
// the per-source mapping that turns typed results into the
// registry-agnostic model.RegistryEntry shape.
package collect

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"registry-stats/internal/api"
	"registry-stats/internal/model"
)

// Options configures a single Run. Sources are the registry clients
// that actually fetch data; RefsFor maps each source's Name() to the
// []model.RepoRef it should fetch, allowing the orchestrator to stay
// agnostic of the per-registry config slice layout. Store persists
// the assembled snapshot; a nil Logger falls back to slog.Default.
// Now is the clock used for the snapshot timestamp (tests inject a
// deterministic clock; production passes time.Now).
type Options struct {
	Store   api.Store
	Logger  *slog.Logger
	Now     func() time.Time
	RefsFor func(name string) []model.RepoRef
	Sources []api.RegistrySource
}

// Run orchestrates a single collection cycle. It invokes each source's
// Collect (skipping sources whose ref slice is empty so empty-config
// paths stay zero-cost), assembles the result into a *model.Snapshot,
// persists the snapshot via Store when at least one source produced
// data, and returns the assembled snapshot plus a healthy flag.
//
// Return contract:
//   - (snap, true, nil)  — healthy cycle: all configured sources met
//     their per-source healthy threshold AND the snapshot persisted.
//   - (snap, false, nil) — degraded cycle: at least one source was
//     unhealthy OR the empty-guard fired; the snapshot may still have
//     been persisted when at least one source produced entries.
//   - (nil, false, err)  — save error; no snapshot is on disk.
//
// Degraded detection: healthy is the AND of per-source healthy flags
// over sources that were actually invoked. Sources that were skipped
// (empty refs) do not contribute to the ratio. This mirrors the
// pre-refactor main.go collect() semantics: a cycle with zero
// configured registries was healthy-by-vacuity.
//
// The empty-snapshot guard preserves pre-refactor behavior: when every
// invoked source returns zero entries, the snapshot is NOT saved
// (saving a zero-pull snapshot would corrupt the daily-delta
// calculation by treating it as a genuine drop). The returned healthy
// flag in that case is false so the caller can flag the cycle as
// degraded in its healthcheck.
func Run(ctx context.Context, opts Options) (snap *model.Snapshot, healthy bool, err error) {
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

		entries, attempted, srcHealthy := src.Collect(ctx, refs)
		if !srcHealthy {
			// Mirror the pre-refactor DockerHub warn log: surface the
			// severe-degradation signal whenever a source returns some
			// entries but less than half of what it attempted. The
			// source's Collect already decides healthy via Degraded; the
			// orchestrator just amplifies that with a warn when partial
			// data is present, matching main.go's pre-refactor phrasing.
			if src.Source() == model.SourceDockerHub && len(entries) > 0 {
				logger.Warn("docker hub collection severely degraded",
					"succeeded", len(entries), "attempted", attempted)
			}
			degraded = true
		}

		switch src.Source() {
		case model.SourceDockerHub:
			snap.DockerHub = entriesToDockerHub(entries)
		case model.SourceGHCR:
			snap.GHCR = entriesToGHCR(entries)
		default:
			// Unknown source types fall through with a warn log.
			// Today this branch is unreachable because only dockerhub
			// and ghcr exist; it exists so that adding a new source
			// doesn't silently drop its entries on the floor — they'll
			// at least surface in the log below. Name() is logged
			// (not Source()) because the human-readable string is
			// what's useful in an alert.
			logger.Warn("collect: unknown source; entries dropped",
				"source", src.Name(), "entries", len(entries))
		}
	}

	// Don't save empty snapshots — they corrupt daily delta calculations
	// by treating the missing data as genuine zero-pull days.
	if len(snap.DockerHub) == 0 && len(snap.GHCR) == 0 {
		if !invokedAnySource {
			logger.Warn("no repos configured, skipping snapshot save")
		} else {
			logger.Error("all collections failed, skipping snapshot save")
		}
		return snap, false, nil
	}

	if saveErr := opts.Store.Save(ctx, snap); saveErr != nil {
		logger.Error("failed to save snapshot", "error", saveErr)
		return nil, false, fmt.Errorf("save snapshot: %w", saveErr)
	}

	if degraded {
		logger.Warn("partial collection failure, snapshot saved with available data",
			"docker_hub", len(snap.DockerHub), "ghcr", len(snap.GHCR))
	}

	logger.Info("collection complete",
		"docker_hub", len(snap.DockerHub),
		"ghcr", len(snap.GHCR),
		"duration", now().Sub(start).Round(time.Millisecond))

	return snap, !degraded, nil
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
// back into the typed []model.RepoStats slice that the snapshot's
// docker_hub on-disk array requires. The per-source dockerhub.Client
// populates Name/LastUpdated/PullCount/Tags from its Collect call;
// this helper copies them into the destination shape field-for-field.
//
// Entries with empty Name are skipped defensively — the dockerhub
// client always populates Name, but stripping empty entries keeps the
// on-disk shape clean if a future source regression produces a zero
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
// real observed count. Filtering zero-downloads only happens at the
// handler layer (forEachSummaryEntry) to preserve the legacy daily-
// delta semantics.
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
