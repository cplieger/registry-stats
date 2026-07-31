// Package ghcr is the GitHub Container Registry source for registry-stats.
// It collects per-package download counts by scraping the public
// github.com package pages (GHCR has no unauthenticated API for this
// data), handling wildcard owner expansion via the owner's packages
// listing page.
//
// Contract boundaries kept intact through extraction:
//   - URL shapes (https://github.com/users/{owner}/packages,
//     https://github.com/users/{owner}/packages/container/package/{name})
//     are unchanged; httpx.DockerGitHubRedirectPolicy (wired on the shared
//     *http.Client in main.go) still enforces the SSRF allowlist for
//     github.com hops.
//   - The HTML parsing heuristics (Total downloads marker, 500-byte
//     title= search window, /users/{owner}/packages/container/package/
//     prefix matching) are preserved verbatim. Any change here is a
//     behavior-change deferral, not a refactor.
//   - ErrHTMLFormatChanged is the single sentinel returned for any
//     parse failure; callers in main.go compare via errors.Is to decide
//     whether to emit the "format may be changing" WARN/ERROR logs.
//
// The package exposes a *Client (for composition-root wiring via
// api.RegistrySource). The HTML-handling internals (fetchHTML,
// scrapePackageList, scrapeDownloads, buildPackageList) are
// unexported — all collection flows go through *Client.Collect.
// ParseDownloads and ParsePackageList stay exported as pure-core
// parse helpers so in-package and external parse-only tests can
// drive them without constructing a Client; ErrHTMLFormatChanged
// stays exported as the sentinel callers compare via errors.Is.
package ghcr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/keyenc"
	"github.com/cplieger/registry-stats/v2/internal/model"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
)

// ErrHTMLFormatChanged is the sentinel returned for any GHCR HTML parse
// failure — missing "Total downloads" marker, missing title attribute,
// non-numeric or negative count, empty package list. Callers compare via
// errors.Is to distinguish parse drift from transport errors.
var ErrHTMLFormatChanged = errors.New("GHCR HTML format changed")

// maxTitleDistance caps the scan window for the title="N" attribute
// after the "Total downloads" marker. A match further out likely
// belongs to a different element and signals format drift.
const maxTitleDistance = 500

// ghcrBodyCap is the LimitReader cap for GitHub HTML responses. 2 MB
// comfortably fits every real package page; larger responses are a
// format signal, not legitimate content.
const ghcrBodyCap = 2 << 20

// fetchHTML fetches a GitHub HTML page with browser-like headers,
// retrying on 429 and 5xx per opts. Transport errors and non-allowlist
// status codes fail fast. The caller's opts are applied first; fetchHTML
// then appends WithHeaders (the User-Agent/Accept/Accept-Language
// triplet GitHub expects from a browser session — anonymous GHCR pages
// gate on UA) and WithMaxBodyBytes(ghcrBodyCap). Because options are
// applied left-to-right, these appended values always win, preserving
// the pre-library behavior where the browser headers were installed
// unconditionally and the body was capped at ghcrBodyCap.
func fetchHTML(ctx context.Context, client *http.Client, pageURL string, opts []httpx.GetOption) (string, error) {
	// Build a fresh slice so the caller's opts (reused across every
	// scrape via c.retryOpts) is never mutated by the append.
	htmlOpts := make([]httpx.GetOption, 0, len(opts)+2)
	htmlOpts = append(htmlOpts, opts...)
	htmlOpts = append(htmlOpts,
		httpx.WithHeaders(func(req *http.Request) {
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
			req.Header.Set("Accept", "text/html")
			req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		}),
		httpx.WithMaxBodyBytes(ghcrBodyCap),
	)
	body, err := httpx.GetBytes(ctx, client, pageURL, htmlOpts...)
	if err != nil {
		// An over-cap response is a format signal, not a transport error: a
		// GHCR page that suddenly exceeds ghcrBodyCap means the markup bloated
		// or changed. Route it into the ErrHTMLFormatChanged path so the
		// caller's majority-format-drift escalation catches it, while keeping
		// the typed *ResponseTooLargeError unwrappable. (v1 silently truncated
		// the body; httpx v2 returns the typed error.)
		var tooLarge *httpx.ResponseTooLargeError
		if errors.As(err, &tooLarge) {
			return "", fmt.Errorf("%w: %w", ErrHTMLFormatChanged, err)
		}
		return "", err
	}
	return string(body), nil
}

