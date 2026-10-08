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

// versionsHTML is the served shape of a versions page's two filter links.
func versionsHTML(tagged, untagged string) string {
	return `<a class="btn-link selected" href="/users/o/packages/container/p/versions?filters%5Bversion_type%5D=tagged">` +
		`<svg class="octicon octicon-tag"><path d="M1"></path></svg>` + "\n  " + tagged + " tagged\n</a>" +
		`<a class="btn-link " href="/users/o/packages/container/p/versions?filters%5Bversion_type%5D=untagged">` +
		"\n  " + untagged + " untagged\n</a>"
}

func TestParseVersionCounts_ReadsBothFilterLinks(t *testing.T) {
	tests := []struct {
		name             string
		html             string
		tagged, untagged int64
	}{
		{"thousands separators", versionsHTML("1,002", "2,215"), 1002, 2215},
		{"zero untagged", versionsHTML("7", "0"), 7, 0},
		{"millions", versionsHTML("1,234,567", "12"), 1234567, 12},
		{"pagination links carry no count", versionsHTML("3", "4") +
			`<a href="/o/p/pkgs/container/p/versions?filters%5Bversion_type%5D=tagged&amp;page=2">2</a>`, 3, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tagged, untagged, err := parseVersionCounts(tt.html)
			if err != nil || tagged != tt.tagged || untagged != tt.untagged {
				t.Errorf("parseVersionCounts(%q) = (%d, %d, %v), want (%d, %d, nil)", tt.html, tagged, untagged, err, tt.tagged, tt.untagged)
			}
		})
	}
}

// A count printed as a bound must never publish as a value: a lower bound read as
// the count would look like a real number on the dashboard.
func TestParseVersionCounts_FormatChanged(t *testing.T) {
	tests := []struct {
		name string
		html string
	}{
		{"count shown as a bound", versionsHTML("1000+", "5")},
		{"count shown in words", versionsHTML("many", "5")},
		{"signed count", versionsHTML("-3", "5")},
		{"untagged link missing", versionsHTML("1", "2")[:strings.Index(versionsHTML("1", "2"), "<a class=\"btn-link \"")]},
		{"tagged link twice", versionsHTML("1", "2") + versionsHTML("1", "2")},
		{"link does not close", strings.TrimSuffix(versionsHTML("1", "2"), "\n</a>")},
		{"no count before the state", versionsHTML("", "2")},
		{"overlong count", versionsHTML(strings.Repeat("9", 16), "2")},
		{"doubled separator", versionsHTML("1,,002", "2")},
		{"short group after a separator", versionsHTML("1,02", "2")},
		{"leading separator", versionsHTML(",002", "2")},
		{"trailing separator", versionsHTML("1,", "2")},
		{"first group over three digits", versionsHTML("1002,215", "2")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tagged, untagged, err := parseVersionCounts(tt.html); !errors.Is(err, errHTMLFormatChanged) {
				t.Errorf("parseVersionCounts(%q) = (%d, %d, %v), want errHTMLFormatChanged", tt.html, tagged, untagged, err)
			}
		})
	}
}
