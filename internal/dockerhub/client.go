// Package dockerhub is the Docker Hub RegistrySource implementation. It
// collects per-repo pull counts and tag metadata via the unauthenticated
// Docker Hub /v2/ API, handling wildcard owner expansion, per-repo tag
// pagination, and severe-degradation detection.
//
// Contract boundaries kept intact through extraction:
//   - URL shapes (https://hub.docker.com/v2/repositories/{owner}/...) are
//     unchanged; httpx.DockerGitHubRedirectPolicy (wired on the shared
//     *http.Client in main.go) still enforces the SSRF allowlist.
//   - Response JSON parsing (pull_count, last_updated, per-tag images[])
//     matches the model.Snapshot.DockerHub shape byte-for-byte.
//   - Page caps (owner listing: 10 pages; per-repo tags: 50 pages; 100
//     items per page) and the "hit cap → warn log" signal are preserved
//     so dashboards that alert on truncation still see the same key set.
//
// The package exposes a *Client (for composition-root wiring via
// api.RegistrySource) plus the exported Degraded predicate (a pure
// function retained because its input-shape is what the legacy
// TestDockerHubDegraded matrix asserts against). All collection
// behavior is reached through *Client methods; there are no
// free-function shims.
package dockerhub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/model"
	"github.com/cplieger/registry-stats/internal/urlsafe"
)

// Pagination caps. Chosen well above realistic usage so normal traffic
// never hits them; hitting a cap is a signal that behaviour changed and
// the warn logs below surface it. OwnerPages and TagPages bound the
// worst case per collection (1000 repos/owner, 5000 tags/repo).
const (
	MaxOwnerPages = 10
	MaxTagPages   = 50
	PageSize      = 100
)

// Client implements api.RegistrySource for Docker Hub. Construct via
// NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.Option
	pageCap   int // overrides MaxOwnerPages + MaxTagPages when non-zero; 0 = use defaults
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, applying retryOpts to each call via httpx.Retry. A
// nil logger falls back to slog.Default. pageCap of 0 means "use the
// package-default caps"; any non-zero value applies to both owner
// listing and per-repo tag listing (used by tests to force the cap).
func NewClient(client *http.Client, retryOpts []httpx.Option, pageCap int, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http:      client,
		retryOpts: retryOpts,
		pageCap:   pageCap,
		logger:    logger,
	}
}

// Name identifies this source in logs and in the per-source health ratio.
// Matches the "dockerhub" const used on the HTTP API surface.
func (c *Client) Name() string { return c.Source().String() }

// Source returns the typed model.RegistrySource the orchestrator uses
// to route entries into snap.DockerHub without a string compare.
// model.SourceDockerHub.String() must stay equal to Name() — both
// read as "dockerhub" on the wire.
func (c *Client) Source() model.RegistrySource { return model.SourceDockerHub }

// Collect gathers pull counts and tag metadata for every ref in refs.
// Returns the per-repo entries plus the attempted count (including
// failures) and a healthy flag. healthy is false when the collection is
// severely degraded (see Degraded) OR when a wildcard owner-listing
// wholly failed — a non-nil listing error that yielded zero usable
// repos. The listing-failure signal is distinct because a wholesale
// listing outage leaves attempted == 0, which Degraded alone reads as
// healthy and would therefore mask a total Docker Hub outage.
//
// entries carry only the Docker Hub-relevant fields (Name, LastUpdated,
// PullCount, Tags); DownloadCount is left zero so later collect-level
// code can map entries into model.Snapshot.DockerHub without bleed.
func (c *Client) Collect(
	ctx context.Context,
	refs []model.RepoRef,
) (entries []model.RegistryEntry, attempted int, healthy bool) {
	results, attempted, listingFailed := collect(ctx, c, refs)
	entries = make([]model.RegistryEntry, 0, len(results))
	for _, r := range results {
		entries = append(entries, model.RegistryEntry{
			Name:        r.Repo,
			LastUpdated: r.LastUpdated,
			PullCount:   r.PullCount,
			Tags:        r.Tags,
		})
	}
	return entries, attempted, !Degraded(results, attempted) && !listingFailed
}

// Compile-time assertion: *Client satisfies api.RegistrySource.
var _ api.RegistrySource = (*Client)(nil)

// collect is the shared implementation behind Client.Collect. listingFailed
// propagates the wildcard wholesale-listing-failure signal (see
// collectWildcards) up to Collect so it can factor into the healthy verdict.
func collect(ctx context.Context, c *Client, refs []model.RepoRef) (results []model.RepoStats, attempted int, listingFailed bool) {
	wildcardResults, wildcardAttempted, seen, listingFailed := collectWildcards(ctx, c, refs)
	explicitResults, explicitAttempted := collectExplicit(ctx, c, refs, seen)
	return append(wildcardResults, explicitResults...), wildcardAttempted + explicitAttempted, listingFailed
}

