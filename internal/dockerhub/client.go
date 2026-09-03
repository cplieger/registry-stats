// Package dockerhub collects pull counts for public Docker Hub repositories
// through the unauthenticated /v2/ API, expanding "owner/*" refs against the
// owner listing.
//
// Outbound requests use the caller-supplied *http.Client, on which main
// wires httpx.DockerGitHubRedirectPolicy as the SSRF allowlist.
package dockerhub

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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
// reaches 198 repositories. maxOwnerPages only bounds a non-terminating
// next token; it is not a truncation signal. listingElemCap bounds one
// page's decode cost with headroom for upstream page-size widening.
const (
	maxOwnerPages  = 10
	pageSize       = 99
	listingElemCap = 4 * pageSize
)

// Client is the Docker Hub source (it satisfies collect.Source at the wiring
// site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.GetOption
}

// Options configures NewClient beyond the required HTTP client: the
// per-request retry options and the logger, each with a meaningful zero
// (httpx defaults; slog.Default).
type Options struct {
	// Logger receives the client's logs; nil falls back to slog.Default.
	Logger *slog.Logger
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx
	// defaults.
	RetryOpts []httpx.GetOption
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	logger := cmp.Or(opts.Logger, slog.Default())
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

// Collect gathers pull counts for every ref in refs. Returns the per-repo
// entries plus the attempted count (including failures). listingFailed reports
// a wildcard owner listing that failed without yielding any usable repos.
// Cancellation itself never sets listingFailed; a wholesale failure recorded
// before the cancellation persists. An explicit ref is counted in attempted
// before its metadata fetch, so cancellation during that fetch leaves it in
// attempted but not entries.
func (c *Client) Collect(ctx context.Context, refs []registry.RepoRef) (entries []registry.Entry, attempted int, listingFailed bool) {
	wildcardResults, wildcardAttempted, seen, listingFailed := c.collectWildcards(ctx, refs)
	explicitResults, explicitAttempted := c.collectExplicit(ctx, refs, seen)
	entries = slices.Concat(wildcardResults, explicitResults)
	attempted = wildcardAttempted + explicitAttempted
	return entries, attempted, listingFailed
}

// collectWildcards expands every "*" ref into concrete repo entries.
// The returned seen map tracks which repos were collected so
// collectExplicit can skip duplicates.
//
// listingFailed is true when at least one owner listing wholly failed
// (see collectWildcardRef).
func (c *Client) collectWildcards(ctx context.Context, refs []registry.RepoRef) (results []registry.Entry, attempted int, seen map[registry.RepoRef]bool, listingFailed bool) {
	seen = make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		refResults, refAttempted, refFailed := c.collectWildcardRef(ctx, ref.Owner, seen)
		results = append(results, refResults...)
		attempted += refAttempted
		if refFailed {
			listingFailed = true
		}
	}
	return results, attempted, seen, listingFailed
}

