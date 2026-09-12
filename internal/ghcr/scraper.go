// Package ghcr is the GitHub Container Registry source for registry-stats.
// It collects per-package download counts by scraping the public
// github.com package pages (GHCR has no unauthenticated API for this
// data). A wildcard owner is expanded through the owner's packages
// listing, read page by page in the form the account kind uses: a user's
// at /<owner>?tab=packages, an organization's at /orgs/<owner>/packages.
//
// errHTMLFormatChanged is the single sentinel returned for any parse
// failure; Client classifies it with errors.Is and emits dedicated
// format-drift ERRORs when a listing, or a majority of package scrapes,
// crosses the threshold.
package ghcr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
	"github.com/cplieger/runesafe/v2"
)

var (
	// errHTMLFormatChanged is the sentinel for GHCR HTML parse failures.
	// Callers compare via errors.Is to distinguish parse drift from transport
	// errors.
	errHTMLFormatChanged = errors.New("GHCR HTML format changed")
	errEmptyListing      = errors.New("GHCR package listing was empty")
)

const htmlWhitespace = " \t\n\f\r"

// ghcrBodyCap is the LimitReader cap for GitHub HTML responses; a larger
// response is a format signal, not legitimate content.
const ghcrBodyCap = 2 << 20

// userAgent identifies this client to github.com. Both pages this package
// reads are served to any agent, so the header is a contact address rather
// than a gate.
const userAgent = "registry-stats (+https://github.com/cplieger/registry-stats)"

// maxListingCandidates bounds how many package label sets one listing
// response can mint: every accepted name becomes an image_pulls_total
// label triple. It is checked before the printed-count comparison
// because that comparison is skipped when a page prints no usable count,
// leaving this the only bound on the count for such a page. 100 is 3.3x
// the thirty links GitHub serves; the largest page measured carried 30.
const maxListingCandidates = 100

// maxListingPages bounds a listing that keeps serving new package names.
// Fifty is a round limit above the largest measured total of 23 pages.
const maxListingPages = 50

// statedPackagesMarker identifies the printed package count for one listing
// page; GitHub prints "1 package" and otherwise "N packages".
const statedPackagesMarker = "package"

// maxRefusalSampleBytes bounds both the retained page slice and the sanitized
// log attribute. The cap at the sample's own site is what bounds the retained
// sample; runesafe.SanitizeSingleLineBounded bounds the log attribute on its
// own, because it caps the sanitized form.
const maxRefusalSampleBytes = 128

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
// The ecosystem filter keeps the listing links and its printed package count
// scoped to container packages. The organization form does not redirect. For
// an organization, the user form redirects to a profile page and drops the
// listing query, so callers probe the organization form first.
func listingURL(kind ownerKind, owner string, page int) string {
	if kind == orgOwner {
		return fmt.Sprintf("https://github.com/orgs/%s/packages?ecosystem=container&page=%d", owner, page)
	}
	return fmt.Sprintf("https://github.com/%s?tab=packages&ecosystem=container&page=%d", owner, page)
}

// linkPrefix is the package-link prefix a listing page of this kind carries.
func linkPrefix(kind ownerKind, owner string) string {
	if kind == orgOwner {
		return fmt.Sprintf("/orgs/%s/packages/container/package/", owner)
	}
	return fmt.Sprintf("/users/%s/packages/container/package/", owner)
}

// fetchHTML fetches a GitHub HTML page, spacing requests by the production
// pacing interval after the first of the cycle. httpx retries 408, 429, 5xx
// and transient transport errors with its defaults; other non-2xx statuses
// fail fast.
func (c *Client) fetchHTML(ctx context.Context, p *pacer, pageURL string) (string, error) {
	if err := p.wait(ctx); err != nil {
		return "", err
	}

	body, err := httpx.GetBytes(ctx, c.http, pageURL,
		httpx.WithLogger(c.opts.Logger),
		httpx.WithExhaustedLevel(slog.LevelDebug),
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Accept", "text/html")
		}),
		httpx.WithMaxBodyBytes(ghcrBodyCap),
	)
	if err != nil {
		// An over-cap page is a format signal, not a transport error.
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return "", fmt.Errorf("%w: %w", errHTMLFormatChanged, err)
		}
		return "", err
	}
	return string(body), nil
}

