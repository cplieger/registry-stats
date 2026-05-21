package webapi

import (
	"sync/atomic"
	"testing"

	"registry-stats/internal/api"
	"registry-stats/internal/testsupport"
)

// TestMemStore_StoreContract verifies that the in-memory fake satisfies
// the same api.Store contract as store.FS, preventing silent drift.

func TestMemStore_StoreContract(t *testing.T) {
	testsupport.RunStoreContract(t, func(t *testing.T) api.Store {
		return testsupport.NewMemStore()
	})
}

// fakeHealth is an in-memory api.HealthSignal for handler tests.

type fakeHealth struct {
	healthy atomic.Bool
}

// Compile-time assertion: *fakeHealth satisfies api.HealthSignal.

var _ api.HealthSignal = (*fakeHealth)(nil)

func (f *fakeHealth) Set(ok bool) { f.healthy.Store(ok) }

func (f *fakeHealth) Healthy() bool { return f.healthy.Load() }

func (f *fakeHealth) Cleanup() { f.healthy.Store(false) }

// fixedSnapshot builds a deterministic snapshot with one DockerHub
// repo and one GHCR package. Callers tweak the counts per test.
