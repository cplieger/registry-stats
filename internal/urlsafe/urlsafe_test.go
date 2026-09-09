package urlsafe

import (
	"fmt"
	"net/url"
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
		{name: "at the bound", size: MaxSegmentBytes, want: true},
		{name: "one over the bound", size: MaxSegmentBytes + 1, want: false},
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

func TestPackageName(t *testing.T) {
	atBound := strings.Repeat("a", MaxSegmentBytes-len("owner"+"/"))
	overBound := atBound + "a"
	tests := []struct {
		name  string
		owner string
		token string
		want  string
	}{
		{name: "nested", owner: "owner", token: "helm-charts%2Fgrafana-operator", want: "helm-charts/grafana-operator"},
		{name: "at whole-name bound", owner: "owner", token: atBound, want: atBound},
		{name: "over whole-name bound", owner: "owner", token: overBound},
		{name: "many short elements", owner: "owner", token: strings.Repeat("a%2F", 126) + "a"},
		{name: "raw slash", owner: "owner", token: "app/versions"},
		{name: "unsafe element", owner: "owner", token: "app%2FRepo$"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PackageName(Owner(tt.owner), tt.token)
			if got != tt.want || (err != nil) != (tt.want == "") {
				t.Errorf("PackageName(%q, %q) = (%q, %v), want %q", tt.owner, tt.token, got, err, tt.want)
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

func FuzzPackageName_decodesBeforeValidatingElements(f *testing.F) {
	f.Add("owner", "helm-charts%2Fgrafana-operator")
	f.Add("owner", "%2E%2E")
	f.Add("owner", "app%252Fversions")
	f.Add("owner", "app/versions")
	f.Add("owner", "%zz")
	f.Add("owner", strings.Repeat("a", MaxSegmentBytes-len("owner/")))
	f.Add("owner", strings.Repeat("a", MaxSegmentBytes-len("owner/")+1))

	f.Fuzz(func(t *testing.T, owner, token string) {
		decoded, decodeErr := url.PathUnescape(token)
		wantOK := !strings.Contains(token, "/") && decodeErr == nil && len(owner)+1+len(decoded) <= MaxSegmentBytes
		if wantOK {
			for part := range strings.SplitSeq(decoded, "/") {
				if !IsSafeURLSegment(part) {
					wantOK = false
					break
				}
			}
		}

		got, err := PackageName(Owner(owner), token)
		if (err == nil) != wantOK {
			t.Fatalf("PackageName(%q, %q) = (%q, %v), accepted = %v", owner, token, got, err, wantOK)
		}
		if err != nil {
			if got != "" {
				t.Fatalf("PackageName(%q, %q) returned name %q with error %v, want empty name", owner, token, got, err)
			}
			return
		}
		if got != decoded {
			t.Fatalf("PackageName(%q, %q) = %q, want decoded name %q", owner, token, got, decoded)
		}
	})
}
