// Package ghcr is the GitHub Container Registry source for registry-stats.
// It collects per-package download counts by scraping the public
// github.com package pages (GHCR has no unauthenticated API for this
// data). A wildcard owner is expanded through the owner's packages
// listing, which is read page by page in the form the account kind uses:
// a user's at /<owner>?tab=packages, an organization's at
// /orgs/<owner>/packages.
//
// errHTMLFormatChanged is the single sentinel returned for any parse
// failure; Client classifies it with errors.Is and emits the dedicated
// format-drift ERRORs when a listing, or a majority of package scrapes,
// crosses the threshold.
//
// The package exposes a *Client (for composition-root wiring via
// collect.Source). The HTML-handling internals are unexported and every
// collection flow goes through *Client.Collect.
package ghcr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
	"github.com/cplieger/runesafe/v2"
)

// errHTMLFormatChanged is the sentinel returned for any GHCR HTML parse
// failure — missing "Total downloads" marker, a count element that is not
// the marker's own, a non-numeric or negative count, a first listing page
// with no package links. Callers compare via errors.Is to distinguish
// parse drift from transport errors.
var errHTMLFormatChanged = errors.New("GHCR HTML format changed")

// maxTitleDistance caps the scan window after the "Total downloads"
// marker. A count element further out belongs to something else.
const maxTitleDistance = 500

// ghcrBodyCap is the LimitReader cap for GitHub HTML responses. 2 MB
// comfortably fits every real package page; larger responses are a
// format signal, not legitimate content.
const ghcrBodyCap = 2 << 20

// maxListingPages bounds how many pages of one owner's packages listing a
// cycle reads, mirroring internal/dockerhub's owner-listing bound.
const maxListingPages = 10

// lastPageMarker is GitHub's own positive end-of-listing signal on a
// paginated packages page. Only its PRESENCE stops the loop: were the
// class renamed, the loop degrades to one extra fetch and a clean stop
// instead of the silent truncation that absence-based logic would cause.
const lastPageMarker = "next_page disabled"

// ownerKind is the account form an owner's packages are read through.
// GitHub paginates a user's listing under /<owner>?tab=packages and an
// organization's under /orgs/<owner>/packages, and each page's package
// links carry a matching prefix.
type ownerKind int

const (
	userOwner ownerKind = iota
	orgOwner
)

// listingURL builds the listing URL for one owner kind and 1-based page.
// The user form is what /users/<owner>/packages redirects to, and the
// redirect drops the query — requesting the redirecting form would make
// every page re-fetch page 1.
func listingURL(kind ownerKind, owner string, page int) string {
	if kind == orgOwner {
		return fmt.Sprintf("https://github.com/orgs/%s/packages?page=%d", owner, page)
	}
	return fmt.Sprintf("https://github.com/%s?tab=packages&page=%d", owner, page)
}

// linkPrefix is the package-link prefix a listing page of this kind carries.
func linkPrefix(kind ownerKind, owner string) string {
	if kind == orgOwner {
		return fmt.Sprintf("/orgs/%s/packages/container/package/", owner)
	}
	return fmt.Sprintf("/users/%s/packages/container/package/", owner)
}

// fetchHTML fetches a GitHub HTML page, spacing consecutive GHCR requests
// by c.pacingDelay first (Docker Hub runs immediately before). httpx
// retries 429, 5xx and transient transport errors (timeouts, connection
// resets, DNS failures) per c.opts.RetryOpts; other non-2xx statuses and
// non-transient transport errors fail fast. The appended browser headers
// (anonymous GHCR pages gate on User-Agent) and ghcrBodyCap always win,
// because options are applied left to right.
func (c *Client) fetchHTML(ctx context.Context, pageURL string) (string, error) {
	timer := time.NewTimer(c.pacingDelay())
	select {
	case <-ctx.Done():
		timer.Stop()
		return "", ctx.Err()
	case <-timer.C:
	}

	// Build a fresh slice so c.opts.RetryOpts, reused across every request,
	// is never mutated by the append.
	htmlOpts := make([]httpx.GetOption, 0, len(c.opts.RetryOpts)+2)
	htmlOpts = append(htmlOpts, c.opts.RetryOpts...)
	htmlOpts = append(htmlOpts,
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
			req.Header.Set("Accept", "text/html")
			req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		}),
		httpx.WithMaxBodyBytes(ghcrBodyCap),
	)
	body, err := httpx.GetBytes(ctx, c.http, pageURL, htmlOpts...)
	if err != nil {
		// An over-cap page is a format signal, not a transport error, so it
		// takes the format-drift path with the typed error still unwrappable.
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return "", fmt.Errorf("%w: %w", errHTMLFormatChanged, err)
		}
		return "", err
	}
	return string(body), nil
}