// scrapePackageList fetches an owner's packages listing page and
// returns the discovered package names. Returns ErrHTMLFormatChanged
// (wrapped) when the HTML contains no recognized package links.
func scrapePackageList(ctx context.Context, client *http.Client, owner string, opts []httpx.GetOption) ([]string, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages", owner)
	html, err := fetchHTML(ctx, client, pageURL, opts)
	if err != nil {
		return nil, err
	}
	return ParsePackageList(html, owner)
}

// packageListParser accumulates the deduplicated, URL-safe package names
// scraped from an owner's packages page. It carries the line scan's
// mutable accounting (seen + names) so the line loop in ParsePackageList
// stays flat.
type packageListParser struct {
	prefix string
	owner  string
	seen   map[string]bool
	names  []string
}

// scanLine extracts every package-link name on one line of HTML,
// appending new safe names to p.names. Empty and unsafe names are
// skipped (unsafe ones logged); duplicates are dropped.
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
		if name == "" {
			continue
		}
		if !urlsafe.IsSafeURLSegment(name) {
			slog.Debug("skipping package name with unsafe characters", "name", name, "owner", p.owner)
			continue
		}
		if !p.seen[name] {
			p.seen[name] = true
			p.names = append(p.names, name)
		}
	}
}

// ParsePackageList extracts package names from an owner's packages page
// HTML. Looks for /users/{owner}/packages/container/package/{name}
// links and filters names through urlsafe.IsSafeURLSegment so a crafted
// page cannot smuggle path traversal into downstream URL construction.
// Duplicate names (same page can list a package multiple times) are
// deduplicated in insertion order.
func ParsePackageList(html, owner string) ([]string, error) {
	p := packageListParser{
		prefix: fmt.Sprintf("/users/%s/packages/container/package/", owner),
		owner:  owner,
		seen:   make(map[string]bool),
	}
	for line := range strings.SplitSeq(html, "\n") {
		p.scanLine(line)
	}

	if len(p.names) == 0 {
		return nil, fmt.Errorf("%w: no packages found on %s's packages page", ErrHTMLFormatChanged, owner)
	}

	return p.names, nil
}

// scrapeDownloads fetches a single package page and returns its total
// download count. Non-2xx responses and transport errors bubble up as
// httpx.GetBytes returned them; parse failures return ErrHTMLFormatChanged.
func scrapeDownloads(ctx context.Context, client *http.Client, owner, pkg string, opts []httpx.GetOption) (int64, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", owner, pkg)
	html, err := fetchHTML(ctx, client, pageURL, opts)
	if err != nil {
		return 0, err
	}
	return ParseDownloads(html)
}

// ParseDownloads extracts the download count from a single package
// page. Looks for the "Total downloads" marker, then scans forward at
// most maxTitleDistance bytes for the first title="N" attribute and
// parses N as a non-negative int64. Line boundaries are intentionally
// not meaningful — GitHub's HTML can reflow whitespace without breaking
// this parser.
func ParseDownloads(html string) (int64, error) {
	markerIdx := strings.Index(html, "Total downloads")
	if markerIdx == -1 {
		return 0, ErrHTMLFormatChanged
	}
	rest := html[markerIdx:]
	rest = rest[:min(len(rest), maxTitleDistance)]
	titleIdx := strings.Index(rest, `title="`)
	if titleIdx == -1 {
		return 0, ErrHTMLFormatChanged
	}
	rest = rest[titleIdx+len(`title="`):]
	raw, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return 0, ErrHTMLFormatChanged
	}
	count, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: parse count: %w", ErrHTMLFormatChanged, err)
	}
	if count < 0 {
		return 0, fmt.Errorf("%w: negative download count: %d", ErrHTMLFormatChanged, count)
	}
	return count, nil
}

