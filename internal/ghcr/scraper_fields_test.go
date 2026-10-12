package ghcr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParsePublished_ReadsTheLabelledTime(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{"compact", `<span>Last published</span><h3 title="2026-09-09T07:14:12-07:00">3 hours ago</h3>`, "2026-09-09T14:14:12Z"},
		{"served whitespace", "<span class=\"d-block\">\n  Last published\n </span>\n <h3 title=\"2026-01-02T03:04:05Z\">x</h3>", "2026-01-02T03:04:05Z"},
		{"label text elsewhere is not a marker", `<p>Last published releases</p><span>Last published</span><h3 title="2026-01-02T03:04:05Z">x</h3>`, "2026-01-02T03:04:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePublished(tt.html)
			if err != nil {
				t.Fatalf("parsePublished(%q) error = %v, want nil", tt.html, err)
			}
			if want, _ := time.Parse(time.RFC3339, tt.want); !got.Equal(want) {
				t.Errorf("parsePublished(%q) = %v, want %v", tt.html, got, want)
			}
		})
	}
}

func TestParsePublished_FormatChanged(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{"no marker", downloadsHTML("1")[strings.Index(downloadsHTML("1"), "<span>Total"):]},
		{"two markers", `<span>Last published</span><h3 title="2026-01-02T03:04:05Z">x</h3><span>Last published</span><h3 title="2026-01-02T03:04:05Z">x</h3>`},
		{"no h3 after the marker", `<span>Last published</span><div title="2026-01-02T03:04:05Z">x</div>`},
		{"relative text only", `<span>Last published</span><h3>3 hours ago</h3>`},
		{"not RFC 3339", `<span>Last published</span><h3 title="Sep 9, 2026">x</h3>`},
		{"title unclosed", `<span>Last published</span><h3 title="2026-01-02T03:04:05Z`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := parsePublished(tt.html); !errors.Is(err, errHTMLFormatChanged) {
				t.Errorf("parsePublished(%q) = (%v, %v), want errHTMLFormatChanged", tt.html, got, err)
			}
		})
	}
}