// refusals summarises the package names a listing's charset gate refused:
// how many, and one of them. A count plus one sample bounds all three
// quantities a hostile page can inflate — retained bytes, record count and
// record size — where a slice of refused names bounds none.
type refusals struct {
	Count  int
	Sample string
}

// merge folds another page's refusals in, keeping the first sample.
func (r refusals) merge(other refusals) refusals {
	if r.Count == 0 {
		r.Sample = other.Sample
	}
	r.Count += other.Count
	return r
}

// scrapePackageList reads one owner's packages listing and returns the
// package names in listing order plus what the charset gate refused. A
// first-page failure, and a first page with no package links, yield no
// names and an error; a later page's failure returns the names collected
// so far alongside it.
func (c *Client) scrapePackageList(ctx context.Context, owner string) ([]string, refusals, error) {
	names, refused, err := c.readListing(ctx, owner, userOwner)
	if err != nil || len(names) > 0 {
		return names, refused, err
	}

	// A user-form page with no package links does not say whether this owner
	// is an organization, so the org form settles the KIND — and only that: a
	// 404 there proves the owner is a user and leaves its zero links as
	// ambiguous as before, which is what emptyListingError reports.
	orgNames, orgRefused, orgErr := c.readListing(ctx, owner, orgOwner)
	switch {
	case orgErr == nil && len(orgNames) > 0:
		return orgNames, orgRefused, nil
	case orgErr == nil:
		return nil, orgRefused, emptyListingError(owner)
	case isNotFound(orgErr):
		return nil, refused, emptyListingError(owner)
	default:
		return nil, refused, orgErr
	}
}

// readListing walks one owner's listing in kind's URL form from page 1 up
// to the cap. A page after the first that adds no name is the end of the
// listing; one that FAILS is a partial listing, returned with the names
// already collected. Exhausting the cap while names still arrive is a
// truncated listing. Both of those states WARN, because alerts.yaml keys
// on their message text.
func (c *Client) readListing(ctx context.Context, owner string, kind ownerKind) ([]string, refusals, error) {
	var (
		names   []string
		refused refusals
	)
	seen := make(map[string]bool)
	maxPages := c.listingPageCap()
	for page := 1; page <= maxPages; page++ {
		html, err := c.fetchHTML(ctx, listingURL(kind, owner, page))
		if err != nil {
			if len(names) == 0 {
				return nil, refused, err
			}
			c.opts.Logger.Warn("ghcr owner listing partially failed",
				"owner", owner, "packages", len(names), "page", page, "error", err)
			return names, refused, err
		}
		pageNames, pageRefused := parsePackageList(html, owner, kind)
		refused = refused.merge(pageRefused)
		added := 0
		for _, name := range pageNames {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
			added++
		}
		if added == 0 || strings.Contains(html, lastPageMarker) {
			return names, refused, nil
		}
	}
	c.opts.Logger.Warn("ghcr owner listing hit page cap; results may be truncated",
		"owner", owner, "max_pages", maxPages)
	return names, refused, nil
}

// emptyListingError names both causes of a first listing page with no
// package links, the checkable one first, behind the format sentinel every
// caller compares with errors.Is.
func emptyListingError(owner string) error {
	return fmt.Errorf("%w: no public container packages found for %s - check the owner name, or the listing markup changed",
		errHTMLFormatChanged, owner)
}

