// Package urlsafe holds URL path segment safety predicates.
package urlsafe

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MaxSegmentBytes bounds one decoded URL path segment. PackageName uses it
// to bound the whole decoded owner/name reference. It does not bound the
// percent-escaped form a caller may encode into one segment.
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

// PackageName decodes GHCR's one-token spelling of a package name. The
// decoded name may contain '/'-separated path elements. A caller placing it
// back into one URL path segment must encode it with url.PathEscape. Errors
// name the refusing rule for callers to report and are not sentinels.
func PackageName(owner, token string) (name string, err error) {
	if strings.Contains(token, "/") {
		return "", errors.New("raw slash; percent-encode nested names")
	}
	name, err = url.PathUnescape(token)
	if err != nil {
		return "", errors.New("invalid percent-escape")
	}
	if len(owner)+1+len(name) > MaxSegmentBytes {
		return "", fmt.Errorf("owner/repository reference over %d bytes", MaxSegmentBytes)
	}
	for part := range strings.SplitSeq(name, "/") {
		if !IsSafeURLSegment(part) {
			return "", errors.New("path element not a safe URL segment")
		}
	}
	return name, nil
}