// refusals summarises listing candidates whose name urlsafe.PackageName
// rejects. Sample is one such candidate if any carried bytes, bounded by
// maxRefusalSampleBytes and detached from the listing page.
type refusals struct {
	Sample string
	Count  int
}

// merge folds another page's refusals in, keeping the first non-empty sample.
func (r refusals) merge(other refusals) refusals {
	if r.Sample == "" {
		r.Sample = other.Sample
	}
	r.Count += other.Count
	return r
}

// refuse counts one rejected candidate, keeping the first informative sample
// bounded and detached from the page.
func (r refusals) refuse(candidate string) refusals {
	if r.Sample == "" {
		r.Sample = strings.Clone(runesafe.CapBytes(candidate, maxRefusalSampleBytes))
	}
	r.Count++
	return r
}

// scrapePackageList reads one owner's packages listing and returns the
// package names in listing order plus the candidates it could not use. An
// error is returned when neither account form yields names; a later-page
// failure returns names already collected alongside the error.
func (c *Client) scrapePackageList(ctx context.Context, p *pacer, owner string) ([]string, refusals, error) {
	orgNames, refused, orgErr := c.readListing(ctx, p, owner, orgOwner)
	if len(orgNames) > 0 || errors.Is(orgErr, errEmptyListing) {
		return orgNames, refused, orgErr
	}

	// The organization form is the kind probe: names or a listing stating zero
	// packages identify an organization and a 404 identifies a user. If it
	// yields neither, the user form is still tried so an organization-form
	// failure cannot hide a valid user listing.
	names, userRefused, err := c.readListing(ctx, p, owner, userOwner)
	refused = refused.merge(userRefused)
	switch {
	case len(names) > 0:
		return names, refused, err
	case orgErr != nil && !isNotFound(orgErr):
		return nil, refused, orgErr
	case err == nil:
		return nil, refused, emptyListingError(owner)
	default:
		return nil, refused, err
	}
}

// errTextForLog bounds an upstream error string in a log attribute: long enough
// to diagnose a GitHub failure, short enough that one upstream message cannot
// dominate the alert-keyed line.
func errTextForLog(err error) string {
	return runesafe.SanitizeSingleLineBounded(err.Error(), 256)
}

// partialListing returns a failed page with the names already collected. It
// levels the record by cause, as the wholesale arm does: markup drift is
// actionable and reads on the level=ERROR stream, anything else is a WARN.
func (c *Client) partialListing(ctx context.Context, owner string, page int, names []string, refused refusals, err error) ([]string, refusals, error) {
	if len(names) > 0 && ctx.Err() == nil {
		level := slog.LevelWarn
		if errors.Is(err, errHTMLFormatChanged) {
			level = slog.LevelError
		}
		c.opts.Logger.Log(ctx, level, "ghcr owner listing partially failed",
			"owner", owner, "packages", len(names), "page", page,
			"error", errTextForLog(err))
	}
	return names, refused, err
}

