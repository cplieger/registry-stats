package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func FuzzStoreLoad(f *testing.F) {
	f.Add([]byte(`{"timestamp":"2026-01-01T00:00:00Z","docker_hub":[],"ghcr":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Add([]byte(`{"timestamp":"bad","docker_hub":[{"repo":"x","pull_count":1}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 50<<20 {
			return
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-01-01.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewFS(dir)
		<-s.idxReady
		snap, err := s.Load(context.Background(), "2026-01-01")
		if err == nil {
			// Timestamp must round-trip through RFC3339
			if _, parseErr := time.Parse(time.RFC3339, snap.Timestamp.Format(time.RFC3339)); parseErr != nil {
				t.Errorf("timestamp not parseable as RFC3339: %v", parseErr)
			}
		}
	})
}
