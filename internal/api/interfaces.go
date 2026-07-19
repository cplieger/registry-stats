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

	"github.com/cplieger/registry-stats/v2/internal/model"
)

// RegistrySource collects registry-specific statistics for a list of refs.
// Implementations return flat per-image RegistryEntry records; the
// orchestrator (internal/collect) stamps each with the source's registry
// label for the metric surface.
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

// HealthSignal abstracts the file-marker liveness healthcheck used by
// distroless containers. *health.Marker satisfies it. The collect loop
// flips it once per cycle (healthy when a cycle collected at least one
// repo); the `registry-stats health` subcommand reads the marker file for
// the Docker HEALTHCHECK. HTTP serving-readiness is a separate concern —
// GET /api/health is backed by a webhttp.Ready gate, not this marker — so
// this interface is deliberately write-only.
type HealthSignal interface {
	Set(healthy bool)
}