// readListing walks one owner's listing in kind's URL form from page 1 up
// to the cap. A page with no new package name ends the listing and is graded by
// emptyPageError. Each usable printed package count is checked against that
// page's distinct names plus refusals. Refused candidates make a page partial
// only if it adds no usable name; mixed pages continue and report refusals.
// Other early stops return collected names. A non-cancelled partial is logged
// at the cause's level; the cap always warns.
// Each page is an independent GET against a listing GitHub may change
// between them, so one cycle's enumeration is a snapshot and not a
// transaction. A package shifted backward across the read cursor by a
// mid-walk delete is absent for that cycle with no signal available, and
// one deleted mid-walk may still be published for it; a mid-walk insert
// before the cursor leaves the new package absent for the same reason and
// re-serves the name it displaced at the top of the next page, where a
// checked printed count above that page's added names counts it and this
// walk does not act on it. All three self-heal at the next poll, and an
// absent series already means "not measured this cycle" per obs.SetImage's
// retirement contract.
func (c *Client) readListing(ctx context.Context, p *pacer, owner string, kind ownerKind) ([]string, refusals, error) {
	var (
		names   []string
		refused refusals
	)
	seen := make(map[string]bool)
	for page := 1; page <= maxListingPages; page++ {
		html, err := c.fetchHTML(ctx, p, listingURL(kind, owner, page))
		if err != nil {
			return c.partialListing(ctx, owner, page, names, refused, err)
		}
		pageNames, pageRefused := parsePackageList(html, owner, kind)
		refused = refused.merge(pageRefused)
		stated, statedOK, err := listingPagePopulation(html, page, pageNames, pageRefused)
		if err != nil {
			return c.partialListing(ctx, owner, page, names, refused, err)
		}
		kept, added := appendUnseenNames(names, seen, pageNames)
		names = kept
		acct := pageAccounting{
			page:     page,
			added:    added,
			names:    pageNames,
			refused:  pageRefused,
			stated:   stated,
			statedOK: statedOK,
		}
		complete, err := c.listingPageComplete(owner, &acct)
		if err != nil {
			return c.partialListing(ctx, owner, page, names, refused, err)
		}
		if complete {
			if len(names) == 0 && acct.statedOK && acct.stated == 0 {
				return nil, refused, confirmedEmptyListingError(owner)
			}
			return names, refused, nil
		}
	}
	c.opts.Logger.Warn("ghcr owner listing hit page cap; results truncated",
		"owner", owner, "max_pages", maxListingPages)
	return names, refused, nil
}

func listingPagePopulation(html string, page int, names []string, refused refusals) (stated int, statedOK bool, err error) {
	candidates := len(names) + refused.Count
	if candidates > maxListingCandidates {
		return 0, false, fmt.Errorf("%w: listing page %d carried %d package candidates", errHTMLFormatChanged, page, candidates)
	}
	stated, statedOK = statedPackages(html)
	if !statedOK {
		return stated, false, nil
	}
	accounted := len(slices.Compact(slices.Sorted(slices.Values(names)))) + refused.Count
	if stated != accounted {
		return stated, true, fmt.Errorf("%w: listing page %d states %d packages against %d accounted for",
			errHTMLFormatChanged, page, stated, accounted)
	}
	return stated, true, nil
}

type pageAccounting struct {
	names    []string
	refused  refusals
	page     int
	added    int
	stated   int
	statedOK bool
}

func (c *Client) listingPageComplete(owner string, acct *pageAccounting) (bool, error) {
	if !acct.statedOK {
		// The printed count is the only proof this page was read whole;
		// without it the walk publishes whatever it found.
		c.opts.Logger.Warn("ghcr listing page states no usable package count; completeness unchecked",
			"owner", owner, "page", acct.page)
	}
	if acct.added == 0 {
		return true, emptyPageError(acct)
	}
	return false, nil
}

// emptyPageError names why a listing page that added no new name is a format
// change, or returns nil when the page is the listing's clean end.
func emptyPageError(acct *pageAccounting) error {
	switch {
	case acct.refused.Count > 0:
		return fmt.Errorf("%w: listing page %d yielded no usable package names", errHTMLFormatChanged, acct.page)
	case len(acct.names) > 0:
		return fmt.Errorf("%w: listing page %d re-served %d names already collected", errHTMLFormatChanged, acct.page, len(acct.names))
	}
	return nil
}

// appendUnseenNames appends every name not already in seen and reports how
// many it added so the caller can distinguish progress from an empty or
// repeated page.
func appendUnseenNames(names []string, seen map[string]bool, pageNames []string) (kept []string, added int) {
	kept = names
	for _, name := range pageNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		kept = append(kept, name)
		added++
	}
	return kept, added
}

// confirmedEmptyListingError reports an owner whose listing page itself
// states zero container packages, so neither of emptyListingError's causes applies.
func confirmedEmptyListingError(owner string) error {
	return fmt.Errorf("%w: %s has no public container packages (the listing states 0)", errEmptyListing, owner)
}

