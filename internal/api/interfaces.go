// Package api holds the cross-package interfaces that registry-stats
// subpackages depend on. This is the composition spine: concrete types
// in internal/store, internal/dockerhub, internal/ghcr, and the root
// healthMarker implement these interfaces, and the composition root
// (main.go) wires them together.
//
// The interfaces are deliberately small — each represents a single
// concern — so fakes in *_test.go files stay tiny.
package api

import (
	"context"
	"net/http"

	"registry-stats/internal/model"
)

// Store persists and retrieves daily snapshots. Concrete implementation:
// *store.FS in internal/store.
type Store interface {
	Save(ctx context.Context, snap *model.Snapshot) error
	Load(ctx context.Context, date string) (*model.Snapshot, error)
	ListDates(ctx context.Context) ([]string, error)
	Prune(ctx context.Context, retentionDays int) (int, error)
	CleanupStaleTmp(ctx context.Context) error
	// PullSeries returns the pre-computed pull-count time-series from
	// the in-memory index. Decouples the read path from full-snapshot
	// iteration so handler latency is independent of retention window.
	PullSeries(ctx context.Context) []model.PullEntry
}

// RegistrySource collects registry-specific statistics for a list of refs.
// Implementations return registry-agnostic RegistryEntry values; the
// orchestrator (internal/collect) maps them into per-registry on-disk
// arrays. attempted counts refs the source tried to fetch (including
// failures) so degraded detection can see the shortfall; healthy is true
// only when the source met its per-source health criteria.
//
// Name() returns the lowercase on-wire name used in WARN/ERROR log
// k/v pairs and in the JSON summary row's `registry` field
// (inviolate: log keys + HTTP API surface). Source() returns the
// typed model.RegistrySource — the orchestrator uses it to route
// entries into per-source slices without a string compare, while
// Name() continues to surface the human-readable label in logs.
// Implementations MUST keep String() of Source() equal to Name()
// so the two views never drift.
type RegistrySource interface {
	Name() string
	Source() model.RegistrySource
	Collect(
		ctx context.Context,
		refs []model.RepoRef,
	) (entries []model.RegistryEntry, attempted int, healthy bool)
}

// HealthSignal abstracts the file-marker healthcheck used by distroless
// containers. *healthMarker in main satisfies this; handlers use it to
// render /api/health without reaching into a global.
type HealthSignal interface {
	Set(healthy bool)
	Healthy() bool
	Cleanup()
}

// HTTPDoer is the subset of *http.Client that the registry clients use.
// Kept as an interface so tests can inject a mock without a real
// *http.Client. httpx.NewClient returns a concrete *http.Client which
// satisfies this structurally.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}