// packageKey builds the dedup key for one (owner, package) pair. It is the
// single encoder for buildPackageList's `seen` map, which BOTH the wildcard
// expansion (owner + scraped package name) and the explicit-ref pass (owner +
// configured repo) write into — sharing one map is what lets a wildcard-found
// package suppress the same package listed explicitly. Because they share the
// map they must share the encoding: if the two builders ever disagreed on how
// a pair becomes a string, the explicit ref would stop matching its wildcard
// twin and get appended a second time, so the same image would be scraped and
// exported twice. Having one function is what makes that impossible.
//
// The components are escaped with keyenc rather than concatenated so the key's
// injectivity is a property of the key, not an inherited guarantee from a
// validator two packages away. Today every component is separator-free —
// config's ParseRepoRefs gates owner and repo through
// urlsafe.IsSafeURLSegment ([A-Za-z0-9._-], so no ':' and no '\'), the literal
// "*" wildcard marker contains neither, and scraped names are filtered through
// the same predicate in packageListParser.scanLine — so no collision is
// reachable today and this changes no behavior. It stops the key from
// depending on that: were the allowlist relaxed, a third component added, or a
// name sourced from somewhere that does not filter, a name carrying the
// separator could forge another pair's key. The consequence would be a silent
// data hole rather than a wrong number: the forged key already reads as seen,
// so the real package is dropped from the returned list, never scraped, and
// its registrystats_image_pulls_total series simply stops appearing on
// /metrics — a gap in the dashboard with nothing logged.
//
// The bytes changed with the adoption ('/' -> ':'). That is free here: this
// key exists only inside one buildPackageList call, is never persisted,
// logged, exported as a metric label, or compared across runs.
func packageKey(owner, pkg string) string {
	return keyenc.Join(owner, pkg)
}

// expandWildcard scrapes one wildcard owner's package listing and
// appends each new (deduplicated) package ref to packages. It returns
// the grown slice plus listing-failure and format-drift-failure counts
// (0 or 1 each) so the caller can keep its running tallies without
// nesting the failure classification inside its loop.
func expandWildcard(
	ctx context.Context,
	client *http.Client,
	logger *slog.Logger,
	ref model.RepoRef,
	opts []httpx.GetOption,
	seen map[string]bool,
	packages []model.RepoRef,
) (out []model.RepoRef, listingFailures, listingParseFailures int) {
	names, err := scrapePackageList(ctx, client, ref.Owner, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Shutdown/deadline cancelled the listing scrape; expected, not a failure.
			// Do not log ERROR -- avoids a false alert on the level=error GHCR stream on
			// every SIGTERM that lands mid-listing. Counts unchanged (still 1 listing failure).
			logger.Debug("ghcr package listing cancelled", "owner", ref.Owner, "error", err)
			return packages, 1, 0
		}
		logger.Error("ghcr package listing failed", "owner", ref.Owner, "error", err)
		if errors.Is(err, httpx.ErrRateLimited) {
			logger.Warn("ghcr listing rate limited", "owner", ref.Owner,
				"hint", "consider increasing pacing delay or reducing package count")
		}
		if errors.Is(err, ErrHTMLFormatChanged) {
			return packages, 1, 1
		}
		return packages, 1, 0
	}
	for _, name := range names {
		key := packageKey(ref.Owner, name)
		if !seen[key] {
			seen[key] = true
			packages = append(packages, model.RepoRef{Owner: ref.Owner, Repo: name})
		}
	}
	logger.Info("ghcr wildcard expanded", "owner", ref.Owner, "packages", len(names))
	return packages, 0, 0
}

// buildPackageList expands wildcard refs (owner/*) by scraping the
// owner's packages page, then appends explicit refs unless already
// covered by a wildcard. Returns the deduplicated list plus counts of
// listing-level failures and format-drift listing failures so the
// caller can decide whether to log a format-change alert.
//
// Both passes key the `seen` map through packageKey — see that function for
// why the shared map requires a shared encoder.
func buildPackageList(
	ctx context.Context,
	client *http.Client,
	logger *slog.Logger,
	refs []model.RepoRef,
	opts []httpx.GetOption,
) (packages []model.RepoRef, listingFailures, listingParseFailures int) {
	seen := make(map[string]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		var lf, pf int
		packages, lf, pf = expandWildcard(ctx, client, logger, ref, opts, seen, packages)
		listingFailures += lf
		listingParseFailures += pf
	}
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		key := packageKey(ref.Owner, ref.Repo)
		if !seen[key] {
			seen[key] = true
			packages = append(packages, ref)
		}
	}
	return packages, listingFailures, listingParseFailures
}
