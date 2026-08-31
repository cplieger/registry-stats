// Package dockerhub collects pull counts and total tag counts for public
// Docker Hub repositories through the unauthenticated /v2/ API, expanding
// "owner/*" refs against the owner listing.
//
// Outbound requests use the caller-supplied *http.Client, on which main
// wires httpx.DockerGitHubRedirectPolicy as the SSRF allowlist.
package dockerhub

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
)

// Pagination bounds for the owner listing. maxOwnerPages bounds how many
// pages one owner costs, and hitting it is a signal that behaviour changed
// (the warn log below surfaces it). pageSize is both what the request asks
// for and the element count parseRepoListPage refuses to exceed; a page's
// SIZE is bounded by httpx's 10 MiB body cap and each listed name by
// urlsafe.IsSafeURLSegment.
const (
	maxOwnerPages = 10
	pageSize      = 100
)

// Client is the Docker Hub source (it satisfies collect.Source at the wiring
// site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.GetOption
	// pageCap overrides maxOwnerPages when non-zero; 0 = the default.
	// Written only by the in-package test that exercises the pagination bound.
	pageCap int
}

// Options configures NewClient beyond the required HTTP client: the
// per-request retry options and the logger, each with a meaningful zero
// (httpx defaults; slog.Default).
type Options struct {
	// Logger receives the client's warnings; nil falls back to slog.Default.
	Logger *slog.Logger
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx
	// defaults.
	RetryOpts []httpx.GetOption
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http:      client,
		retryOpts: opts.RetryOpts,
		logger:    logger,
	}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare and to derive the "dockerhub" log label
// (registry.DockerHub.String()).
func (c *Client) Source() registry.ID { return registry.DockerHub }

// Collect gathers pull counts and total tag counts for every ref in
// refs. Returns the per-repo entries plus the attempted count (including
// failures) and a healthy flag. healthy is false when the collection is
// severely degraded (see degraded) OR when a wildcard owner-listing
// wholly failed — a non-nil listing error that yielded zero usable
// repos. The listing-failure signal is distinct because a wholesale
// listing outage leaves attempted == 0, which degraded alone reads as
// healthy and would therefore mask a total Docker Hub outage.
//
// A repo whose tag-count fetch fails still contributes its entry (pulls
// intact) with TagCount 0, so the caller emits no image_tags series for
// it that cycle rather than a wrong value — the same skip-don't-zero
// rule the GHCR source applies to failed scrapes.
func (c *Client) Collect(
	ctx context.Context,
	refs []registry.RepoRef,
) (entries []registry.Entry, attempted int, healthy bool) {
	wildcardResults, wildcardAttempted, seen, listingFailed := collectWildcards(ctx, c, refs)
	explicitResults, explicitAttempted := collectExplicit(ctx, c, refs, seen)
	entries = append(wildcardResults, explicitResults...)
	attempted = wildcardAttempted + explicitAttempted
	return entries, attempted, !degraded(entries, attempted) && !listingFailed
}

// collectWildcards expands every "*" ref into concrete repo entries by
// listing the owner's public repos, then fetches each repo's tag count.
// The returned seen map tracks which repos were collected so
// collectExplicit can skip duplicates; its key is the repo pair itself,
// so no encoding is shared and the two passes cannot disagree about one.
//
// listingFailed is true when at least one owner listing wholly failed
// (see collectWildcardRef), so a total Docker Hub listing outage surfaces
// as unhealthy instead of an empty-but-healthy result.
func collectWildcards(ctx context.Context, c *Client, refs []registry.RepoRef) (results []registry.Entry, attempted int, seen map[registry.RepoRef]bool, listingFailed bool) {
	seen = make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		if ctx.Err() != nil {
			return results, attempted, seen, listingFailed
		}
		refResults, refAttempted, refFailed := collectWildcardRef(ctx, c, ref.Owner, seen)
		results = append(results, refResults...)
		attempted += refAttempted
		if refFailed {
			listingFailed = true
		}
	}
	return results, attempted, seen, listingFailed
}

