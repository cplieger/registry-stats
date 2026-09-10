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

// maxListingCandidates bounds matching link occurrences plus refused
// candidates per page, counted before dedup; the largest measured page
// carried 30 candidates.
const maxListingCandidates = 100

// maxListingPages bounds a listing that keeps serving new package names.
// Fifty is a round limit above the largest measured total of 23 pages.
const maxListingPages = 50

// statedPackagesMarker identifies the printed package count for one listing page.
const statedPackagesMarker = "packages"

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

// fetchHTML fetches a GitHub HTML page, spacing requests by c.pacingDelay
// after the first of the cycle. httpx retries 408, 429, 5xx and transient
// transport errors per c.opts.RetryOpts; other non-2xx statuses fail fast. The
// appended request headers and ghcrBodyCap always win, because options are
// applied left to right.
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
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Accept", "text/html")
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
	orgNames, refused, orgErr := c.readListing(ctx, p, owner, orgOwner)
	if len(orgNames) > 0 {
		return orgNames, refused, orgErr
	}

	// The organization form is the kind probe: names identify an organization
	// and a 404 identifies a user. If it yields no names, the user form is still
	// tried so an organization-form failure cannot hide a valid user listing.
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
// one deleted mid-walk may still be published for it; both self-heal at
// the next poll, and an absent series already means "not measured this
// cycle" per obs.SetImage's retirement contract.
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
		pageNames, pageRefused, err := parsePackageList(html, owner, kind)
		if err != nil {
			return c.partialListing(ctx, owner, page, names, refused, err)
		}
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