// collectWildcards expands every "*" ref into concrete repo entries by
// listing the owner's public repos, then fetches each repo's tags. The
// returned seen map tracks which repos were collected so collectExplicit
// can skip duplicates.
//
// listingFailed is true when at least one wildcard owner-listing wholly
// failed: listRepos returned a non-nil error AND yielded zero usable
// repos. That case is distinct from a partial failure (some pages
// succeeded, so listRepos returned an error alongside real repos) and
// from a legitimately empty owner (nil error, zero repos) — only the
// wholesale outage flags listingFailed, so a total Docker Hub listing
// outage surfaces as unhealthy instead of an empty-but-healthy result.
func collectWildcards(ctx context.Context, c *Client, refs []model.RepoRef) (results []model.RepoStats, attempted int, seen map[string]bool, listingFailed bool) {
	seen = make(map[string]bool)
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
// repo's tags, deduping against seen (shared across wildcard refs and the
// later explicit pass, so this mutates it in place). listingFailed reports
// a wholesale listing outage for this owner: a non-nil listRepos error that
// yielded zero usable repos, in which case attempted won't increment for it
// and Degraded would read the empty result as healthy — so Collect must see
// the signal to report unhealthy. A partial failure (error alongside real
// repos) and a legitimately empty owner (nil error, zero repos) both return
// listingFailed=false, leaving the health verdict to the degradation path.
func collectWildcardRef(ctx context.Context, c *Client, owner string, seen map[string]bool) (results []model.RepoStats, attempted int, listingFailed bool) {
	repos, err := listRepos(ctx, c, owner)
	if err != nil {
		if len(repos) == 0 {
			listingFailed = true
			c.logger.Warn("docker hub listing wholly failed", "owner", owner, "error", err)
		} else {
			c.logger.Warn("docker hub listing partially failed", "owner", owner, "fetched", len(repos), "error", err)
		}
	}
	for i, r := range repos {
		if ctx.Err() != nil {
			return results, attempted, listingFailed
		}
		if seen[r.Repo] {
			continue
		}
		seen[r.Repo] = true
		attempted++
		repos[i].Tags = collectTags(ctx, c, r.Repo)
		results = append(results, repos[i])
		c.logger.Debug("docker hub repo collected", "repo", r.Repo, "pulls", r.PullCount, "tags", len(repos[i].Tags))
	}
	c.logger.Info("docker hub wildcard expanded", "owner", owner, "repos", len(repos))
	return results, attempted, listingFailed
}

// collectExplicit fetches each non-wildcard ref unless it was already
// collected via a wildcard expansion (tracked in seen).
func collectExplicit(ctx context.Context, c *Client, refs []model.RepoRef, seen map[string]bool) (results []model.RepoStats, attempted int) {
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		if ctx.Err() != nil {
			return results, attempted
		}
		name := ref.Owner + "/" + ref.Repo
		if seen[name] {
			continue
		}
		seen[name] = true
		attempted++

		repoURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", ref.Owner, ref.Repo)
		repoData, err := get(ctx, c, repoURL)
		if err != nil {
			c.logger.Error("docker hub fetch failed", "repo", name, "error", err)
			continue
		}

		pullCount, lastUpdated, err := ParseRepoMeta(repoData)
		if err != nil {
			c.logger.Error("docker hub parse failed", "repo", name, "error", err)
			continue
		}

		tags := collectTags(ctx, c, name)
		results = append(results, model.RepoStats{
			Repo:        name,
			PullCount:   pullCount,
			LastUpdated: lastUpdated,
			Tags:        tags,
		})
		c.logger.Debug("docker hub repo collected", "repo", name, "pulls", pullCount, "tags", len(tags))
	}
	return results, attempted
}

