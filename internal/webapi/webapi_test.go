package webapi

import (
	"sync/atomic"
	"testing"

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

// TestFakeHealthSignal exercises the in-memory HealthSignal fake so its
// Set/Healthy round-trip is covered (and reachable for dead-code analysis).
func TestFakeHealthSignal(t *testing.T) {
	var f fakeHealth
	if f.Healthy() {
		t.Fatal("zero-value fakeHealth.Healthy() = true, want false")
	}
	f.Set(true)
	if !f.Healthy() {
		t.Fatal("after Set(true), Healthy() = false, want true")
	}
	f.Set(false)
	if f.Healthy() {
		t.Fatal("after Set(false), Healthy() = true, want false")
	}
}
