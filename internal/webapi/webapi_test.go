package webapi

import (
	"sync/atomic"

	"github.com/cplieger/registry-stats/internal/api"
)

// fakeHealth is an in-memory api.HealthSignal for handler tests.

type fakeHealth struct {
	healthy atomic.Bool
}

// Compile-time assertion: *fakeHealth satisfies api.HealthSignal.

var _ api.HealthSignal = (*fakeHealth)(nil)

func (f *fakeHealth) Set(ok bool) { f.healthy.Store(ok) }

func (f *fakeHealth) Healthy() bool { return f.healthy.Load() }
