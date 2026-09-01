// Package urlsafe holds URL path segment safety predicates.
package urlsafe

import "regexp"

// maxSegmentBytes matches the container-reference repository-path limit.
const maxSegmentBytes = 255

// safeSegment is an allowlist so unrecognized input is rejected by default.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeURLSegment reports whether s can safely appear in a URL path segment.
func IsSafeURLSegment(s string) bool {
	if s == "." || s == ".." || len(s) > maxSegmentBytes {
		return false
	}
	return safeSegment.MatchString(s)
}