// collectWildcardRef lists one owner's public repos, deduping against the
// shared seen map (mutated in place). listingFailed reports a wholesale
// listing outage for this owner. A partial failure, a legitimately empty owner
// and a cancelled cycle all leave it false; a stop is not an outage.
func (c *Client) collectWildcardRef(ctx context.Context, owner string, seen map[registry.RepoRef]bool) (results []registry.Entry, attempted int, listingFailed bool) {
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
			"fetched", len(repos),
			"advertised", advertised,
			"shape_change", shapeChanged(err),
			"error", runesafe.SanitizeSingleLineBounded(err.Error(), 256))
	default:
		c.logger.Info("docker hub wildcard expanded", "owner", owner, "repos", len(repos), "advertised", advertised)
	}
	for i := range repos {
		if ctx.Err() != nil {
			return results, attempted, listingFailed
		}
		repo := repos[i]
		name := repo.Owner + "/" + repo.Repo
		key := registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}
		if seen[key] {
			continue
		}
		seen[key] = true
		attempted++
		results = append(results, repo)
		c.logger.Debug("docker hub repo collected", "repo", name, "pulls", repo.Pulls)
	}
	return results, attempted, listingFailed
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
			c.logger.Error("docker hub fetch failed", "repo", name,
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
// but that served fewer distinct repositories than its own total is an error;
// a walk that stopped early returns what it collected with advertised set.
func (c *Client) listRepos(ctx context.Context, owner string) (entries []registry.Entry, advertised int, err error) {
	var repos []registry.Entry
	seen := make(map[registry.RepoRef]bool)
	complete := false

	for page := 1; page <= maxOwnerPages; page++ {
		if ctx.Err() != nil {
			return repos, advertised, ctx.Err()
		}
		url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/?page_size=%d&page=%d", owner, pageSize, page)
		data, err := c.get(ctx, url)
		if err != nil {
			return repos, advertised, fmt.Errorf("list repos page %d: %w", page, err)
		}

		pageRepos, more, pageTotal, err := parseRepoListPage(data, owner)
		if err != nil {
			return repos, advertised, fmt.Errorf("parse repo list page %d: %w", page, err)
		}
		if page == 1 {
			advertised = pageTotal
		}
		for _, repo := range pageRepos {
			key := registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}
			if seen[key] {
				continue
			}
			seen[key] = true
			repos = append(repos, repo)
		}

		if !more {
			complete = true
			break
		}
	}
	if !complete {
		return repos, advertised, nil
	}
	if len(repos) < advertised {
		return repos, advertised, fmt.Errorf("%w: owner listing served %d of the %d repositories it advertises",
			errResponseUnparsable, len(repos), advertised)
	}

	return repos, advertised, nil
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
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil || tok.Kind() != '{' {
		return nil, false, 0, fmt.Errorf("%w: listing page is not a JSON object", errResponseUnparsable)
	}
	var (
		next  string
		count *int
		seen  int
		repos = make([]registry.Entry, 0, pageSize)
	)
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
		}
		if tok.Kind() == '}' {
			break
		}
		switch tok.String() {
		case "count":
			if err := json.UnmarshalDecode(dec, &count); err != nil {
				return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
			}
		case "next":
			if err := json.UnmarshalDecode(dec, &next); err != nil {
				return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
			}
		case "results":
			open, err := dec.ReadToken()
			if err != nil {
				return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
			}
			if open.Kind() == 'n' {
				continue
			}
			if open.Kind() != '[' {
				return nil, false, 0, fmt.Errorf("%w: results is not an array", errResponseUnparsable)
			}
			for dec.PeekKind() != ']' {
				if seen >= listingElemCap {
					return nil, false, 0, fmt.Errorf("%w: listing page carried more than %d results for a page_size=%d request", errResponseUnparsable, listingElemCap, pageSize)
				}
				var res struct {
					PullCount *int64 `json:"pull_count"`
					Name      string `json:"name"`
				}
				if err := json.UnmarshalDecode(dec, &res); err != nil {
					return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
				}
				seen++
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
			if _, err := dec.ReadToken(); err != nil {
				return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
			}
		default:
			if err := dec.SkipValue(); err != nil {
				return nil, false, 0, fmt.Errorf("%w: %w", errResponseUnparsable, err)
			}
		}
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, false, 0, fmt.Errorf("%w: trailing data after the listing object", errResponseUnparsable)
	}
	if len(repos) == 0 && seen > 0 {
		return nil, false, 0, fmt.Errorf("all %d listed repo names rejected: %w", seen, errRepoNameUnsafe)
	}
	if count == nil || *count < 0 {
		return nil, false, 0, fmt.Errorf("%w: listing total missing or negative", errResponseUnparsable)
	}
	return repos, next != "", *count, nil
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
// for. A case-variant member name is NOT one of these — json/v2 is
// case-sensitive and ignores it silently, leaving the field nil, so a
// required-field sentinel classifies that case.
var errResponseUnparsable = errors.New("docker hub response did not decode")

func shapeChanged(err error) bool {
	return errors.Is(err, errPullCountInvalid) ||
		errors.Is(err, errRepoNameUnsafe) ||
		errors.Is(err, errResponseUnparsable)
}

// get is the single retry-wrapped HTTP GET used by every Docker Hub
// helper. The response body is capped at httpx.DefaultMaxBodyBytes
// (10 MB) unless the caller's retryOpts override it.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	return httpx.GetBytes(ctx, c.http, url, c.retryOpts...)
}