// isNotFound reports whether err is GitHub answering 404, which on the
// organization listing form means the owner is a user account.
func isNotFound(err error) bool {
	if status, ok := errors.AsType[*httpx.StatusError](err); ok {
		return status.Code == http.StatusNotFound
	}
	return false
}

// packageListParser accumulates the deduplicated, URL-safe package names
// scraped from one packages page. It carries the line scan's mutable
// accounting (seen, names and the refusal summary) so the line loop in
// parsePackageList stays flat.
type packageListParser struct {
	prefix  string
	seen    map[string]bool
	names   []string
	refused refusals
}

// scanLine extracts every package-link name on one line of HTML,
// appending new safe names to p.names. Names that are not safe URL
// segments are counted in p.refused instead; duplicates are dropped.
func (p *packageListParser) scanLine(line string) {
	for {
		idx := strings.Index(line, p.prefix)
		if idx == -1 {
			return
		}
		line = line[idx+len(p.prefix):]
		end := strings.IndexAny(line, `"'<>`)
		if end == -1 {
			return
		}
		name := line[:end]
		if !urlsafe.IsSafeURLSegment(name) {
			if p.refused.Count == 0 {
				p.refused.Sample = name
			}
			p.refused.Count++
			continue
		}
		if !p.seen[name] {
			p.seen[name] = true
			p.names = append(p.names, name)
		}
	}
}

// parsePackageList extracts package names from one page of an owner's
// packages listing, matching kind's package-link prefix and filtering
// names through urlsafe.IsSafeURLSegment so a crafted page cannot smuggle
// path traversal into downstream URL construction. Duplicates (one page
// can list a package twice) are dropped in insertion order. Zero names is
// not an error here: what it means depends on the page number, which only
// the caller knows.
func parsePackageList(html, owner string, kind ownerKind) ([]string, refusals) {
	p := packageListParser{
		prefix: linkPrefix(kind, owner),
		seen:   make(map[string]bool),
	}
	for line := range strings.SplitSeq(html, "\n") {
		p.scanLine(line)
	}
	return p.names, p.refused
}

// scrapeDownloads fetches a single package page and returns its total
// download count. Non-2xx responses and transport errors bubble up as
// httpx.GetBytes returned them; parse failures return errHTMLFormatChanged.
func (c *Client) scrapeDownloads(ctx context.Context, owner, pkg string) (int64, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", owner, pkg)
	html, err := c.fetchHTML(ctx, pageURL)
	if err != nil {
		return 0, err
	}
	return parseDownloads(html)
}

// parseDownloads extracts the download count from a single package page:
// the title attribute of the <h3> element immediately following the
// "Total downloads" marker element, whitespace skipped. Anything else
// there, or a second titled element within maxTitleDistance, means the
// count can no longer be associated with the marker by position, so it
// fails closed with errHTMLFormatChanged rather than publishing a number
// that may belong to another element. Line boundaries are deliberately
// not meaningful: GitHub reflows whitespace without breaking this.
func parseDownloads(html string) (int64, error) {
	markerIdx := strings.Index(html, "Total downloads")
	if markerIdx == -1 {
		return 0, errHTMLFormatChanged
	}
	rest := html[markerIdx:]
	window := rest[:min(len(rest), maxTitleDistance)]

	markerEnd := strings.Index(window, ">")
	if markerEnd == -1 {
		return 0, errHTMLFormatChanged
	}
	afterMarker := window[markerEnd+1:]
	countIdx := markerEnd + 1 + len(afterMarker) - len(strings.TrimLeft(afterMarker, " \t\r\n"))
	if countIdx >= len(window) || !strings.HasPrefix(rest[countIdx:], "<h3") {
		return 0, errHTMLFormatChanged
	}

	// The start tag is bounded in the UNTRUNCATED remainder, so a tag that
	// opens inside the window and closes past it still parses.
	tag := rest[countIdx:]
	tagEnd := strings.Index(tag, ">")
	if tagEnd == -1 {
		return 0, errHTMLFormatChanged
	}
	tag = tag[:tagEnd]
	titleIdx := strings.Index(tag, `title="`)
	if titleIdx == -1 || strings.Count(window, `title="`) > 1 {
		return 0, errHTMLFormatChanged
	}
	raw, _, ok := strings.Cut(tag[titleIdx+len(`title="`):], `"`)
	if !ok {
		return 0, errHTMLFormatChanged
	}
	count, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: parse count: %w", errHTMLFormatChanged, err)
	}
	if count < 0 {
		return 0, fmt.Errorf("%w: negative download count: %d", errHTMLFormatChanged, count)
	}
	return count, nil
}

