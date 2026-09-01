package urlsafe

import (
	"fmt"
	"strings"
	"testing"
)

func TestIsSafeURLSegment(t *testing.T) {
	safe := []string{"cplieger", "fclones-scheduler", "home.assistant", "my_repo"}
	for _, s := range safe {
		if !IsSafeURLSegment(s) {
			t.Errorf("IsSafeURLSegment(%q) = false, want true", s)
		}
	}
	unsafe := []string{"a/b", "a%20b", "a\\b", "a?b", "a#b", "a@b", "a:b"}
	for _, s := range unsafe {
		if IsSafeURLSegment(s) {
			t.Errorf("IsSafeURLSegment(%q) = true, want false", s)
		}
	}
}

func TestIsSafeURLSegment_rejects_traversal_names(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"...", true},
		{".hidden", true},
		{"a", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.input), func(t *testing.T) {
			if got := IsSafeURLSegment(tt.input); got != tt.want {
				t.Errorf("IsSafeURLSegment(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSafeURLSegment_bounds_length(t *testing.T) {
	tests := []struct {
		name string
		size int
		want bool
	}{
		{name: "at the bound", size: maxSegmentBytes, want: true},
		{name: "one over the bound", size: maxSegmentBytes + 1, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Repeat("a", tt.size)
			if got := IsSafeURLSegment(input); got != tt.want {
				t.Errorf("IsSafeURLSegment(%d chars) = %v, want %v", tt.size, got, tt.want)
			}
		})
	}
}

// FuzzIsSafeURLSegment pins the positive security contract of the URL
// path-segment allowlist: every string IsSafeURLSegment accepts must be
// non-empty, must not be a traversal element ("." or ".."), and must
// consist solely of the allowed bytes [A-Za-z0-9._-]. The byte-membership
// check re-derives the allowlist independently rather than calling back
// into the production regexp, so a regexp mutated to admit any other byte
// is caught here.
func FuzzIsSafeURLSegment(f *testing.F) {
	f.Add("cplieger")
	f.Add("fclones-scheduler")
	f.Add("home.assistant")
	f.Add("my_repo")
	f.Add("...")
	f.Add(".hidden")
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("a/b")
	f.Add("a b")
	f.Add("a~b")
	f.Add("caf\xc3\xa9")
	f.Add("a\x00b")
	f.Fuzz(func(t *testing.T, s string) {
		if !IsSafeURLSegment(s) {
			return
		}
		if s == "" || s == "." || s == ".." {
			t.Errorf("IsSafeURLSegment(%q) = true, but it is empty or a traversal element", s)
		}
		for _, b := range []byte(s) {
			allowed := (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
				(b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-'
			if !allowed {
				t.Errorf("IsSafeURLSegment(%q) = true, but it contains disallowed byte %q", s, b)
			}
		}
	})
}
