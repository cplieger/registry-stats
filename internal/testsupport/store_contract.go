package testsupport

import (
	"testing"
	"time"

	"registry-stats/internal/api"
	"registry-stats/internal/model"
)

// NewContractMemStore returns a minimal api.Store fake for testing the
// contract helper itself. It delegates to the shared MemStore.
func NewContractMemStore() api.Store {
	return NewMemStore()
}

// RunStoreContract exercises the core api.Store semantics against any
// implementation. Both the real store.FS and in-memory fakes must pass
// this contract to prove they behave identically.
func RunStoreContract(t *testing.T, newStore func(t *testing.T) api.Store) {
	t.Helper()

	snap := &model.Snapshot{
		Timestamp: time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		DockerHub: []model.RepoStats{{Repo: "owner/app", PullCount: 42}},
		GHCR:      []model.GhcrStats{{Package: "owner/pkg", DownloadCount: 7}},
	}

	t.Run("Save_and_Load_roundtrip", func(t *testing.T) {
		contractSaveLoadRoundtrip(t, newStore(t), snap)
	})
	t.Run("Load_missing_date_returns_error", func(t *testing.T) {
		contractLoadMissingDate(t, newStore(t))
	})
	t.Run("ListDates_returns_sorted", func(t *testing.T) {
		contractListDatesSorted(t, newStore(t))
	})
	t.Run("Prune_zero_retention_is_noop", func(t *testing.T) {
		contractPruneZeroNoop(t, newStore(t), snap)
	})
	t.Run("CleanupStaleTmp_no_error", func(t *testing.T) {
		contractCleanupStaleTmpNoError(t, newStore(t))
	})
	t.Run("PullSeries_reflects_saved_data", func(t *testing.T) {
		contractPullSeriesReflectsSaved(t, newStore(t), snap)
	})
}

func contractSaveLoadRoundtrip(t *testing.T, s api.Store, snap *model.Snapshot) {
	t.Helper()
	ctx := t.Context()
	if err := s.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "2026-05-10")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DockerHub[0].PullCount != 42 {
		t.Errorf("PullCount = %d, want 42", got.DockerHub[0].PullCount)
	}
	if got.GHCR[0].DownloadCount != 7 {
		t.Errorf("DownloadCount = %d, want 7", got.GHCR[0].DownloadCount)
	}
}

func contractLoadMissingDate(t *testing.T, s api.Store) {
	t.Helper()
	_, err := s.Load(t.Context(), "1999-01-01")
	if err == nil {
		t.Fatal("expected error for missing date")
	}
}

func contractListDatesSorted(t *testing.T, s api.Store) {
	t.Helper()
	ctx := t.Context()
	dates := []string{"2026-05-12", "2026-05-10", "2026-05-11"}
	for _, d := range dates {
		ts, parseErr := time.Parse("2006-01-02", d)
		if parseErr != nil {
			t.Fatalf("time.Parse(%q): %v", d, parseErr)
		}
		sn := &model.Snapshot{Timestamp: ts, DockerHub: []model.RepoStats{{Repo: "r", PullCount: 1}}}
		if err := s.Save(ctx, sn); err != nil {
			t.Fatalf("Save(%s): %v", d, err)
		}
	}
	got, err := s.ListDates(ctx)
	if err != nil {
		t.Fatalf("ListDates: %v", err)
	}
	want := []string{"2026-05-10", "2026-05-11", "2026-05-12"}
	if len(got) != len(want) {
		t.Fatalf("ListDates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListDates[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func contractPruneZeroNoop(t *testing.T, s api.Store, snap *model.Snapshot) {
	t.Helper()
	ctx := t.Context()
	if err := s.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	n, err := s.Prune(ctx, 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("Prune(0) = %d, want 0", n)
	}
}

func contractCleanupStaleTmpNoError(t *testing.T, s api.Store) {
	t.Helper()
	if err := s.CleanupStaleTmp(t.Context()); err != nil {
		t.Fatalf("CleanupStaleTmp: %v", err)
	}
}

func contractPullSeriesReflectsSaved(t *testing.T, s api.Store, snap *model.Snapshot) {
	t.Helper()
	ctx := t.Context()
	if err := s.Save(ctx, snap); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries := s.PullSeries(ctx)
	if len(entries) == 0 {
		t.Fatal("PullSeries returned no entries after Save")
	}
	found := false
	for _, e := range entries {
		if e.Repo == "owner/app" && e.PullCount == 42 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PullSeries missing owner/app with PullCount=42; got %v", entries)
	}
}
