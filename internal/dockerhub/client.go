// Package dockerhub collects pull counts for public Docker Hub repositories
// through the unauthenticated /v2/ API, expanding "owner/*" refs against the
// owner listing.
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
	"slices"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
	"github.com/cplieger/registry-stats/v2/internal/urlsafe"
	"github.com/cplieger/runesafe/v2"
)

// Docker Hub refuses anonymous owner-listing requests whose offset reaches
// 100, so pageSize 99 is the largest size with a legal second page and
// maxOwnerPages stops the walk there: a third page would answer 403 rather
// than data. Reaching the cap means the owner publishes more than
// pageSize*maxOwnerPages repositories and this walk holds only part of it.
// listingElemCap bounds one page's decode cost with headroom for upstream
// page-size widening. A body larger than 1 MiB is a format signal, not
// content; the largest measured full upstream page is about 75 KB, leaving
// about 14x headroom.
const (
	maxOwnerPages  = 2
	pageSize       = 99
	listingElemCap = 4 * pageSize
	listingBodyCap = 1 << 20
)

// Client is the Docker Hub source (it satisfies collect.Source at the wiring
// site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.GetOption
}

// Options configures NewClient beyond the required HTTP client: the
// per-request retry options and required logger.
type Options struct {
	// Logger receives the client's logs; required.
	Logger *slog.Logger
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx
	// defaults.
	RetryOpts []httpx.GetOption
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	return &Client{
		http:      client,
		retryOpts: opts.RetryOpts,
		logger:    opts.Logger,
	}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare and to derive the "dockerhub" log label
// (registry.DockerHub.String()).
func (c *Client) Source() registry.ID { return registry.DockerHub }

// Collect gathers pull counts for every ref in refs. fetched and attempted
// count successful and total per-repo metadata fetches. Repos whose counts
// came from a wildcard owner listing are entries but not fetches.
// listingFailed reports a wildcard owner listing that failed without yielding
// any usable repos. Cancellation itself never sets listingFailed; a wholesale
// failure recorded before the cancellation persists. An explicit ref is
// counted in attempted before its metadata fetch, so cancellation during that
// fetch leaves it in attempted but not fetched.
func (c *Client) Collect(ctx context.Context, refs []registry.RepoRef) (entries []registry.Entry, fetched, attempted int, listingFailed bool) {
	wildcardResults, seen, listingFailed := c.collectWildcards(ctx, refs)
	explicitResults, explicitAttempted := c.collectExplicit(ctx, refs, seen)
	entries = slices.Concat(wildcardResults, explicitResults)
	return entries, len(explicitResults), explicitAttempted, listingFailed
}

// collectWildcards expands every "*" ref into concrete repo entries.
// The returned seen map tracks which repos were collected so
// collectExplicit can skip duplicates.
//
// listingFailed is true when at least one owner listing wholly failed
// (see collectWildcardRef).
func (c *Client) collectWildcards(ctx context.Context, refs []registry.RepoRef) (results []registry.Entry, seen map[registry.RepoRef]bool, listingFailed bool) {
	seen = make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		refResults, refFailed := c.collectWildcardRef(ctx, ref.Owner, seen)
		results = append(results, refResults...)
		if refFailed {
			listingFailed = true
		}
	}
	return results, seen, listingFailed
}

// collectWildcardRef lists one owner's public repos, deduping against the
// shared seen map (mutated in place). listingFailed reports a wholesale
// listing outage for this owner. A partial failure, a legitimately empty owner
// and a cancelled cycle all leave it false; a stop is not an outage.
func (c *Client) collectWildcardRef(ctx context.Context, owner string, seen map[registry.RepoRef]bool) (results []registry.Entry, listingFailed bool) {
	repos, advertised, err := c.listRepos(ctx, owner)
	switch {
	case ctx.Err() != nil:
		// A stop is not a classification.
	case err != nil && len(repos) == 0:
		listingFailed = true
		c.logger.Warn("docker hub listing wholly failed",
			"owner", owner,
			"advertised", advertised,
			"shape_change", shapeChanged(err),
			"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
	case err != nil:
		c.logger.Warn("docker hub listing partially failed",
			"owner", owner,
			"repos", len(repos),
			"advertised", advertised,
			"shape_change", shapeChanged(err),
			"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
	case len(repos) == 0:
		c.logger.Error("docker hub wildcard expanded", "owner", owner, "repos", 0, "advertised", advertised)
	default:
		c.logger.Info("docker hub wildcard expanded", "owner", owner, "repos", len(repos), "advertised", advertised)
	}
	for i := range repos {
		repo := repos[i]
		name := repo.Owner + "/" + repo.Repo
		key := registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}
		if seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, repo)
		c.logger.Debug("docker hub repo collected", "repo", name, "pulls", repo.Pulls)
	}
	return results, listingFailed
}