// expandWildcard scrapes one wildcard owner's packages listing and appends
// each new (deduplicated) package ref to packages. listingWhollyFailed is
// true when the listing errored with no names at all: an unknown number of
// packages went uncollected, which the health verdict must see. A cancelled
// cycle is a stop, not an outage, and sets neither return.
func (c *Client) expandWildcard(
	ctx context.Context,
	ref registry.RepoRef,
	seen map[registry.RepoRef]bool,
	packages []registry.RepoRef,
) (out []registry.RepoRef, listingWhollyFailed bool, listingParseFailures int) {
	names, refused, err := c.scrapePackageList(ctx, ref.Owner)
	if refused.Count > 0 {
		// Bounded at the emit site: the sample is a raw scraped string, and
		// 128 bytes is above any legitimate GHCR package name.
		c.opts.Logger.Debug("ghcr listing refused package names with unsafe characters",
			"owner", ref.Owner, "refused", refused.Count,
			"sample", runesafe.SanitizeSingleLineBounded(refused.Sample, 128))
	}
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown/deadline cancelled the listing scrape; expected, not a
			// failure. Logging it at ERROR would fire a false alert on the
			// level=error stream on every SIGTERM landing mid-listing.
			c.opts.Logger.Debug("ghcr package listing cancelled", "owner", ref.Owner, "error", err)
			return packages, false, 0
		}
		c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner, "error", err)
		if errors.Is(err, httpx.ErrRateLimited) {
			c.opts.Logger.Warn("ghcr listing rate limited", "owner", ref.Owner,
				"hint", "consider increasing pacing delay")
		}
		wholly := len(names) == 0
		if errors.Is(err, errHTMLFormatChanged) {
			return c.appendPackages(names, ref.Owner, seen, packages), wholly, 1
		}
		return c.appendPackages(names, ref.Owner, seen, packages), wholly, 0
	}
	packages = c.appendPackages(names, ref.Owner, seen, packages)
	c.opts.Logger.Info("ghcr wildcard expanded", "owner", ref.Owner, "packages", len(names))
	return packages, false, 0
}

// appendPackages adds every not-yet-seen (owner, name) pair to packages.
func (c *Client) appendPackages(
	names []string,
	owner string,
	seen map[registry.RepoRef]bool,
	packages []registry.RepoRef,
) []registry.RepoRef {
	for _, name := range names {
		ref := registry.RepoRef{Owner: owner, Repo: name}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		packages = append(packages, ref)
	}
	return packages
}

// buildPackageList expands wildcard refs (owner/*) by scraping the owner's
// packages listing, then appends explicit refs unless already covered by a
// wildcard. Both passes key the shared `seen` map on the (owner, package)
// pair itself, so no encoding is shared and the two cannot disagree about
// one package. listingWhollyFailed is true when any owner's listing errored
// with no names; listingParseFailures counts the ones that were format drift.
func (c *Client) buildPackageList(
	ctx context.Context,
	refs []registry.RepoRef,
) (packages []registry.RepoRef, listingWhollyFailed bool, listingParseFailures int) {
	seen := make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		var wholly bool
		var pf int
		packages, wholly, pf = c.expandWildcard(ctx, ref, seen, packages)
		listingWhollyFailed = listingWhollyFailed || wholly
		listingParseFailures += pf
	}
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		packages = append(packages, ref)
	}
	return packages, listingWhollyFailed, listingParseFailures
}