// emptyListingError names both causes of a first listing page with no
// package links, the checkable one first, behind the format sentinel.
func emptyListingError(owner string) error {
	return fmt.Errorf("%w: %w: no public container packages found for %s - check the owner name, or the listing markup changed",
		errHTMLFormatChanged, errEmptyListing, owner)
}

// isNotFound reports whether err is GitHub answering 404, which on the
// organization listing form means the owner is a user account.
func isNotFound(err error) bool {
	if status, ok := errors.AsType[*httpx.StatusError](err); ok {
		return status.Code == http.StatusNotFound
	}
	return false
}

// parsePackageList extracts package names from one owner's listing page.
// The caller compares the page's printed package count against the set read
// and refuses the page on a mismatch. A package name comes from a double-quoted
// href whose value starts with this account kind's package-link prefix.
// Registered owner casing is response data, so the prefix is matched without
// transforming untrusted HTML. Zero names is not an error here because its
// meaning depends on the page number, which only the caller knows.
func parsePackageList(html, owner string, kind ownerKind) (names []string, refused refusals) {
	prefix := linkPrefix(kind, owner)
	const hrefOpen = `href="`
	for rest := html; ; {
		hrefAt := strings.Index(rest, hrefOpen)
		if hrefAt < 0 {
			return names, refused
		}
		if hrefAt == 0 || !strings.ContainsRune(htmlWhitespace, rune(rest[hrefAt-1])) {
			rest = rest[hrefAt+len(hrefOpen):]
			continue
		}
		rest = rest[hrefAt+len(hrefOpen):]
		value, after, ok := strings.Cut(rest, `"`)
		if !ok {
			return names, refused
		}
		rest = after
		if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
			continue
		}
		raw := value[len(prefix):]
		pkg, nameErr := urlsafe.PackageName(urlsafe.Owner(owner), raw)
		if nameErr != nil {
			refused = refused.refuse(raw)
			continue
		}
		names = append(names, strings.Clone(pkg))
	}
}

// statedPackages reports the package count a listing page publishes for
// itself, and whether the page stated one usably. The count is per page, so it
// grades that page's population. An absent or ambiguous marker is not usable
// and is not drift: a heading rename upstream must make this signal go quiet,
// not fail every poll of every owner.
func statedPackages(html string) (n int, ok bool) {
	markers := 0
	for at := 0; ; {
		i := strings.Index(html[at:], statedPackagesMarker)
		if i < 0 {
			return n, markers == 1
		}
		i += at
		if count, found := statedPackageCount(html, i); found {
			if markers == 0 {
				n = count
			}
			markers++
		}
		at = i + len(statedPackagesMarker)
	}
}

func statedPackageCount(html string, markerAt int) (int, bool) {
	before := strings.TrimRight(html[:markerAt], htmlWhitespace)
	after, _ := strings.CutPrefix(html[markerAt+len(statedPackagesMarker):], "s")
	after = strings.TrimLeft(after, htmlWhitespace)
	if !strings.HasPrefix(after, "</") {
		return 0, false
	}
	digitStart := len(before)
	for digitStart > 0 && '0' <= before[digitStart-1] && before[digitStart-1] <= '9' {
		digitStart--
	}
	if digitStart == 0 || digitStart == len(before) || before[digitStart-1] != '>' {
		return 0, false
	}
	count, err := strconv.Atoi(before[digitStart:])
	return count, err == nil
}

// parseDownloads extracts the download count from a single package page: the
// title attribute of the <h3> element immediately following the page's one
// "Total downloads" marker element. Anything else there, including an ambiguous
// marker, fails closed with errHTMLFormatChanged rather than publishing a
// number that may belong to another element.
func parseDownloads(html string) (int64, error) {
	const marker = ">Total downloads</span>"
	markers := strings.Count(html, marker)
	if markers != 1 {
		return 0, fmt.Errorf("%w: %d download-count markers", errHTMLFormatChanged, markers)
	}
	rest := strings.TrimLeft(html[strings.Index(html, marker)+len(marker):], htmlWhitespace)
	const countOpen = `<h3 title="`
	if !strings.HasPrefix(rest, countOpen) {
		return 0, fmt.Errorf("%w: the element after the marker is not %s", errHTMLFormatChanged, countOpen)
	}
	raw, _, ok := strings.Cut(rest[len(countOpen):], `"`)
	if !ok {
		return 0, fmt.Errorf("%w: count title attribute does not close", errHTMLFormatChanged)
	}
	count, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("%w: parse count: %w", errHTMLFormatChanged, err)
	}
	return int64(count), nil
}

