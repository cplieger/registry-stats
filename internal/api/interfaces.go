// Package api holds the cross-package interfaces that registry-stats
// subpackages depend on. This is the composition spine: concrete types
// in internal/dockerhub, internal/ghcr, and the root *health.Marker
// implement these interfaces, and the composition root (main.go) wires
// them together.
//
// The interfaces are deliberately small — each represents a single
// concern — so fakes in *_test.go files stay tiny.
package api

import (
	"context"

	"github.com/cplieger/registry-stats/internal/model"
)

// RegistrySource collects registry-specific statistics for a list of refs.
// Implementations return registry-agnostic RegistryEntry values; the
// orchestrator (internal/collect) maps them into per-registry slices.
// attempted counts refs the source tried to fetch (including failures)
// so degraded detection can see the shortfall; healthy is true only
// when the source met its per-source health criteria.
//
// Name() returns the lowercase on-wire name used in WARN/ERROR log
// k/v pairs. Source() returns the typed model.RegistrySource — the
// orchestrator uses it to route entries into per-source slices without
// a string compare, while Name() continues to surface the human-
// readable label in logs. Implementations MUST keep String() of
// Source() equal to Name() so the two views never drift.
type RegistrySource interface {
	Name() string
	Source() model.RegistrySource
	Collect(
		ctx context.Context,
		refs []model.RepoRef,
	) (entries []model.RegistryEntry, attempted int, healthy bool)
}

// HealthSignal abstracts the file-marker healthcheck used by distroless
// containers. *health.Marker satisfies this; handlers use it to
// render /api/health without reaching into a global.
type HealthSignal interface {
	Set(healthy bool)
	Healthy() bool
}
