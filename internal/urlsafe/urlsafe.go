// Package urlsafe holds URL path segment safety predicates.
package urlsafe

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MaxSegmentBytes is the container-reference grammar's 255-byte repository-path
// limit, measured over the slash-joined path excluding the registry host.
// IsSafeURLSegment applies it per segment; CheckReference applies it to
// owner/name as a whole. It does not bound the percent-escaped form a caller
// may encode into one segment.
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

// Owner is a registry owner segment as supplied by a caller that has already
// accepted it with IsSafeURLSegment.
type Owner string

// CheckReference reports the refusing rule when the slash-joined owner/name
// repository path exceeds MaxSegmentBytes. config.repoName calls it on both
// registry arms, so the bound is one rule with one wording rather than a
// per-registry copy; a caller that discovers a name elsewhere applies it
// itself or does not.
func CheckReference(owner Owner, name string) error {
	if len(owner)+1+len(name) > MaxSegmentBytes {
		return fmt.Errorf("owner/repository reference over %d bytes", MaxSegmentBytes)
	}
	return nil
}

// PackageName decodes GHCR's one-token spelling of a package name. The
// decoded name may contain '/'-separated path elements. Callers must encode
// the returned name with url.PathEscape when placing it back into one URL path
// segment. Errors name the refusing rule for callers to report and are not
// sentinels. owner is read only for its length, as part of the whole-reference
// budget; the caller owns putting it through IsSafeURLSegment.
func PackageName(owner Owner, token string) (name string, err error) {
	if strings.Contains(token, "/") {
		return "", errors.New("raw slash; percent-encode nested names")
	}
	name, err = url.PathUnescape(token)
	if err != nil {
		return "", errors.New("invalid percent-escape")
	}
	if err := CheckReference(owner, name); err != nil {
		return "", err
	}
	for part := range strings.SplitSeq(name, "/") {
		if !IsSafeURLSegment(part) {
			return "", errors.New("path element not a safe URL segment")
		}
	}
	return name, nil
}