// expandWildcard scrapes one wildcard owner's packages listing and appends
// each new (deduplicated) package ref to packages. listingWhollyFailed is
// true when the listing errored with no names at all: an unknown number of
// packages went uncollected, which the health verdict must see. A cancelled
// cycle is a stop, not an outage, and sets neither return.
func (c *Client) expandWildcard(
	ctx context.Context,
	p *pacer,
	ref registry.RepoRef,
	seen map[registry.RepoRef]bool,
	packages []registry.RepoRef,
) (out []registry.RepoRef, listingWhollyFailed bool) {
	out = packages
	names, refused, err := c.scrapePackageList(ctx, p, ref.Owner)
	if refused.Count > 0 {
		// A refused candidate is omitted every cycle, so report it at the default level.
		c.opts.Logger.Warn("ghcr listing refused package candidates",
			"owner", ref.Owner, "refused", refused.Count,
			"sample", runesafe.SanitizeSingleLineBounded(refused.Sample, maxRefusalSampleBytes))
	}
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown/deadline cancelled the listing scrape; expected, not a
			// failure. Logging it at ERROR would fire a false alert on the
			// level=error stream on every SIGTERM landing mid-listing.
			c.opts.Logger.Debug("ghcr package listing cancelled", "owner", ref.Owner,
				"error", errTextForLog(err))
			return out, false
		}
		// A listing that states zero container packages is a definitive
		// upstream answer, not an unread listing.
		confirmedEmpty := errors.Is(err, errEmptyListing) && !errors.Is(err, errHTMLFormatChanged)
		listingWhollyFailed = len(names) == 0 && !confirmedEmpty
		if listingWhollyFailed {
			switch {
			case errors.Is(err, errHTMLFormatChanged) && !errors.Is(err, errEmptyListing):
				c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner,
					"error", errTextForLog(err),
					"report_at", "https://github.com/cplieger/registry-stats/issues")
			default:
				c.opts.Logger.Warn("ghcr package listing failed", "owner", ref.Owner,
					"error", errTextForLog(err))
			}
		} else if confirmedEmpty {
			c.opts.Logger.Warn("ghcr owner has no public container packages",
				"owner", ref.Owner)
		}
		return c.appendPackages(names, ref.Owner, seen, out), listingWhollyFailed
	}
	out = c.appendPackages(names, ref.Owner, seen, out)
	c.opts.Logger.Info("ghcr wildcard expanded", "owner", ref.Owner, "packages", len(names))
	return out, false
}

// appendPackages adds every not-yet-seen (owner, name) pair to packages.
func (*Client) appendPackages(
	names []string,
	owner string,
	seen map[registry.RepoRef]bool,
	packages []registry.RepoRef,
) []registry.RepoRef {
	for _, name := range names {
		ref := registry.RepoRef{Owner: owner, Repo: strings.ToLower(name)}
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
// wildcard. Wildcard refs are folded to the config-canonical explicit spelling,
// so a case-preserving listing still covers its explicit twin.
// listingWhollyFailed is true when any owner's listing errored with no names.
func (c *Client) buildPackageList(
	ctx context.Context,
	p *pacer,
	refs []registry.RepoRef,
) (packages []registry.RepoRef, listingWhollyFailed bool) {
	seen := make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		var wholly bool
		packages, wholly = c.expandWildcard(ctx, p, ref, seen, packages)
		listingWhollyFailed = listingWhollyFailed || wholly
	}
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		if seen[ref] {
			continue
		}
		packages = append(packages, ref)
	}
	return packages, listingWhollyFailed
}
