package model

import (
	"encoding/json"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// TestSnapshotJSONRoundTripShape pins the JSON field names for the
// model types. This minimal test catches accidental struct-tag drift
// (e.g. renaming "pull_count" to "pullCount").
func TestSnapshotJSONRoundTripShape(t *testing.T) {
	snap := Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []RepoStats{{
			Repo:      "owner/app",
			PullCount: 42,
			Tags: []TagInfo{{
				Name:   "latest",
				Digest: "sha256:abc",
				Images: []ImageInfo{
					{Architecture: "amd64", OS: "linux", Size: 512},
				},
			}},
		}},
		GHCR: []GhcrStats{{Package: "owner/pkg", DownloadCount: 100}},
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Spot-check the inviolate field names. Full schema is enforced by
	// downstream round-trip tests; this guards against tag typos.
	s := string(data)
	for _, field := range []string{
		`"timestamp"`, `"docker_hub"`, `"ghcr"`,
		`"repo"`, `"pull_count"`, `"tags"`,
		`"name"`, `"digest"`, `"images"`,
		`"architecture"`, `"os"`,
		`"package"`, `"download_count"`,
	} {
		if !contains(s, field) {
			t.Errorf("marshaled JSON missing field %s: %s", field, s)
		}
	}

	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.DockerHub[0].PullCount != 42 {
		t.Errorf("PullCount round-trip: got %d, want 42", decoded.DockerHub[0].PullCount)
	}
	if decoded.GHCR[0].DownloadCount != 100 {
		t.Errorf("DownloadCount round-trip: got %d, want 100", decoded.GHCR[0].DownloadCount)
	}
	if len(decoded.DockerHub[0].Tags) != 1 || len(decoded.DockerHub[0].Tags[0].Images) != 1 {
		t.Errorf("Tags/Images not preserved: %+v", decoded.DockerHub[0].Tags)
	}
}

// contains is a tiny helper used above. We deliberately avoid depending on
// strings to keep this test file's imports minimal (seen as a code-review
// signal that the model package has no runtime dependencies).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestSnapshotJSONRoundTrip (migrated from main_test.go in chain step 6)
// is a value-level counterpart to TestSnapshotJSONRoundTripShape: it
// checks that the concrete PullCount / DownloadCount / Tags / Images
// values survive Marshal → Unmarshal unchanged. The shape test above
// already covers the struct-tag surface; this one nails down the
// value-preservation contract these legacy JSON tags carry (v2 is
// stateless; the tags are exercised only by these round-trip tests).
func TestSnapshotJSONRoundTrip(t *testing.T) {
	snap := Snapshot{
		Timestamp: time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC),
		DockerHub: []RepoStats{{
			Repo:        "owner/app",
			PullCount:   42,
			LastUpdated: "2026-03-06T12:00:00Z",
			Tags: []TagInfo{{
				Name: "latest", FullSize: 1024, Digest: "sha256:abc",
				Images: []ImageInfo{
					{Architecture: "amd64", OS: "linux", Size: 512, Digest: "sha256:def"},
				},
			}},
		}},
		GHCR: []GhcrStats{{Package: "owner/pkg", DownloadCount: 100}},
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var loaded Snapshot
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded.DockerHub) != 1 || loaded.DockerHub[0].PullCount != 42 {
		t.Errorf("DockerHub: got %+v", loaded.DockerHub)
	}
	if len(loaded.GHCR) != 1 || loaded.GHCR[0].DownloadCount != 100 {
		t.Errorf("GHCR: got %+v", loaded.GHCR)
	}
	if len(loaded.DockerHub[0].Tags) != 1 || len(loaded.DockerHub[0].Tags[0].Images) != 1 {
		t.Errorf("Tags/Images not preserved: %+v", loaded.DockerHub[0].Tags)
	}
}

// TestSnapshotJSONRoundTrip_PBT (migrated from main_test.go in chain
// step 6) is the property-based counterpart to TestSnapshotJSONRoundTrip:
// for any randomly-generated (bounded) Snapshot, Marshal → Unmarshal
// preserves the PullCount and DownloadCount values. Uses rapid's
// StringMatching + Int64Range generators to sweep the corners the
// example-based test can't reach.
func TestSnapshotJSONRoundTrip_PBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		snap := Snapshot{
			Timestamp: time.Date(
				rapid.IntRange(2020, 2030).Draw(t, "year"),
				time.Month(rapid.IntRange(1, 12).Draw(t, "month")),
				rapid.IntRange(1, 28).Draw(t, "day"),
				0, 0, 0, 0, time.UTC),
			DockerHub: []RepoStats{{
				Repo:      rapid.StringMatching(`[a-z]{1,5}/[a-z]{1,5}`).Draw(t, "repo"),
				PullCount: rapid.Int64Range(0, 1<<40).Draw(t, "pulls"),
			}},
			GHCR: []GhcrStats{{
				Package:       rapid.StringMatching(`[a-z]{1,5}/[a-z]{1,5}`).Draw(t, "pkg"),
				DownloadCount: rapid.Int64Range(0, 1<<40).Draw(t, "downloads"),
			}},
		}

		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded Snapshot
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.DockerHub[0].PullCount != snap.DockerHub[0].PullCount {
			t.Errorf("PullCount round-trip: %d → %d",
				snap.DockerHub[0].PullCount, decoded.DockerHub[0].PullCount)
		}
		if decoded.GHCR[0].DownloadCount != snap.GHCR[0].DownloadCount {
			t.Errorf("DownloadCount round-trip: %d → %d",
				snap.GHCR[0].DownloadCount, decoded.GHCR[0].DownloadCount)
		}
	})
}

func TestRegistrySource_String(t *testing.T) {
	tests := []struct {
		name string
		src  RegistrySource
		want string
	}{
		{"dockerhub", SourceDockerHub, "dockerhub"},
		{"ghcr", SourceGHCR, "ghcr"},
		{"unknown is empty", SourceUnknown, ""},
		{"out-of-range is empty", RegistrySource(99), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.src.String(); got != tt.want {
				t.Errorf("RegistrySource(%d).String() = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}