// markupTagEnd treats a quote as opening an attribute value only after '=',
// matching HTML attribute syntax, so a stray quote cannot extend the tag. An
// '='-opened value GitHub never closes still swallows following markup; the
// page's printed package count refuses the resulting short name set.
func markupTagEnd(html string, start int) (int, bool) {
	var quote byte
	afterEq := false
	for i := start + 1; i < len(html); i++ {
		c := html[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '=':
			afterEq = true
		case afterEq && (c == '\'' || c == '"'):
			quote = c
		case c == '>':
			return i, true
		}
		if quote == 0 && c != '=' && !strings.ContainsRune(htmlWhitespace, rune(c)) {
			afterEq = false
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
	// An attribute name must start after whitespace or a '<'. The check bounds the
	// name to bytes that could begin one; nextAttributeName has already skipped
	// valueless attributes, so a name reached through one is still read.
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

// equalASCIIFold compares ASCII case without Unicode simple folding, which
// would let non-ASCII near-spellings match parser literals.
func equalASCIIFold(s, t string) bool {
	if len(s) != len(t) {
		return false
	}
	for i := range len(s) {
		if lowerASCII(s[i]) != lowerASCII(t[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

// startsTagName reports whether b is the ASCII letter that opens a start tag.
func startsTagName(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

func commentEnd(html string, open int) (int, bool) {
	rest := html[open+len("<!--"):]
	// An abruptly closed comment (<!--> or <!--->) ends at its '>'.
	if after, ok := strings.CutPrefix(rest, ">"); ok {
		return len(html) - len(after), true
	}
	if after, ok := strings.CutPrefix(rest, "->"); ok {
		return len(html) - len(after), true
	}

	// A comment ends at "-->", or earlier at the bang close "--!>".
	end := strings.Index(rest, "-->")
	scope := rest
	if end >= 0 {
		scope = rest[:end]
	}
	if bang := strings.Index(scope, "--!>"); bang >= 0 {
		return open + len("<!--") + bang + len("--!>"), true
	}
	if end >= 0 {
		return open + len("<!--") + end + len("-->"), true
	}
	return 0, false
}

// nextStartTag reads the markup at or after cursor. emit is true only for a
// start tag; next is where scanning resumes; ok is false at end of input.
//
// A terminated comment is skipped at its comment end ("-->", "--!>", or an
// abrupt "<!-->"/"<!--->"). A doctype, bogus declaration, CDATA section,
// processing instruction, or nameless end tag is skipped at its own '>'; a
// named end tag is read like a start tag, because HTML gives it the same
// attribute syntax. Each ends at its first '>' even when a quote precedes it,
// which is where HTML's own doctype and bogus-comment states end them, so bytes
// after that '>' are markup again and a link there is emitted. A CDATA section
// in foreign content is the one case that differs: HTML ends that at ']]>', so
// a link after a '>' in its body is text upstream and a package name here. A
// construct whose terminator is absent runs to end of input, so the walk ends
// there rather than resuming inside it; next then carries the construct's own
// offset.
func nextStartTag(html string, cursor int) (tag string, next int, emit, unterminated, ok bool) {
	i := strings.IndexByte(html[cursor:], '<')
	if i < 0 {
		return "", 0, false, false, false
	}
	open := cursor + i
	// A '<' that is the input's final byte opens no markup.
	if open+1 >= len(html) {
		return "", open + 1, false, false, true
	}
	if strings.HasPrefix(html[open:], "<!--") {
		if end, found := commentEnd(html, open); found {
			return "", end, false, false, true
		}
		return "", open, false, true, true
	}
	// An end tag carries the same attribute syntax as a start tag, so a '>'
	// inside one of its quoted values does not end it; it is read like a
	// start tag and yielded to nobody.
	if open+2 < len(html) && html[open+1] == '/' && startsTagName(html[open+2]) {
		if end, found := markupTagEnd(html, open); found {
			return "", end + 1, false, false, true
		}
		return "", open, false, true, true
	}
	// A '<' that cannot open a start tag opens no markup either: a doctype, a bogus
	// declaration, a CDATA section in HTML content, a processing instruction and a
	// nameless end tag all end at their own '>', so their bodies are not markup. An
	// unterminated one ends the walk, as an unterminated comment does.
	if html[open+1] == '!' || html[open+1] == '?' || html[open+1] == '/' {
		if end := strings.IndexByte(html[open:], '>'); end >= 0 {
			return "", open + end + 1, false, false, true
		}
		return "", open, false, true, true
	}
	// Any other '<' not followed by an ASCII letter advances one byte.
	if !startsTagName(html[open+1]) {
		return "", open + 1, false, false, true
	}
	tagEnd, found := markupTagEnd(html, open)
	if !found {
		return "", open, false, true, true
	}
	return html[open:tagEnd], tagEnd + 1, true, false, true
}

// startTags yields each span that begins at a '<' followed by an ASCII letter and
// ends at the first '>' outside a quoted span. End tags, processing
// instructions, and terminated comments are skipped. A raw-text element body
// (script, style, textarea, title) is walked as ordinary markup, including tag-shaped bytes.
// A terminator lookup that runs to end of input ends the walk with
// errHTMLFormatChanged instead of a span.
func startTags(html string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for cursor := 0; cursor < len(html); {
			tag, next, emit, unterminated, ok := nextStartTag(html, cursor)
			if !ok {
				return
			}
			if unterminated {
				yield("", fmt.Errorf("%w: unterminated markup construct at byte %d", errHTMLFormatChanged, next))
				return
			}
			cursor = next
			if emit && !yield(tag, nil) {
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

// parsePackageList extracts package names from one owner's listing page. A
// name comes from an href on any span the lexical walk emits, page text
// included; the page's printed package count is compared against the set
// read, and a mismatch refuses the page.
// Registered owner casing is response data, so the prefix is matched without
// transforming untrusted HTML.
// Zero names is not an error here because its meaning depends on the page
// number, which only the caller knows.
func parsePackageList(html, owner string, kind ownerKind) (names []string, refused refusals, err error) {
	prefix := linkPrefix(kind, owner)
	for tag, walkErr := range startTags(html) {
		if walkErr != nil {
			return names, refused, walkErr
		}
		tagNames, tagRefused := parsePackageTag(tag, owner, prefix)
		names = append(names, tagNames...)
		refused = refused.merge(tagRefused)
	}
	return names, refused, nil
}

func parsePackageTag(tag, owner, prefix string) (names []string, refused refusals) {
	for name, value := range tagAttributes(tag) {
		raw, isPackage := packageHref(name, value, prefix)
		if !isPackage {
			continue
		}
		pkg, nameErr := urlsafe.PackageName(urlsafe.Owner(owner), raw)
		if nameErr != nil {
			refused = refused.refuse(raw)
			continue
		}
		names = append(names, strings.Clone(pkg))
	}
	return names, refused
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
	after := strings.TrimLeft(html[markerAt+len(statedPackagesMarker):], htmlWhitespace)
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

// packageHref reports the package segment carried under prefix.
func packageHref(name, value, prefix string) (segment string, isPackage bool) {
	if !equalASCIIFold(name, "href") || len(value) < len(prefix) ||
		!equalASCIIFold(value[:len(prefix)], prefix) {
		return "", false
	}
	return value[len(prefix):], true
}

// scrapeDownloads fetches a single package page and returns its total
// download count. Non-2xx responses and transport errors bubble up as
// httpx.GetBytes returned them; parse failures return errHTMLFormatChanged.
// The /users/ form serves both account kinds: GitHub redirects an organization
// to the repo-scoped package page through the allowlisted redirect policy, so
// expandWildcard's account kind is deliberately not threaded here.
func (c *Client) scrapeDownloads(ctx context.Context, p *pacer, ref registry.RepoRef) (int64, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", ref.Owner, url.PathEscape(ref.Repo))
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

	// The end tag closing the marker element may carry an attribute whose value
	// contains '>', so its end is found quote-aware: stopping at the first '>'
	// lands inside that value, and bytes written there are then read as the
	// count element below.
	endTagAt := strings.Index(rest, "<")
	if endTagAt < 0 {
		return 0, errHTMLFormatChanged
	}
	markerEnd, ok := markupTagEnd(rest, endTagAt)
	if !ok {
		return 0, errHTMLFormatChanged
	}
	afterMarker := rest[markerEnd+1:]
	countIdx := markerEnd + 1 + len(afterMarker) - len(strings.TrimLeft(afterMarker, htmlWhitespace))
	const countTag = "<h3"
	if len(rest)-countIdx < len(countTag) || !equalASCIIFold(rest[countIdx:countIdx+len(countTag)], countTag) {
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

func nextCommentSpan(html string, cursor int) (start, end int, terminated bool) {
	offset := strings.Index(html[cursor:], "<!--")
	if offset < 0 {
		return len(html), len(html), true
	}
	start = cursor + offset
	end, terminated = commentEnd(html, start)
	if !terminated {
		end = len(html)
	}
	return start, end, terminated
}

// markerText returns the first plausible "Total downloads" occurrence and
// the number found. A plausible occurrence is bounded like element text: the
// previous non-whitespace byte closes a start tag and the next bytes begin an
// element's closing tag. It skips comments but does not track raw-text element
// context, so a script body with that shape counts too. If the real marker is
// also present, parseDownloads rejects the duplicate; if the false shape is the
// only one, its count is read, a shape GitHub does not serve.
func markerText(html string) (idx, n int) {
	const marker = "Total downloads"
	// The marker search only moves forward, so each comment is scanned
	// once: a new "<!--" can only be in the window since the previous
	// occurrence, and a marker inside a comment resumes at that comment's
	// end.
	commentStart, commentAt, terminated := 0, 0, true
	for at := 0; ; {
		i := strings.Index(html[at:], marker)
		if i < 0 {
			return idx, n
		}
		i += at
		if commentAt <= i {
			commentStart, commentAt, terminated = nextCommentSpan(html, commentAt)
		}
		if commentStart <= i && i < commentAt {
			if !terminated {
				return idx, n
			}
			at = commentAt
			continue
		}
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
// An unquoted value, a second title, and a '>' inside a quoted value each
// refuse the tag. Stray markup before the title is not refused:
// nextAttributeName reads it as a valueless attribute, so the count is still
// read. GitHub serves neither shape; what bounds a wrong number is
// parseDownloads' unique-marker refusal and the positional h3, not this walk.
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
		if equalASCIIFold(name, "title") {
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
		seen[ref] = true
		packages = append(packages, ref)
	}
	return packages, listingWhollyFailed
}