// collectWildcardRef lists one owner's public repos and collects each
// repo's tag count, deduping against the shared seen map (mutated in
// place) whose key is the repo pair itself, so the wildcard and explicit
// passes cannot disagree about one repo. listingFailed reports a wholesale
// listing outage for this owner — a listRepos error that yielded zero
// repos, where attempted stays 0 and degraded would read the empty result
// as healthy. A partial failure, a legitimately empty owner and a
// cancelled cycle all leave it false; only the first two leave the verdict
// to the degradation path, because a stop is not an outage.
func collectWildcardRef(ctx context.Context, c *Client, owner string, seen map[registry.RepoRef]bool) (results []registry.Entry, attempted int, listingFailed bool) {
	repos, err := listRepos(ctx, c, owner)
	switch {
	case ctx.Err() != nil:
		// A stop is not a classification: no record, and listingFailed
		// stays false.
	case err != nil && len(repos) == 0:
		listingFailed = true
		c.logger.Warn("docker hub listing wholly failed", "owner", owner, "error", err)
	case err != nil:
		c.logger.Warn("docker hub listing partially failed", "owner", owner, "fetched", len(repos), "error", err)
	default:
		c.logger.Info("docker hub wildcard expanded", "owner", owner, "repos", len(repos))
	}
	for i := range repos {
		if ctx.Err() != nil {
			return results, attempted, listingFailed
		}
		repo := &repos[i]
		name := repo.Owner + "/" + repo.Repo
		key := registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}
		if seen[key] {
			continue
		}
		seen[key] = true
		attempted++
		repo.TagCount = tagCount(ctx, c, repo.Owner, repo.Repo)
		results = append(results, *repo)
		c.logger.Debug("docker hub repo collected", "repo", name,
			"pulls", repo.Pulls, "tags", repo.TagCount)
	}
	return results, attempted, listingFailed
}

// collectExplicit fetches each non-wildcard ref unless it was already
// collected via a wildcard expansion (tracked in seen, keyed by the repo
// pair itself).
func collectExplicit(ctx context.Context, c *Client, refs []registry.RepoRef, seen map[registry.RepoRef]bool) (results []registry.Entry, attempted int) {
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		if ctx.Err() != nil {
			return results, attempted
		}
		name := ref.Owner + "/" + ref.Repo
		if seen[ref] {
			continue
		}
		seen[ref] = true
		attempted++

		repoURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", ref.Owner, ref.Repo)
		repoData, err := get(ctx, c, repoURL)
		if err != nil {
			if ctx.Err() != nil {
				// In-flight fetch cancelled by shutdown: expected, so it is
				// recorded at Debug rather than reported as a registry failure
				// (mirrors the GHCR scrape site).
				c.logger.Debug("docker hub fetch cancelled", "repo", name, "error", err)
				return results, attempted
			}
			c.logger.Error("docker hub fetch failed", "repo", name, "error", err)
			continue
		}

		pullCount, err := parseRepoMeta(repoData)
		if err != nil {
			c.logger.Error("docker hub parse failed", "repo", name, "error", err)
			continue
		}

		tags := tagCount(ctx, c, ref.Owner, ref.Repo)
		results = append(results, registry.Entry{
			Owner:    ref.Owner,
			Repo:     ref.Repo,
			Pulls:    pullCount,
			TagCount: tags,
		})
		c.logger.Debug("docker hub repo collected", "repo", name, "pulls", pullCount, "tags", tags)
	}
	return results, attempted
}

// listRepos paginates the Docker Hub owner listing endpoint. Returns
// entries with Owner/Repo/Pulls populated; TagCount is left 0 for the
// caller to fill separately.
func listRepos(ctx context.Context, c *Client, owner string) ([]registry.Entry, error) {
	var repos []registry.Entry
	hitCap := true
	maxPages := c.ownerPageCap()

	for page := 1; page <= maxPages; page++ {
		if ctx.Err() != nil {
			return repos, ctx.Err()
		}
		url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/?page_size=%d&page=%d", owner, pageSize, page)
		data, err := get(ctx, c, url)
		if err != nil {
			return repos, fmt.Errorf("list repos page %d: %w", page, err)
		}

		pageRepos, more, err := parseRepoListPage(data, owner)
		if err != nil {
			return repos, fmt.Errorf("parse repo list: %w", err)
		}
		repos = append(repos, pageRepos...)

		if !more {
			hitCap = false
			break
		}
	}
	if hitCap {
		c.logger.Warn("docker hub owner listing hit page cap; results may be truncated",
			"owner", owner, "max_pages", maxPages)
	}

	return repos, nil
}

// tagCount fetches a repo's total tag count with a single page_size=1
// request, reading the tags listing's own top-level "count" field — the
// registry's exact total regardless of tag cardinality. On fetch or
// parse failure it logs a WARN and returns 0 so the caller emits no
// image_tags series for the repo this cycle rather than a wrong value
// (the skip-don't-zero rule; image_pulls_total is unaffected).
func tagCount(ctx context.Context, c *Client, owner, repo string) int {
	name := owner + "/" + repo
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags/?page_size=1&page=1", name)
	data, err := get(ctx, c, url)
	if err != nil {
		if ctx.Err() != nil {
			// A stop, not a registry failure; the GHCR scrape site records
			// the same state at Debug.
			c.logger.Debug("docker hub tag count fetch cancelled", "repo", name, "error", err)
			return 0
		}
		c.logger.Warn("docker hub tag count fetch failed", "repo", name, "error", err)
		return 0
	}
	n, err := parseTagCount(data)
	if err != nil {
		c.logger.Warn("docker hub tag count parse failed", "repo", name, "error", err)
		return 0
	}
	return n
}

