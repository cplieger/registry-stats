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
	"net/http"
	"net/url"
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

// maxListingCandidates bounds matching link occurrences plus refused
// candidates per page, counted before dedup; the largest measured page
// carried 30 candidates.
const maxListingCandidates = 100

// maxListingPages bounds how many pages of one owner's packages listing a
// cycle reads.
const maxListingPages = 10

// lastPageMarker is GitHub's own positive end-of-listing signal. Only its
// PRESENCE stops the loop: were the class renamed, the loop degrades to
// one extra fetch and a clean stop instead of silent truncation.
const lastPageMarker = "next_page disabled"

// maxRefusalSampleBytes bounds both the retained page slice and the sanitized
// log attribute. Both caps are needed because invalid UTF-8 expands when
// runesafe.SanitizeSingleLineBounded replaces it.
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

// fetchHTML fetches a GitHub HTML page, spacing requests by c.pacingDelay
// after the first of the cycle. httpx retries 429, 5xx and transient transport
// errors per c.opts.RetryOpts; other non-2xx statuses fail fast. The
// appended browser headers (anonymous GHCR pages gate on User-Agent) and
// ghcrBodyCap always win, because options are applied left to right.
func (c *Client) fetchHTML(ctx context.Context, p *pacer, pageURL string) (string, error) {
	if err := p.wait(ctx); err != nil {
		return "", err
	}

	// Fresh slice so c.opts.RetryOpts, reused across every request, is
	// never mutated by the append.
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
		// An over-cap page is a format signal, not a transport error.
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return "", fmt.Errorf("%w: %w", errHTMLFormatChanged, err)
		}
		return "", err
	}
	return string(body), nil
}

// refusals summarises listing candidates parsePackageList could not use:
// a path outside an href value, an unterminated href value, or a name
// urlsafe.PackageName rejects. Sample is the first candidate, bounded by
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
// package names in listing order plus the candidates it could not use. A
// first-page failure, and a first page with no package links, yield no
// names and an error; a later page's failure returns the names collected
// so far alongside it.
func (c *Client) scrapePackageList(ctx context.Context, p *pacer, owner string) ([]string, refusals, error) {
	names, refused, err := c.readListing(ctx, p, owner, userOwner)
	if err != nil || len(names) > 0 {
		return names, refused, err
	}

	// A user-form page with no package links does not say whether this owner
	// is an organization, so the org form settles the KIND — and only that: a
	// 404 there proves the owner is a user and leaves its zero links as
	// ambiguous as before, which is what emptyListingError reports.
	orgNames, orgRefused, orgErr := c.readListing(ctx, p, owner, orgOwner)
	// Past the guard above the user form yielded no names and no error, which
	// only its refusal-free first page does, so orgRefused is the only
	// non-empty diagnostic this function can return.
	switch {
	case orgErr == nil && len(orgNames) > 0:
		return orgNames, orgRefused, nil
	case orgErr == nil || (len(orgNames) == 0 && isNotFound(orgErr)):
		return nil, orgRefused, emptyListingError(owner)
	default:
		return orgNames, orgRefused, orgErr
	}
}

