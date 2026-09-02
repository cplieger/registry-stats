// Package urlsafe holds URL path segment safety predicates.
package urlsafe

import (
	"net/url"
	"regexp"
	"strings"
)

// MaxSegmentBytes bounds one URL path segment. A caller walking a
// multi-element name bounds each element, not the total; PackageName owns
// the whole-name bound.
const MaxSegmentBytes = 255

// safeSegment is an allowlist so unrecognized input is rejected by default.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeURLSegment reports whether s can safely appear in a URL path segment.
// The allowlist's + quantifier rejects an empty segment.
func IsSafeURLSegment(s string) bool {
	if s == "." || s == ".." || len(s) > MaxSegmentBytes {
		return false
	}
	return safeSegment.MatchString(s)
}

// PackageName decodes GHCR's one-token spelling of a package name and
// reports whether it is safe to put in a URL path and a metric label.
// A raw '/' means the token is not that spelling: after the split it
// reads as a separator the per-element gate cannot see.
func PackageName(owner, token string) (name string, ok bool) {
	if strings.Contains(token, "/") {
		return "", false
	}
	name, err := url.PathUnescape(token)
	if err != nil || name == "" {
		return "", false
	}
	if len(owner)+1+len(name) > MaxSegmentBytes {
		return "", false
	}
	for part := range strings.SplitSeq(name, "/") {
		if !IsSafeURLSegment(part) {
			return "", false
		}
	}
	return name, true
}
