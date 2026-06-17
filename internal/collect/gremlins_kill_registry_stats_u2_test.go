package collect

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/model"
)

// gk_registry_stats_u2_capHandler is a slog.Handler that records message
// texts so a test can assert whether a specific log line was (or was
// not) emitted. The Run orchestrator emits its severe-degradation signal
// only as a log line, so capturing it is the only observable.
type gk_registry_stats_u2_capHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *gk_registry_stats_u2_capHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *gk_registry_stats_u2_capHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}

func (h *gk_registry_stats_u2_capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gk_registry_stats_u2_capHandler) WithGroup(string) slog.Handler      { return h }

func (h *gk_registry_stats_u2_capHandler) gk_registry_stats_u2_saw(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// gk_registry_stats_u2_fakeSource is a canned api.RegistrySource. Its
// Collect always reports unhealthy so Run enters the `if !srcHealthy`
// block that guards the line-92 warn condition.
type gk_registry_stats_u2_fakeSource struct {
	source  model.RegistrySource
	entries []model.RegistryEntry
}

func (f *gk_registry_stats_u2_fakeSource) Name() string                 { return f.source.String() }
func (f *gk_registry_stats_u2_fakeSource) Source() model.RegistrySource { return f.source }

func (f *gk_registry_stats_u2_fakeSource) Collect(
	_ context.Context,
	_ []model.RepoRef,
) ([]model.RegistryEntry, int, bool) {
	return f.entries, len(f.entries), false
}

var _ api.RegistrySource = (*gk_registry_stats_u2_fakeSource)(nil)

const gk_registry_stats_u2_degradedMsg = "docker hub collection severely degraded"

// TestRun_severe_degradation_warn_condition pins the exact truth table of
//
//	src.Source() == model.SourceDockerHub && len(entries) > 0
//
// at collect.go:92. This kills three mutants on that line:
//   - 92:20 CONDITIONALS_NEGATION (`==` -> `!=`): the DockerHub case must
//     warn (mutant suppresses it) and the GHCR case must stay silent
//     (mutant would warn for a non-DockerHub source).
//   - 92:61 CONDITIONALS_NEGATION (`>` -> `<=`): the entries>0 DockerHub
//     case must warn (mutant `<=0` suppresses it).
//   - 92:61 CONDITIONALS_BOUNDARY (`>` -> `>=`): the entries==0 DockerHub
//     case must stay silent (mutant `>=0` would warn at the boundary).
func TestRun_severe_degradation_warn_condition(t *testing.T) {
	dhEntries := []model.RegistryEntry{{Name: "owner/app", PullCount: 1}}
	ghEntries := []model.RegistryEntry{{Name: "owner/pkg", DownloadCount: 1}}

	tests := []struct {
		name     string
		source   model.RegistrySource
		entries  []model.RegistryEntry
		wantWarn bool
	}{
		{"dockerhub_unhealthy_with_entries_warns", model.SourceDockerHub, dhEntries, true},
		{"dockerhub_unhealthy_zero_entries_silent", model.SourceDockerHub, nil, false},
		{"ghcr_unhealthy_with_entries_silent", model.SourceGHCR, ghEntries, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &gk_registry_stats_u2_capHandler{}
			src := &gk_registry_stats_u2_fakeSource{source: tt.source, entries: tt.entries}

			_, _, err := Run(t.Context(), Options{
				Sources: []api.RegistrySource{src},
				Logger:  slog.New(h),
				RefsFor: func(string) []model.RepoRef {
					return []model.RepoRef{{Owner: "owner", Repo: "x"}}
				},
			})
			if err != nil {
				t.Fatalf("Run() err = %v, want nil", err)
			}

			got := h.gk_registry_stats_u2_saw(gk_registry_stats_u2_degradedMsg)
			if got != tt.wantWarn {
				t.Errorf("Run() severe-degradation warn emitted = %v, want %v (source=%s, entries=%d)",
					got, tt.wantWarn, tt.source, len(tt.entries))
			}
		})
	}
}