// partialListing returns a failed page with the names already collected. It
// warns for non-cancelled partial results because alerts/logql.yaml keys on the
// message text.
func (c *Client) partialListing(ctx context.Context, owner string, page int, names []string, refused refusals, err error) ([]string, refusals, error) {
	if len(names) > 0 && ctx.Err() == nil {
		c.opts.Logger.Warn("ghcr owner listing partially failed",
			"owner", owner, "packages", len(names), "page", page,
			"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
	}
	return names, refused, err
}

// readListing walks one owner's listing in kind's URL form from page 1 up
// to the cap. A page after the first that adds no name is the end of the
// listing unless it contains refusals, which make the listing untrustworthy.
// A page that fails is a partial listing, returned with the names already
// collected. Exhausting the cap while names still arrive is a truncated
// listing. Both states WARN, unless the cycle's own cancellation caused the
// failure, because alerts/logql.yaml keys on their message text.
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
		candidates := len(pageNames) + pageRefused.Count
		refused = refused.merge(pageRefused)
		if candidates > maxListingCandidates {
			return c.partialListing(ctx, owner, page, names, refused,
				fmt.Errorf("%w: listing page %d carried %d package candidates", errHTMLFormatChanged, page, candidates))
		}
		kept, added := appendUnseenNames(names, seen, pageNames)
		names = kept
		if added == 0 {
			if len(pageNames) == 0 && pageRefused.Count > 0 {
				return c.partialListing(ctx, owner, page, names, refused,
					fmt.Errorf("%w: listing page %d yielded no usable package names", errHTMLFormatChanged, page))
			}
			return names, refused, nil
		}
		if lastPageMarkerPresent(html) {
			return names, refused, nil
		}
	}
	c.opts.Logger.Warn("ghcr owner listing hit page cap; results may be truncated",
		"owner", owner, "max_pages", maxListingPages)
	return names, refused, nil
}

// appendUnseenNames appends every name not already in seen and reports how
// many it added. Zero added means the page carried nothing new, which is what
// ends the walk.
func appendUnseenNames(names []string, seen map[string]bool, pageNames []string) (kept []string, added int) {
	for _, name := range pageNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
		added++
	}
	return names, added
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

func markupTagEnd(html string, start int) (int, bool) {
	var quote byte
	for i := start + 1; i < len(html); i++ {
		switch {
		case quote != 0 && html[i] == quote:
			quote = 0
		case quote == 0 && (html[i] == '\'' || html[i] == '"'):
			quote = html[i]
		case quote == 0 && html[i] == '>':
			return i, true
		}
	}
	return 0, false
}

func quotedAttributeIs(html string, end int, name string) (byte, bool) {
	for _, quote := range []byte{'"', '\''} {
		suffix := name + "=" + string(quote)
		start := end - len(suffix)
		if start <= 0 || html[start:end] != suffix {
			continue
		}
		if html[start-1] == '<' || strings.ContainsRune(htmlWhitespace, rune(html[start-1])) {
			return quote, true
		}
	}
	return 0, false
}

func hrefAttribute(html string, end int) (byte, bool) {
	return quotedAttributeIs(html, end, "href")
}

func readAttribute(tag string, start int) (name, value string, next int, ok bool) {
	attrs := strings.TrimLeft(tag[start:], htmlWhitespace)
	if attrs == "" || attrs == "/" {
		return "", "", len(tag), true
	}

	nameEnd := strings.IndexAny(attrs, "="+htmlWhitespace)
	if nameEnd <= 0 {
		return "", "", 0, false
	}
	name = attrs[:nameEnd]
	attrs = strings.TrimLeft(attrs[nameEnd:], htmlWhitespace)
	if attrs == "" || attrs[0] != '=' {
		return "", "", 0, false
	}
	attrs = strings.TrimLeft(attrs[1:], htmlWhitespace)
	if attrs == "" || (attrs[0] != '\'' && attrs[0] != '"') {
		return "", "", 0, false
	}
	valueStart := len(tag) - len(attrs) + 1
	quote, validName := quotedAttributeIs(tag, valueStart, name)
	if !validName || quote != attrs[0] {
		return "", "", 0, false
	}
	attrs = attrs[1:]
	valueEnd := strings.IndexByte(attrs, quote)
	if valueEnd < 0 {
		return "", "", 0, false
	}
	value = attrs[:valueEnd]
	next = len(tag) - len(attrs) + valueEnd + 1
	if next < len(tag) && !strings.ContainsRune(htmlWhitespace+"/", rune(tag[next])) {
		return "", "", 0, false
	}
	return name, value, next, true
}

func containsMarkerFields(value string) bool {
	fields := strings.Fields(value)
	for marker := range strings.FieldsSeq(lastPageMarker) {
		if !slices.Contains(fields, marker) {
			return false
		}
	}
	return true
}