// collectExplicit fetches each non-wildcard ref unless it was already
// collected earlier in the cycle.
func (c *Client) collectExplicit(ctx context.Context, refs []registry.RepoRef, seen map[registry.RepoRef]bool) (results []registry.Entry, attempted int) {
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
		repoData, err := c.get(ctx, repoURL)
		if err != nil {
			if ctx.Err() != nil {
				// A stop, not a registry failure.
				c.logger.Debug("docker hub fetch cancelled", "repo", name,
					"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
				return results, attempted
			}
			c.logger.Warn("docker hub fetch failed", "repo", name,
				"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			continue
		}

		pullCount, err := parseRepoMeta(repoData)
		if err != nil {
			c.logger.Error("docker hub parse failed",
				"repo", name,
				"shape_change", shapeChanged(err),
				"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
			continue
		}

		results = append(results, registry.Entry{
			Owner: ref.Owner,
			Repo:  ref.Repo,
			Pulls: pullCount,
		})
		c.logger.Debug("docker hub repo collected", "repo", name, "pulls", pullCount)
	}
	return results, attempted
}

// listRepos paginates the Docker Hub owner listing endpoint. advertised is
// the total the owner listing publishes for itself, so the caller can report
// how much of the owner this walk holds. A listing upstream says is complete
// but that served a different number of distinct repositories than its own
// total is an error; a walk that stopped early returns what it collected with
// advertised set.
func (c *Client) listRepos(ctx context.Context, owner string) (entries []registry.Entry, advertised int, err error) {
	seen := make(map[registry.RepoRef]bool)
	complete := false

	for page := 1; page <= maxOwnerPages; page++ {
		if ctx.Err() != nil {
			return entries, advertised, ctx.Err()
		}
		url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/?page_size=%d&page=%d", owner, pageSize, page)
		data, err := c.get(ctx, url)
		if err != nil {
			return entries, advertised, fmt.Errorf("list repos page %d: %w", page, err)
		}

		pageRepos, more, pageTotal, err := parseRepoListPage(data, owner)
		if err != nil {
			return entries, advertised, fmt.Errorf("parse repo list page %d: %w", page, err)
		}
		if page == 1 {
			advertised = pageTotal
		}
		entries = appendDistinct(entries, seen, pageRepos)

		if !more {
			complete = true
			break
		}
	}
	if !complete {
		c.logger.Warn("docker hub owner listing hit page cap; results may be truncated",
			"owner", owner, "repos", len(entries), "advertised", advertised, "max_pages", maxOwnerPages)
		return entries, advertised, nil
	}
	if len(entries) != advertised {
		return entries, advertised, fmt.Errorf("%w: owner listing served %d distinct repositories against the %d it advertises",
			errResponseUnparsable, len(entries), advertised)
	}

	return entries, advertised, nil
}

// appendDistinct appends the page's repositories that entries does not
// already hold, recording each in seen. listRepos compares its distinct
// total with the total the listing advertises, so a row repeated across
// pages must not inflate that count.
func appendDistinct(entries []registry.Entry, seen map[registry.RepoRef]bool, page []registry.Entry) []registry.Entry {
	for _, repo := range page {
		key := registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, repo)
	}
	return entries
}

// parseRepoMeta parses a single Docker Hub repo metadata response,
// returning the pull count.
//
// pull_count is REQUIRED: absent, null or negative is an error, never a
// value, because 0 is a legitimate pull count and image_pulls_total is
// cumulative — silently exporting 0 for a repo that has pulls would look
// like a regression to every downstream alert.
func parseRepoMeta(data []byte) (int64, error) {
	var resp struct {
		PullCount *int64 `json:"pull_count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
	}
	if resp.PullCount == nil || *resp.PullCount < 0 {
		return 0, errPullCountInvalid
	}
	return *resp.PullCount, nil
}

// parseRepoListPage parses one page of the Docker Hub owner-listing
// response. The upstream "next" token is an absolute URL the caller
// does not follow, composing each request from its own page index
// instead. total is the owner's repository count as the envelope
// advertises it, and it is REQUIRED: it is the only value that can
// prove a completed walk collected everything, which listRepos owns.
// The whole PAGE fails on an over-count response, a missing total,
// any retained result without a usable pull_count, or every result
// dropped as unsafe; zero repos with a nil error and a zero total
// reads as a legitimately empty owner.
func parseRepoListPage(data []byte, owner string) (entries []registry.Entry, more bool, total int, err error) {
	var resp struct {
		Count   *int   `json:"count"`
		Next    string `json:"next"`
		Results []struct {
			PullCount *int64 `json:"pull_count"`
			Name      string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
	}
	if len(resp.Results) > listingElemCap {
		return nil, false, 0, fmt.Errorf("%w: listing page carried more than %d results for a page_size=%d request",
			errResponseUnparsable, listingElemCap, pageSize)
	}
	repos := make([]registry.Entry, 0, len(resp.Results))
	for _, res := range resp.Results {
		// Repo names become published label values. Apply the same
		// allowlist that defines errRepoNameUnsafe before retaining them.
		if !urlsafe.IsSafeURLSegment(res.Name) {
			continue
		}
		if res.PullCount == nil || *res.PullCount < 0 {
			return nil, false, 0, fmt.Errorf("repo %q: %w", res.Name, errPullCountInvalid)
		}
		repos = append(repos, registry.Entry{Owner: owner, Repo: res.Name, Pulls: *res.PullCount})
	}
	if len(repos) == 0 && len(resp.Results) > 0 {
		return nil, false, 0, fmt.Errorf("all %d listed repo names rejected: %w", len(resp.Results), errRepoNameUnsafe)
	}
	if resp.Count == nil || *resp.Count < 0 {
		return nil, false, 0, fmt.Errorf("%w: listing total missing or negative", errResponseUnparsable)
	}
	return repos, resp.Next != "", *resp.Count, nil
}

// errPullCountInvalid is returned when a Docker Hub response decodes as
// JSON but carries no usable cumulative pull count — a format-change
// signal, distinct from malformed JSON.
var errPullCountInvalid = errors.New("pull count missing or negative")

// errRepoNameUnsafe classifies a page whose names Docker Hub cannot serve.
var errRepoNameUnsafe = errors.New("listed repo names rejected as unsafe")

// errResponseUnparsable classifies any post-200 schema failure as a
// format-change signal rather than a transport one: a json/v2 decode
// rejection, or a decoded envelope that contradicts what was asked
// for. json/v2 is case-sensitive and skips a case-variant member name
// silently, leaving the field nil, so a required-field sentinel
// classifies that case: errPullCountInvalid for a repo's own pull_count,
// and this sentinel for the listing envelope's count.
var errResponseUnparsable = errors.New("docker hub response did not decode")

func shapeChanged(err error) bool {
	return errors.Is(err, errPullCountInvalid) ||
		errors.Is(err, errRepoNameUnsafe) ||
		errors.Is(err, errResponseUnparsable)
}

// get is the single retry-wrapped HTTP GET used by every Docker Hub helper.
// listingBodyCap always wins because options are applied left to right.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	opts := make([]httpx.GetOption, 0, len(c.retryOpts)+1)
	opts = append(opts, c.retryOpts...)
	opts = append(opts, httpx.WithMaxBodyBytes(listingBodyCap))
	data, err := httpx.GetBytes(ctx, c.http, url, opts...)
	if err != nil {
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return nil, fmt.Errorf("%w: %w", errResponseUnparsable, err)
		}
		return nil, err
	}
	return data, nil
}
