// Package urlsafe holds URL path segment safety predicates. It depends
// only on the standard library and on no other internal/* package so
// it can be imported from any layer (config, scrapers, handlers) without
// risking an import cycle. The predicate here is the single source of
// truth for "is this string safe to embed in a URL path segment"; every
// caller that builds a URL from external input (env vars today, HTTP
// request bodies tomorrow) routes through this function.
package urlsafe

import "strings"

// IsSafeURLSegment returns true if s contains no characters that could
// break URL path construction or enable path traversal. Empty, ".", and
// ".." are rejected so the function's guarantee holds even if the input
// surface is broadened in the future (e.g. accepting repos from an HTTP
// request instead of env vars).
func IsSafeURLSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/%\\?#@:")
}