// tagHasMarkerClass reports whether one start tag's class attribute carries
// every token in lastPageMarker.
func tagHasMarkerClass(tag string) bool {
	attrAt := strings.IndexAny(tag, htmlWhitespace+"/")
	if attrAt < 0 {
		return false
	}
	for {
		name, value, next, valid := readAttribute(tag, attrAt)
		if !valid || name == "" {
			return false
		}
		if name == "class" && containsMarkerFields(value) {
			return true
		}
		attrAt = next
	}
}

// lastPageMarkerPresent reports whether a start tag's class token list carries
// every token in lastPageMarker. Text and unrelated attributes cannot answer it.
func lastPageMarkerPresent(html string) bool {
	for cursor := 0; cursor < len(html); {
		i := strings.IndexByte(html[cursor:], '<')
		if i < 0 {
			return false
		}
		open := cursor + i
		tagEnd, ok := markupTagEnd(html, open)
		if !ok {
			return false
		}
		cursor = tagEnd + 1
		// Skip an end tag, a comment and a processing instruction: only a start
		// tag carries the class attribute this answers from.
		if open+1 >= tagEnd || strings.ContainsRune("/!?", rune(html[open+1])) {
			continue
		}
		if tagHasMarkerClass(html[open:tagEnd]) {
			return true
		}
	}
	return false
}

// parsePackageList extracts package names from one page of an owner's
// packages listing. The registered owner casing is response data, so the
// prefix is matched without transforming untrusted HTML. Zero names is not
// an error here: what it means depends on the page number, which only the
// caller knows.
func parsePackageList(html, owner string, kind ownerKind) ([]string, refusals) {
	prefix := linkPrefix(kind, owner)
	root := "/users/"
	if kind == orgOwner {
		root = "/orgs/"
	}
	var names []string
	var refused refusals
	for cursor := 0; cursor < len(html); {
		i := strings.Index(html[cursor:], root)
		if i == -1 {
			break
		}
		at := cursor + i
		if at+len(prefix) > len(html) || !strings.EqualFold(html[at:at+len(prefix)], prefix) {
			// The root self-overlaps only at its trailing slash, where the prefix
			// would begin mid-path rather than at a link, so skipping it whole
			// cannot pass over a candidate addressing this owner.
			cursor = at + len(root)
			continue
		}

		quote, ok := hrefAttribute(html, at)
		if !ok {
			refused = refused.refuse(html[at : at+len(prefix)])
			cursor = at + len(root)
			continue
		}

		nameStart := at + len(prefix)
		nameEnd := strings.IndexByte(html[nameStart:], quote)
		if nameEnd == -1 {
			refused = refused.refuse(html[nameStart:])
			break
		}
		nameEnd += nameStart
		raw := html[nameStart:nameEnd]
		cursor = nameEnd

		name, err := urlsafe.PackageName(owner, raw)
		if err != nil {
			refused = refused.refuse(raw)
			continue
		}
		names = append(names, strings.Clone(name))
	}
	return names, refused
}

// scrapeDownloads fetches a single package page and returns its total
// download count. Non-2xx responses and transport errors bubble up as
// httpx.GetBytes returned them; parse failures return errHTMLFormatChanged.
// The /users/ form serves both account kinds: GitHub redirects an organization
// to the repo-scoped package page through the allowlisted redirect policy, so
// expandWildcard's account kind is deliberately not threaded here.
func (c *Client) scrapeDownloads(ctx context.Context, p *pacer, owner, pkg string) (int64, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", owner, url.PathEscape(pkg))
	html, err := c.fetchHTML(ctx, p, pageURL)
	if err != nil {
		return 0, err
	}
	return parseDownloads(html)
}

