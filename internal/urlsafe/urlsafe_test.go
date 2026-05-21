package urlsafe

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
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

func TestIsSafeURLSegment_rejects_empty_dot_dotdot(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"...", true},     // three dots is safe (not a traversal)
		{".hidden", true}, // dotfile-style name is safe
	}
	for _, tt := range tests {
		if got := IsSafeURLSegment(tt.input); got != tt.want {
			t.Errorf("IsSafeURLSegment(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsSafeURLSegment_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		input := rapid.String().Draw(t, "input")
		_ = IsSafeURLSegment(input) // must not panic
	})
}

func TestIsSafeURLSegment_rejects_all_unsafe_chars(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		unsafe := rapid.SampledFrom([]byte{'/', '%', '\\', '?', '#', '@', ':'}).Draw(t, "char")
		prefix := rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "prefix")
		suffix := rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "suffix")
		input := prefix + string(unsafe) + suffix
		if IsSafeURLSegment(input) {
			t.Errorf("IsSafeURLSegment(%q) = true, want false (contains %q)", input, string(unsafe))
		}
	})
}