// degraded reports whether a Docker Hub collection result is severely
// degraded: zero results with at least one attempt, or more than half of
// attempts failed.
func degraded(results []registry.Entry, attempted int) bool {
	if attempted == 0 {
		return false
	}
	if len(results) == 0 {
		return true
	}
	return len(results)*2 < attempted
}

// parseRepoMeta parses a single Docker Hub repo metadata response,
// returning the pull count. It is the pure parse core behind Collect's
// explicit-ref path.
//
// pull_count is REQUIRED: absent, null or negative is an error, never a
// value, because 0 is a legitimate pull count and image_pulls_total is
// cumulative — silently exporting 0 for a repo that has pulls would look
// like a regression to every downstream alert. Same skip-don't-zero rule
// parseTagCount applies to the tag count.
func parseRepoMeta(data []byte) (int64, error) {
	var resp struct {
		PullCount *int64 `json:"pull_count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	if resp.PullCount == nil || *resp.PullCount < 0 {
		return 0, errPullCountInvalid
	}
	return *resp.PullCount, nil
}

// parseRepoListPage parses one page of the Docker Hub owner-listing
// response: the page's repos (Owner from the request, Repo the listed
// name, TagCount left 0) plus whether a further page is offered. The
// upstream "next" token is an absolute URL the caller does not follow,
// composing each request from its own page index instead. The whole PAGE
// fails on an over-count response, a result without a usable pull_count,
// or every result dropped as unsafe — zero repos with a nil error reads as
// a legitimately empty owner; one unsafe name beside usable ones is dropped.
func parseRepoListPage(data []byte, owner string) ([]registry.Entry, bool, error) {
	var resp struct {
		Next    string `json:"next"`
		Results []struct {
			PullCount *int64 `json:"pull_count"`
			Name      string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false, err
	}
	if len(resp.Results) > pageSize {
		return nil, false, fmt.Errorf("listing page carried %d results for a page_size=%d request", len(resp.Results), pageSize)
	}
	repos := make([]registry.Entry, 0, len(resp.Results))
	for _, r := range resp.Results {
		// Repo name comes from the Docker Hub listing JSON (registry data,
		// not env config) and flows into the tags URL via tagCount. Route
		// it through urlsafe so a crafted/garbled listing response cannot
		// inject path-traversal or query chars into an outbound URL segment,
		// matching the GHCR scraper's package-list guard. Legitimate
		// Docker Hub names are always URL-safe, so this only drops names the
		// registry could not legitimately produce.
		if !urlsafe.IsSafeURLSegment(r.Name) {
			continue
		}
		if r.PullCount == nil || *r.PullCount < 0 {
			return nil, false, fmt.Errorf("repo %q: %w", r.Name, errPullCountInvalid)
		}
		repos = append(repos, registry.Entry{
			Owner: owner,
			Repo:  r.Name,
			Pulls: *r.PullCount,
		})
	}
	if len(repos) == 0 && len(resp.Results) > 0 {
		return nil, false, fmt.Errorf("all %d listed repo names rejected as unsafe", len(resp.Results))
	}
	return repos, resp.Next != "", nil
}

// errPullCountInvalid is returned when a Docker Hub response decodes as
// JSON but carries no usable cumulative pull count ("pull_count" absent,
// null or negative) — a format-change signal, distinct from malformed JSON.
var errPullCountInvalid = errors.New("pull count missing or negative")

// errTagCountInvalid is returned by parseTagCount when the response
// decodes as JSON but carries no usable total ("count" absent or
// negative) — a format-change signal, distinct from malformed JSON.
var errTagCountInvalid = errors.New("tag count missing or negative")

// parseTagCount parses the Docker Hub tags-listing response's top-level
// "count" field — the registry's own total tag count for the repo. A
// response without a non-negative count is an error so a malformed or
// reshaped response can never flow into the image_tags gauge as a bogus
// value. Pure parse core behind the per-repo tag-count fetch.
func parseTagCount(data []byte) (int, error) {
	var resp struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	if resp.Count == nil || *resp.Count < 0 {
		return 0, errTagCountInvalid
	}
	return *resp.Count, nil
}

// get is the single retry-wrapped HTTP GET used by every Docker Hub
// helper. The response body is capped at httpx.DefaultMaxBodyBytes
// (10 MB) by the library unless the caller's retryOpts override it.
func get(ctx context.Context, c *Client, url string) ([]byte, error) {
	return httpx.GetBytes(ctx, c.http, url, c.retryOpts...)
}

// ownerPageCap resolves c.pageCap against the default for owner listing.
func (c *Client) ownerPageCap() int {
	if c.pageCap > 0 {
		return c.pageCap
	}
	return maxOwnerPages
}
