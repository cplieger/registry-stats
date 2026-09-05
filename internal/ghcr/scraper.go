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
	"iter"
	"log/slog"
	"net/http"
	"net/url"
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

// advertisedPagesAttr is the only value that can prove the walk read every
// page GitHub says exists. It is absent for a single-page listing.
const advertisedPagesAttr = "data-total-pages"

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
// after the first of the cycle. httpx retries 408, 429, 5xx and transient
// transport errors per c.opts.RetryOpts; other non-2xx statuses fail fast. The
// appended browser headers (anonymous GHCR pages gate on User-Agent) and
// ghcrBodyCap always win, because options are applied left to right.
func (c *Client) fetchHTML(ctx context.Context, p *pacer, pageURL string) (string, error) {
	if err := p.wait(ctx); err != nil {
		return "", err
	}

	// Fresh slice so c.opts.RetryOpts, reused across every request, is
	// never mutated by the append.
	htmlOpts := make([]httpx.GetOption, 0, len(c.opts.RetryOpts)+4)
	htmlOpts = append(htmlOpts, c.opts.RetryOpts...)
	htmlOpts = append(htmlOpts,
		httpx.WithLogger(c.opts.Logger),
		httpx.WithExhaustedLevel(slog.LevelDebug),
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
	names, refused, err := c.readListing(ctx, p, owner, userOwner)
	if len(names) > 0 {
		return names, refused, err
	}

	// A user-form read that produced no names settles nothing about the
	// account kind, and one that failed settles less, so the org form is
	// asked either way. Two org-form answers make the user form's zero links
	// the final word: a 404, which proves the owner is a user account, and a
	// clean read with no links, which is an organization publishing nothing.
	// emptyListingError names both causes because neither is distinguishable
	// from changed listing markup.
	orgNames, orgRefused, orgErr := c.readListing(ctx, p, owner, orgOwner)
	refused = refused.merge(orgRefused)
	switch {
	case len(orgNames) > 0:
		return orgNames, refused, orgErr
	case err != nil:
		return nil, refused, err
	case orgErr == nil || isNotFound(orgErr):
		return nil, refused, emptyListingError(owner)
	default:
		return nil, refused, orgErr
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
// to the cap. An advertised page count stops the walk after every page GitHub
// says exists; without one, a later page with no package link ends the listing.
// Refused candidates make a page partial only if it adds no usable name; mixed
// pages continue and return the refusal summary for caller reporting. Other
// early stops return names already collected. A non-cancelled partial warns
// after collecting names; reaching the cap always warns.
func (c *Client) readListing(ctx context.Context, p *pacer, owner string, kind ownerKind) ([]string, refusals, error) {
	var (
		names      []string
		refused    refusals
		advertised int
	)
	seen := make(map[string]bool)
	for page := 1; page <= maxListingPages; page++ {
		html, err := c.fetchHTML(ctx, p, listingURL(kind, owner, page))
		if err != nil {
			return c.partialListing(ctx, owner, page, names, refused, err)
		}
		pageNames, pageRefused, pageTotal := parsePackageList(html, owner, kind)
		candidates := len(pageNames) + pageRefused.Count
		refused = refused.merge(pageRefused)
		if candidates > maxListingCandidates {
			return c.partialListing(ctx, owner, page, names, refused,
				fmt.Errorf("%w: listing page %d carried %d package candidates", errHTMLFormatChanged, page, candidates))
		}
		kept, added := appendUnseenNames(names, seen, pageNames)
		names = kept
		if advertised == 0 {
			advertised = pageTotal
		} else if pageTotal != advertised {
			return c.partialListing(ctx, owner, page, names, refused,
				fmt.Errorf("%w: listing page %d advertises %d pages against %d earlier", errHTMLFormatChanged, page, pageTotal, advertised))
		}
		if added == 0 {
			if pageRefused.Count > 0 {
				return c.partialListing(ctx, owner, page, names, refused,
					fmt.Errorf("%w: listing page %d yielded no usable package names", errHTMLFormatChanged, page))
			}
			if len(pageNames) > 0 {
				return c.partialListing(ctx, owner, page, names, refused,
					fmt.Errorf("%w: listing page %d re-served %d names already collected", errHTMLFormatChanged, page, len(pageNames)))
			}
			if page < advertised {
				return c.partialListing(ctx, owner, page, names, refused,
					fmt.Errorf("%w: listing page %d of the %d advertised served no package names", errHTMLFormatChanged, page, advertised))
			}
			return names, refused, nil
		}
		if page == advertised {
			return names, refused, nil
		}
	}
	c.opts.Logger.Warn("ghcr owner listing hit page cap; results truncated",
		"owner", owner, "max_pages", maxListingPages, "advertised_pages", advertised)
	return names, refused, nil
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

// markupTagEnd treats ANY quote as opening an attribute value, not only one
// that follows '='. Deliberate: a stray quote that never re-closes costs only
// its own tag; one that finds a partner makes the walk skip every start tag
// through the next '>' after that partner. GitHub serves no unquoted attribute
// value containing a quote.
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

// nextAttributeName returns the next attribute name at or after start and the
// index of the '=' that follows it, skipping any valueless attribute in
// between: HTML allows one anywhere, GitHub serves several, and the pairs after
// it are still readable. An empty name with ok true is the end of the tag.
// Whitespace between the name and its '=' is not this case and refuses the tag:
// such a name is skipped as valueless, and the bare '=' that follows it names
// nothing.
func nextAttributeName(tag string, start int) (name string, eq int, ok bool) {
	for {
		attrs := strings.TrimLeft(tag[start:], htmlWhitespace)
		if attrs == "" || attrs == "/" {
			return "", len(tag), true
		}
		nameEnd := strings.IndexAny(attrs, "="+htmlWhitespace)
		if nameEnd < 0 {
			return "", len(tag), true
		}
		if nameEnd == 0 {
			return "", 0, false
		}
		at := len(tag) - len(attrs) + nameEnd
		if attrs[nameEnd] != '=' {
			start = at
			continue
		}
		return attrs[:nameEnd], at, true
	}
}

func readAttribute(tag string, start int) (name, value string, next int, ok bool) {
	name, eq, ok := nextAttributeName(tag, start)
	if !ok || name == "" {
		return "", "", eq, ok
	}
	attrs := tag[eq+1:]
	if attrs == "" || (attrs[0] != '\'' && attrs[0] != '"') {
		return "", "", 0, false
	}
	// An attribute name must start at a boundary: the byte before it is the
	// '<' that opens the tag, or whitespace. A name that begins after any
	// other byte, a stray '<' inside the tag included, refuses the tag.
	if before := eq - len(name) - 1; before < 0 ||
		(tag[before] != '<' && !strings.ContainsRune(htmlWhitespace, rune(tag[before]))) {
		return "", "", 0, false
	}
	quote := attrs[0]
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

// startsTagName reports whether b is the ASCII letter that opens a start tag.
func startsTagName(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// nextStartTag reads the markup at or after cursor. emit is true only for a
// start tag; next is where scanning resumes; ok is false at end of input.
//
// GitHub serves an XSS canary comment whose body is a single quote, a double
// quote, and a backtick. Skipping a terminated comment at its "-->" delimiter
// keeps those bytes from becoming attribute quotes. An unterminated comment or
// malformed start tag advances one byte so later tags remain readable.
func nextStartTag(html string, cursor int) (tag string, next int, emit, ok bool) {
	i := strings.IndexByte(html[cursor:], '<')
	if i < 0 {
		return "", 0, false, false
	}
	open := cursor + i
	// A comment ends at its own delimiter, and quotes inside it mean nothing.
	if rest, found := strings.CutPrefix(html[open:], "<!--"); found {
		if end := strings.Index(rest, "-->"); end >= 0 {
			return "", open + len("<!--") + end + len("-->"), false, true
		}
	}
	// Only a start tag's attribute values can carry a '>', so only a start tag's
	// end may be found by tracking quotes. Anything else — an end tag, a
	// processing instruction, an unterminated comment, a '<' in page text —
	// advances one byte.
	if open+1 >= len(html) || !startsTagName(html[open+1]) {
		return "", open + 1, false, true
	}
	tagEnd, found := markupTagEnd(html, open)
	if !found {
		return "", open + 1, false, true
	}
	return html[open:tagEnd], tagEnd + 1, true, true
}

// startTags yields each span that begins at a '<' followed by an ASCII letter and
// ends at the first '>' outside a quoted attribute value. End tags, processing
// instructions, and terminated comments are skipped. A raw-text element body
// (script, style, textarea, title) is walked as ordinary markup, including tag-shaped bytes.
func startTags(html string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for cursor := 0; cursor < len(html); {
			tag, next, emit, ok := nextStartTag(html, cursor)
			if !ok {
				return
			}
			cursor = next
			if emit && !yield(tag) {
				return
			}
		}
	}
}

// tagAttributes yields one start tag's name="value" pairs. The walk stops at
// the first pair readAttribute cannot account for — an unquoted value or a
// malformed pair — because a tag this cannot fully read is a tag whose
// remaining attributes are not knowable.
func tagAttributes(tag string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		at := strings.IndexAny(tag, htmlWhitespace+"/")
		if at < 0 {
			return
		}
		for {
			name, value, next, ok := readAttribute(tag, at)
			if !ok || name == "" {
				return
			}
			if !yield(name, value) {
				return
			}
			at = next
		}
	}
}

// parsePackageList extracts package names and the advertised page total from
// one owner's listing page; zero means the attribute was absent or unusable.
// A name comes only from a real href attribute on a start tag, so page text,
// terminated comments, and unrelated attributes are not candidates. An
// unterminated comment body is still walked. Registered owner casing is
// response data, so the prefix is matched without transforming untrusted HTML.
// Zero names is not an error here because its meaning depends on the page
// number, which only the caller knows.
func parsePackageList(html, owner string, kind ownerKind) ([]string, refusals, int) {
	prefix := linkPrefix(kind, owner)
	var names []string
	var refused refusals
	advertised := 0
	for tag := range startTags(html) {
		for name, value := range tagAttributes(tag) {
			if strings.EqualFold(name, advertisedPagesAttr) {
				if pages, err := strconv.Atoi(strings.Trim(value, htmlWhitespace)); err == nil && pages > 0 {
					advertised = pages
				}
				continue
			}
			if !strings.EqualFold(name, "href") || len(value) < len(prefix) ||
				!strings.EqualFold(value[:len(prefix)], prefix) {
				continue
			}
			raw := value[len(prefix):]
			pkg, err := urlsafe.PackageName(owner, raw)
			if err != nil {
				refused = refused.refuse(raw)
				continue
			}
			names = append(names, strings.Clone(pkg))
		}
	}
	return names, refused, advertised
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
// unique "Total downloads" marker element, whitespace skipped. Anything
// else there, including an ambiguous marker, fails closed with
// errHTMLFormatChanged rather than publishing a number that may belong to
// another element. Line boundaries are deliberately not meaningful: GitHub
// reflows whitespace without breaking this.
func parseDownloads(html string) (int64, error) {
	markerIdx, markers := markerText(html)
	if markers != 1 {
		return 0, fmt.Errorf("%w: %d download-count markers", errHTMLFormatChanged, markers)
	}
	rest := html[markerIdx:]

	markerEnd := strings.Index(rest, ">")
	if markerEnd == -1 {
		return 0, errHTMLFormatChanged
	}
	afterMarker := rest[markerEnd+1:]
	countIdx := markerEnd + 1 + len(afterMarker) - len(strings.TrimLeft(afterMarker, htmlWhitespace))
	const countTag = "<h3"
	if len(rest)-countIdx < len(countTag) || !strings.EqualFold(rest[countIdx:countIdx+len(countTag)], countTag) {
		return 0, errHTMLFormatChanged
	}
	afterName := rest[countIdx+len(countTag):]
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

// markerText returns the first plausible "Total downloads" occurrence and
// the number found. A plausible occurrence is bounded like element text: the
// previous non-whitespace byte closes a start tag and the next bytes begin an
// element's closing tag. It does not track markup context, so a comment or
// script body with that shape counts too. If the real marker is also present,
// parseDownloads rejects the duplicate; if the false shape is the only one,
// its count is read, a shape GitHub does not serve.
func markerText(html string) (idx, n int) {
	const marker = "Total downloads"
	for at := 0; ; {
		i := strings.Index(html[at:], marker)
		if i < 0 {
			return idx, n
		}
		i += at
		before := strings.TrimRight(html[:i], htmlWhitespace)
		rest := strings.TrimLeft(html[i+len(marker):], htmlWhitespace)
		if strings.HasSuffix(before, ">") && strings.HasPrefix(rest, "</") {
			if n == 0 {
				idx = i
			}
			n++
		}
		at = i + len(marker)
	}
}

// titleAttribute returns the h3 start tag's single title attribute value.
// Every valued attribute in the tag must be name="value" or name='value': an
// unquoted value or a second title refuses the tag, and so does a > inside a
// quoted value, which truncates tag before this runs. All three are legal or
// near-legal HTML that GitHub does not currently serve, and refusing them is
// deliberate — a count read out of a tag this function cannot fully account
// for is worse than no count, and the caller reports the refusal as format
// drift.
func titleAttribute(tag string) (string, bool) {
	var (
		title string
		found bool
	)
	for cursor := len("<h3"); ; {
		name, value, next, ok := readAttribute(tag, cursor)
		if !ok {
			return "", false
		}
		if name == "" {
			return title, found
		}
		if strings.EqualFold(name, "title") {
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
				"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			return out, false
		}
		listingWhollyFailed = len(names) == 0
		if listingWhollyFailed {
			switch {
			case errors.Is(err, errHTMLFormatChanged) && !errors.Is(err, errEmptyListing):
				c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256),
					"report_at", "https://github.com/cplieger/registry-stats/issues")
			case errors.Is(err, errEmptyListing):
				c.opts.Logger.Error("ghcr package listing failed", "owner", ref.Owner,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			default:
				c.opts.Logger.Warn("ghcr package listing failed", "owner", ref.Owner,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			}
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