// listRepos paginates the Docker Hub owner listing endpoint. Returns
// model.RepoStats with Repo/PullCount/LastUpdated populated; Tags is
// left nil for the caller to fill separately.
func listRepos(ctx context.Context, c *Client, owner string) ([]model.RepoStats, error) {
	var repos []model.RepoStats
	hitCap := true
	maxPages := c.ownerPageCap()

	for page := 1; page <= maxPages; page++ {
		if ctx.Err() != nil {
			return repos, ctx.Err()
		}
		url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/?page_size=%d&page=%d", owner, PageSize, page)
		data, err := get(ctx, c, url)
		if err != nil {
			return repos, fmt.Errorf("list repos page %d: %w", page, err)
		}

		next, pageRepos, err := ParseRepoListPage(data, owner)
		if err != nil {
			return repos, fmt.Errorf("parse repo list: %w", err)
		}
		repos = append(repos, pageRepos...)

		if next == "" {
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

// collectTags fetches all tags for a Docker Hub repo in owner/name form.
// Parse failures and transport errors terminate pagination; what got
// fetched before the failure is returned. Mirrors the pre-refactor
// "log and break" semantics so the degradation flag upstream still sees
// an identical signal.
func collectTags(ctx context.Context, c *Client, repo string) []model.TagInfo {
	var tags []model.TagInfo
	hitCap := true
	maxPages := c.tagPageCap()

	for page := 1; page <= maxPages; page++ {
		if ctx.Err() != nil {
			return tags
		}
		tagsURL := fmt.Sprintf(
			"https://hub.docker.com/v2/repositories/%s/tags/?page_size=%d&page=%d",
			repo, PageSize, page)
		data, err := get(ctx, c, tagsURL)
		if err != nil {
			c.logger.Warn("docker hub tags fetch failed", "repo", repo, "page", page, "error", err)
			hitCap = false
			break
		}

		next, pageTags, err := ParseTagPage(data)
		if err != nil {
			c.logger.Warn("docker hub tags parse failed", "repo", repo, "error", err)
			hitCap = false
			break
		}
		tags = append(tags, pageTags...)

		if next == "" {
			hitCap = false
			break
		}
	}
	if hitCap {
		c.logger.Warn("docker hub tag listing hit page cap; tag list may be truncated",
			"repo", repo, "max_pages", maxPages)
	}

	return tags
}

// Degraded reports whether a Docker Hub collection result is severely
// degraded: zero results with at least one attempt, or more than half
// of attempts failed. Kept as an exported free function because its
// pure-input shape is what the legacy TestDockerHubDegraded case
// matrix asserts against.
func Degraded(results []model.RepoStats, attempted int) bool {
	if attempted == 0 {
		return false
	}
	if len(results) == 0 {
		return true
	}
	return len(results)*2 < attempted
}

// ParseRepoMeta parses a single Docker Hub repo metadata response,
// returning the pull count and last-updated timestamp. It is the pure
// parse core behind Collect's explicit-ref path, exported so parse-only
// tests and fuzzing can drive it without standing up an HTTP server.
func ParseRepoMeta(data []byte) (pullCount int64, lastUpdated string, err error) {
	var resp struct {
		LastUpdated string `json:"last_updated"`
		PullCount   int64  `json:"pull_count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", err
	}
	return resp.PullCount, resp.LastUpdated, nil
}

// ParseRepoListPage parses one page of the Docker Hub owner-listing
// response. It returns the "next" page token plus the page's repos with
// Repo set to "owner/name" (Tags left nil for the caller to fill). Pure
// parse core behind listRepos, exported for parse-only tests and fuzzing.
func ParseRepoListPage(data []byte, owner string) (next string, repos []model.RepoStats, err error) {
	var resp struct {
		Next    string `json:"next"`
		Results []struct {
			Name        string `json:"name"`
			LastUpdated string `json:"last_updated"`
			PullCount   int64  `json:"pull_count"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", nil, err
	}
	repos = make([]model.RepoStats, 0, len(resp.Results))
	for _, r := range resp.Results {
		// Repo name comes from the Docker Hub listing JSON (registry data,
		// not env config) and flows into the tags URL via collectTags. Route
		// it through urlsafe so a crafted/garbled listing response cannot
		// inject path-traversal or query chars into an outbound URL segment,
		// matching the GHCR scraper's ParsePackageList guard. Legitimate
		// Docker Hub names are always URL-safe, so this only drops names the
		// registry could not legitimately produce.
		if !urlsafe.IsSafeURLSegment(r.Name) {
			slog.Debug("skipping docker hub repo with unsafe name", "owner", owner, "name", r.Name)
			continue
		}
		repos = append(repos, model.RepoStats{
			Repo:        owner + "/" + r.Name,
			PullCount:   r.PullCount,
			LastUpdated: r.LastUpdated,
		})
	}
	return resp.Next, repos, nil
}

// ParseTagPage parses one page of the Docker Hub tags response. It
// returns the "next" page token plus the page's tags with empty-named
// entries filtered out, matching the contract that every collected tag
// carries a name. Pure parse core behind collectTags, exported for
// parse-only tests and fuzzing.
func ParseTagPage(data []byte) (next string, tags []model.TagInfo, err error) {
	var resp struct {
		Next    string          `json:"next"`
		Results []model.TagInfo `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", nil, err
	}
	for _, tag := range resp.Results {
		if tag.Name == "" {
			continue
		}
		tags = append(tags, tag)
	}
	return resp.Next, tags, nil
}

// get is the single retry-wrapped HTTP GET used by every Docker Hub
// helper. The response body is capped at httpx.DefaultMaxBodyBytes
// (10 MB) by the library unless the caller's retryOpts override it —
// matching the pre-library doGet ceiling, so no explicit cap is needed
// here.
func get(ctx context.Context, c *Client, url string) ([]byte, error) {
	return httpx.Retry(ctx, c.http, url, c.retryOpts...)
}

// ownerPageCap resolves c.pageCap against the default for owner listing.
func (c *Client) ownerPageCap() int {
	if c.pageCap > 0 {
		return c.pageCap
	}
	return MaxOwnerPages
}

// tagPageCap resolves c.pageCap against the default for tag listing.
func (c *Client) tagPageCap() int {
	if c.pageCap > 0 {
		return c.pageCap
	}
	return MaxTagPages
}