// parseDownloads extracts the download count from a single package page:
// the title attribute of the <h3> element immediately following the
// "Total downloads" marker element, whitespace skipped. Anything else
// there fails closed with errHTMLFormatChanged rather than publishing a
// number that may belong to another element. Line boundaries are
// deliberately not meaningful: GitHub reflows whitespace without breaking
// this.
func parseDownloads(html string) (int64, error) {
	markerIdx := markerText(html)
	if markerIdx == -1 {
		return 0, errHTMLFormatChanged
	}
	rest := html[markerIdx:]

	markerEnd := strings.Index(rest, ">")
	if markerEnd == -1 {
		return 0, errHTMLFormatChanged
	}
	afterMarker := rest[markerEnd+1:]
	countIdx := markerEnd + 1 + len(afterMarker) - len(strings.TrimLeft(afterMarker, htmlWhitespace))
	if !strings.HasPrefix(rest[countIdx:], "<h3") {
		return 0, errHTMLFormatChanged
	}
	afterName := rest[countIdx+len("<h3"):]
	if afterName == "" || !strings.ContainsRune(htmlWhitespace+">/", rune(afterName[0])) {
		return 0, errHTMLFormatChanged
	}

	tag := rest[countIdx:]
	tagEnd := strings.Index(tag, ">")
	if tagEnd == -1 {
		return 0, errHTMLFormatChanged
	}
	tag = tag[:tagEnd]
	// The title must be an attribute rather than matching bytes in another value.
	raw, ok := titleAttribute(tag)
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

// markerText returns the first "Total downloads" occurrence whose next
// non-whitespace byte begins the element's closing tag.
func markerText(html string) int {
	const marker = "Total downloads"
	for at := 0; ; {
		i := strings.Index(html[at:], marker)
		if i < 0 {
			return -1
		}
		i += at
		rest := strings.TrimLeft(html[i+len(marker):], htmlWhitespace)
		if strings.HasPrefix(rest, "<") {
			return i
		}
		at = i + len(marker)
	}
}

// titleAttribute returns the h3 start tag's single title attribute value.
// Every attribute in the tag must be name="value" or name='value': a bare
// (valueless) attribute, an unquoted value, or a second title all refuse the
// tag, and so does a > inside a quoted value, which truncates tag before this
// runs. All four are legal or near-legal HTML that GitHub does not currently
// serve, and refusing them is deliberate — a count read out of a tag this
// function cannot fully account for is worse than no count, and the caller
// reports the refusal as format drift.
func titleAttribute(tag string) (string, bool) {
	var title string
	found := false
	for cursor := len("<h3"); ; {
		name, value, next, ok := readAttribute(tag, cursor)
		if !ok {
			return "", false
		}
		if name == "" {
			return title, found
		}
		if name == "title" {
			if found {
				return "", false
			}
			title = value
			found = true
		}
		cursor = next
	}
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
			c.opts.Logger.Debug("ghcr package listing cancelled", "owner", ref.Owner, "error", err)
			return packages, false
		}
		wholly := len(names) == 0
		if wholly {
			if errors.Is(err, errHTMLFormatChanged) && !errors.Is(err, errEmptyListing) {
				c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256),
					"report_at", "https://github.com/cplieger/registry-stats/issues")
			} else {
				c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			}
		}
		return c.appendPackages(names, ref.Owner, seen, packages), wholly
	}
	packages = c.appendPackages(names, ref.Owner, seen, packages)
	c.opts.Logger.Info("ghcr wildcard expanded", "owner", ref.Owner, "packages", len(names))
	return packages, false
}

// appendPackages adds every not-yet-seen (owner, name) pair to packages.
func (*Client) appendPackages(
	names []string,
	owner string,
	seen map[registry.RepoRef]bool,
	packages []registry.RepoRef,
) []registry.RepoRef {
	for _, name := range names {
		ref := registry.RepoRef{Owner: owner, Repo: name}
		// Keep the registry spelling in the request and label, but fold the
		// key to match config's canonical explicit refs.
		key := registry.RepoRef{Owner: owner, Repo: strings.ToLower(name)}
		if seen[key] {
			continue
		}
		seen[key] = true
		packages = append(packages, ref)
	}
	return packages
}

// buildPackageList expands wildcard refs (owner/*) by scraping the owner's
// packages listing, then appends explicit refs unless already covered by a
// wildcard. Wildcard keys are folded to the config-canonical explicit spelling,
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
		seen[ref] = true
		packages = append(packages, ref)
	}
	return packages, listingWhollyFailed
}
