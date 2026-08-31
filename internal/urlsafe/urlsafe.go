// Package urlsafe holds URL path segment safety predicates. It depends
// only on the standard library and on no other internal/* package so
// it can be imported from any layer (config, scrapers, handlers) without
// risking an import cycle. The predicate here is the single source of
// truth for "is this string safe to embed in a URL path segment"; every
// caller that builds a URL from external input (env vars today, HTTP
// request bodies tomorrow) routes through this function.
package urlsafe

import "regexp"

// maxSegmentBytes bounds one segment at the length the container-reference
// grammar allows for a whole repository path, so it is above anything either
// registry can name. The allowlist is ASCII, so bytes are characters.
const maxSegmentBytes = 255

// safeSegment is a traversal-and-injection gate rather than a registry
// validity check: it admits a safety superset of what either registry's own
// grammar permits — ASCII letters, digits, dot, underscore, and hyphen. An
// allowlist (rather than a denylist of known-dangerous bytes) keeps the
// guarantee robust as the input surface broadens: anything outside this set
// (spaces, control characters, unicode, %, /, and the rest) is rejected by
// construction rather than only the handful a denylist happens to enumerate.
// github-scout carries a second copy of this allowlist and of maxSegmentBytes;
// nothing enforces the match, so a change to either has to land in both repos.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// IsSafeURLSegment returns true if s is safe to embed in a URL path
// segment: it must be non-empty, not a traversal element ("." or ".."),
// at most maxSegmentBytes long, and consist solely of the permitted
// characters ([A-Za-z0-9._-]). Empty, ".", and ".." are rejected so the
// guarantee holds even if the input surface is broadened in the future
// (e.g. accepting repos from an HTTP request instead of env vars).
func IsSafeURLSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > maxSegmentBytes {
		return false
	}
	return safeSegment.MatchString(s)
}
