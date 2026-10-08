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
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/internal/pacing"
	"github.com/cplieger/registry-stats/internal/registry"
	"github.com/cplieger/registry-stats/internal/urlsafe"
	"github.com/cplieger/runesafe/v2"
)

// Docker Hub refuses an anonymous owner-listing offset at 100, so pageSize 99
// is the largest size with a legal second page and maxOwnerPages stops there;
// reaching the cap means this walk holds only part of the owner.
// listingElemCap refuses a page that served far more rows than pageSize asked
// for, before the total the envelope advertises is read.
// responseBodyCap bounds every response this client reads, listing pages and
// per-repo metadata alike; the largest measured upstream page is about 75 KB.
const (
	maxOwnerPages   = 2
	pageSize        = 99
	listingElemCap  = 4 * pageSize
	responseBodyCap = 1 << 20
)

// DefaultPacing is the production gap between two Docker Hub requests: 2 per
// second, two thirds of the 180 per minute the anonymous API advertises.
const DefaultPacing = 500 * time.Millisecond

// Client is the Docker Hub source (it satisfies collect.Source at the wiring
// site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http   *http.Client
	logger *slog.Logger
}

// Options configures NewClient beyond the required HTTP client.
type Options struct {
	// Logger receives the client's logs; required.
	Logger *slog.Logger
	// Pacing is the least time between the starts of two requests, listing,
	// metadata and tag reads, retries and redirect hops alike. Zero sends them
	// back to back.
	Pacing time.Duration
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	return &Client{
		http:   pacing.Client(client, func() time.Duration { return opts.Pacing }),
		logger: opts.Logger,
	}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare and to derive the "dockerhub" log label
// (registry.DockerHub.String()).
func (c *Client) Source() registry.ID { return registry.DockerHub }

// Collect gathers pull counts for every ref in refs. Cancellation never sets
// ListingFailed; a wholesale failure recorded before cancellation persists.
// An explicit ref interrupted during its metadata fetch is attempted but not
// fetched.
func (c *Client) Collect(ctx context.Context, refs []registry.RepoRef) registry.Collection {
	wildcardResults, seen, listed, listingFailed := c.collectWildcards(ctx, refs)
	explicit := c.collectExplicit(ctx, refs, seen)
	return registry.Collection{
		Entries:       slices.Concat(wildcardResults, explicit.entries),
		Absent:        explicit.absent,
		Unread:        explicit.unread,
		Listed:        listed,
		Fetched:       len(explicit.entries),
		Attempted:     explicit.attempted,
		Definitive:    len(explicit.entries) + len(explicit.absent),
		ListingFailed: listingFailed,
	}
}

// ReadDetail reads one repository's tag count from the first page of its tag
// listing, whose envelope states the total, and logs a failed read at its
// cause's level. A 429 the retries could not clear satisfies
// errors.Is(err, httpx.ErrRateLimited).
func (c *Client) ReadDetail(ctx context.Context, ref registry.RepoRef) (registry.Detail, error) {
	data, err := c.get(ctx, fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/tags?page_size=1", ref.Owner, ref.Repo))
	var tags int64
	if err == nil {
		tags, err = parseTagCount(data)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Log(ctx, failureLevel(err), "docker hub tag read failed",
				"repo", ref.Owner+"/"+ref.Repo, "error", errTextForLog(err))
		}
		return registry.Detail{}, err
	}
	return registry.Detail{Tagged: tags}, nil
}

// parseTagCount reads the tag total a tag-listing page advertises. The count
// is REQUIRED: absent, null or negative is a shape change, never a zero.
func parseTagCount(data []byte) (int64, error) {
	var resp struct {
		Count *int64 `json:"count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("%w: %w", errShapeChanged, err)
	}
	if resp.Count == nil || *resp.Count < 0 {
		return 0, fmt.Errorf("%w: tag count missing or negative", errShapeChanged)
	}
	return *resp.Count, nil
}

// collectWildcards expands every "*" ref into concrete repo entries.
// The returned seen map tracks which repos were collected so
// collectExplicit can skip duplicates, and listed names each owner whose
// listing was read whole.
//
// listingFailed is true when at least one owner listing wholly failed
// (see collectWildcardRef).
func (c *Client) collectWildcards(ctx context.Context, refs []registry.RepoRef) (results []registry.Entry, seen map[registry.RepoRef]bool, listed []string, listingFailed bool) {
	seen = make(map[registry.RepoRef]bool)
	for _, ref := range refs {
		if ref.Repo != "*" {
			continue
		}
		refResults, whole, refFailed := c.collectWildcardRef(ctx, ref.Owner, seen)
		results = append(results, refResults...)
		if whole {
			listed = append(listed, ref.Owner)
		}
		if refFailed {
			listingFailed = true
		}
	}
	return results, seen, listed, listingFailed
}

// errTextForLog bounds an upstream error string in a log attribute: long
// enough to diagnose a Docker Hub failure, short enough that one upstream
// message cannot dominate the alert-keyed line.
func errTextForLog(err error) string {
	return runesafe.SanitizeSingleLineBounded(err.Error(), 256)
}

// collectWildcardRef lists one owner's public repos, recording each repo in
// the shared seen map (mutated in place) so collectExplicit can skip a ref this
// expansion already covered. whole reports a listing read to its end.
// listingFailed reports a wholesale listing outage for this owner. A partial
// failure, a legitimately empty owner and a cancelled cycle all leave it
// false; a stop is not an outage.
func (c *Client) collectWildcardRef(ctx context.Context, owner string, seen map[registry.RepoRef]bool) (results []registry.Entry, whole, listingFailed bool) {
	repos, advertised, whole, err := c.listRepos(ctx, owner)
	switch {
	case ctx.Err() != nil:
		// A stop is not a classification.
		whole = false
	case err != nil && len(repos) == 0:
		listingFailed = true
		c.logger.Log(ctx, failureLevel(err), "docker hub listing wholly failed",
			"owner", owner,
			"advertised", advertised,
			"error", errTextForLog(err))
	case err != nil:
		c.logger.Log(ctx, failureLevel(err), "docker hub listing partially failed",
			"owner", owner,
			"repos", len(repos),
			"advertised", advertised,
			"error", errTextForLog(err))
	case len(repos) == 0:
		c.logger.Warn("docker hub wildcard expanded no repos", "owner", owner, "repos", 0, "advertised", advertised)
	default:
		c.logger.Info("docker hub wildcard expanded", "owner", owner, "repos", len(repos), "advertised", advertised)
	}
	for _, repo := range repos {
		seen[registry.RepoRef{Owner: repo.Owner, Repo: repo.Repo}] = true
		c.logger.Debug("docker hub repo collected", "repo", repo.Owner+"/"+repo.Repo, "pulls", repo.Pulls)
	}
	return repos, whole, listingFailed
}

// explicitResult is collectExplicit's per-cycle outcome.
type explicitResult struct {
	entries   []registry.Entry
	absent    []registry.RepoRef
	unread    []registry.RepoRef
	attempted int
}

// collectExplicit fetches each non-wildcard ref unless a wildcard already
// covered it, sorting each fetch by how it ended.
func (c *Client) collectExplicit(ctx context.Context, refs []registry.RepoRef, seen map[registry.RepoRef]bool) explicitResult {
	var out explicitResult
	for _, ref := range refs {
		if ref.Repo == "*" {
			continue
		}
		if ctx.Err() != nil {
			return out
		}
		if seen[ref] {
			continue
		}
		out.attempted++
		entry, outcome := c.fetchExplicit(ctx, ref)
		switch outcome {
		case fetched:
			out.entries = append(out.entries, entry)
		case notFound:
			out.absent = append(out.absent, ref)
		case unread:
			out.unread = append(out.unread, ref)
		case cancelled:
			out.unread = append(out.unread, ref)
			return out
		}
	}
	return out
}

// fetchOutcome is how one explicit metadata fetch ended.
type fetchOutcome int

const (
	fetched fetchOutcome = iota
	// notFound is Docker Hub's 404, a definitive absence.
	notFound
	unread
	cancelled
)

// fetchExplicit reads one repository's metadata and logs a failure at its
// cause's level; a shutdown is a stop, logged at debug.
func (c *Client) fetchExplicit(ctx context.Context, ref registry.RepoRef) (registry.Entry, fetchOutcome) {
	name := ref.Owner + "/" + ref.Repo
	repoData, err := c.get(ctx, fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", ref.Owner, ref.Repo))
	if err != nil {
		if ctx.Err() != nil {
			c.logger.Debug("docker hub fetch cancelled", "repo", name, "error", errTextForLog(err))
			return registry.Entry{}, cancelled
		}
		c.logger.Log(ctx, failureLevel(err), "docker hub fetch failed", "repo", name, "error", errTextForLog(err))
		if isNotFound(err) {
			return registry.Entry{}, notFound
		}
		return registry.Entry{}, unread
	}
	pullCount, updated, err := parseRepoMeta(repoData)
	if err != nil {
		c.logger.Error("docker hub parse failed", "repo", name, "error", errTextForLog(err))
		return registry.Entry{}, unread
	}
	c.logger.Debug("docker hub repo collected", "repo", name, "pulls", pullCount)
	return registry.Entry{Owner: ref.Owner, Repo: ref.Repo, Pulls: pullCount, Updated: updated}, fetched
}

// isNotFound reports Docker Hub answering 404, its answer for a repository
// that does not exist or is private.
func isNotFound(err error) bool {
	if status, ok := errors.AsType[*httpx.StatusError](err); ok {
		return status.Code == http.StatusNotFound
	}
	return false
}

// listRepos paginates the Docker Hub owner listing endpoint. advertised is the
// total its first page publishes, so the caller can report how much of the
// owner this walk holds, and whole reports a walk that reached the listing's
// end. errShapeChanged classifies a failed walk. A walk stopped at the page
// cap holds only part of the owner, so its totals do not reconcile: it returns
// errShapeChanged when its pages carried more rows than any page advertised,
// and otherwise warns and returns those rows with a nil error.
func (c *Client) listRepos(ctx context.Context, owner string) (entries []registry.Entry, advertised int, whole bool, err error) {
	advertisedMax := 0

	for page := 1; page <= maxOwnerPages; page++ {
		pageURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/?page_size=%d&page=%d", owner, pageSize, page)
		data, err := c.get(ctx, pageURL)
		if err != nil {
			return entries, advertised, false, fmt.Errorf("list repos page %d: %w", page, err)
		}

		pageRepos, more, pageTotal, err := parseRepoListPage(data, owner)
		if err != nil {
			return entries, advertised, false, fmt.Errorf("parse repo list page %d: %w", page, err)
		}
		if page == 1 {
			advertised = pageTotal
		}
		advertisedMax = max(advertisedMax, pageTotal)
		entries = append(entries, pageRepos...)

		if !more {
			return entries, advertised, true, nil
		}
	}
	if len(entries) > advertisedMax {
		return entries, advertised, false, fmt.Errorf("%w: owner listing yielded %d name-allowlisted repository rows against the %d the most any page advertised, on a walk stopped at the page cap",
			errShapeChanged, len(entries), advertisedMax)
	}
	c.logger.Warn("docker hub owner listing hit page cap; results may be truncated",
		"owner", owner, "repos", len(entries),
		"advertised", advertised, "max_pages", maxOwnerPages)
	return entries, advertised, false, nil
}

// parseRepoMeta parses one repo metadata response into its pull count and its
// optional last_updated time, zero when null or absent. pull_count is REQUIRED:
// absent, null or negative is a shape change, never a value, because 0 is a real
// count and a false 0 reads as a regression to every downstream alert. A
// last_updated that is not an RFC 3339 string is a shape change too.
func parseRepoMeta(data []byte) (int64, time.Time, error) {
	var resp struct {
		PullCount   *int64     `json:"pull_count"`
		LastUpdated *time.Time `json:"last_updated"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, time.Time{}, fmt.Errorf("%w: %w", errShapeChanged, err)
	}
	if resp.PullCount == nil || *resp.PullCount < 0 {
		return 0, time.Time{}, fmt.Errorf("%w: pull count missing or negative", errShapeChanged)
	}
	return *resp.PullCount, optionalTime(resp.LastUpdated), nil
}

func optionalTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// parseRepoListPage parses one page of the Docker Hub owner-listing
// response. The upstream "next" token is an absolute URL the caller does
// not follow, composing each request from its own page index instead.
// total is the owner's repository count as the envelope advertises it,
// and it is REQUIRED: the caller publishes it as `advertised` on every
// wildcard record. Zero repos with a nil error and a zero total reads as
// a legitimately empty owner. Each row's last_updated follows parseRepoMeta.
func parseRepoListPage(data []byte, owner string) (entries []registry.Entry, more bool, total int, err error) {
	var resp struct {
		Count   *int   `json:"count"`
		Next    string `json:"next"`
		Results []struct {
			PullCount   *int64     `json:"pull_count"`
			LastUpdated *time.Time `json:"last_updated"`
			Name        string     `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false, 0, fmt.Errorf("%w: %w", errShapeChanged, err)
	}
	if len(resp.Results) > listingElemCap {
		return nil, false, 0, fmt.Errorf("%w: listing page carried more than %d results for a page_size=%d request",
			errShapeChanged, listingElemCap, pageSize)
	}
	if resp.Count == nil || *resp.Count < 0 {
		return nil, false, 0, fmt.Errorf("%w: listing total missing or negative", errShapeChanged)
	}
	if len(resp.Results) > *resp.Count {
		return nil, false, 0, fmt.Errorf("%w: listing page carried %d results against the %d repositories it advertises for the whole owner",
			errShapeChanged, len(resp.Results), *resp.Count)
	}
	repos := make([]registry.Entry, 0, len(resp.Results))
	for _, res := range resp.Results {
		// Each name becomes a published label value and a log attribute, so it passes
		// the urlsafe allowlist and its per-segment length bound here; the joined
		// owner/name rule is upstream's grammar and both segments are already bounded,
		// so it is not re-checked.
		if !urlsafe.IsSafeURLSegment(res.Name) {
			continue
		}
		if res.PullCount == nil || *res.PullCount < 0 {
			return nil, false, 0, fmt.Errorf("%w: repo %q: pull count missing or negative", errShapeChanged, res.Name)
		}
		repos = append(repos, registry.Entry{Owner: owner, Repo: res.Name, Pulls: *res.PullCount, Updated: optionalTime(res.LastUpdated)})
	}
	if len(repos) == 0 && len(resp.Results) > 0 {
		return nil, false, 0, fmt.Errorf("%w: all %d listed repo names rejected as unsafe", errShapeChanged, len(resp.Results))
	}
	return repos, resp.Next != "", *resp.Count, nil
}

// errShapeChanged classifies every post-200 schema failure as a
// format-change signal rather than a transport one: a json/v2 decode
// rejection, or a decoded envelope that contradicts what was asked for.
// json/v2 is case-sensitive and skips a case-variant member name silently,
// leaving the field nil, so a required member that decodes to its zero
// value is classified here rather than read as a value.
var errShapeChanged = errors.New("docker hub response is not the documented shape")

func shapeChanged(err error) bool {
	return errors.Is(err, errShapeChanged)
}

func failureLevel(err error) slog.Level {
	if shapeChanged(err) {
		return slog.LevelError
	}
	return slog.LevelWarn
}

// get is the single retry-wrapped HTTP GET used by every Docker Hub helper.
func (c *Client) get(ctx context.Context, reqURL string) ([]byte, error) {
	data, err := httpx.GetBytes(ctx, c.http, reqURL,
		httpx.WithLogger(c.logger),
		httpx.WithExhaustedLevel(slog.LevelDebug),
		httpx.WithMaxBodyBytes(responseBodyCap))
	if err != nil {
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return nil, fmt.Errorf("%w: %w", errShapeChanged, err)
		}
		return nil, err
	}
	return data, nil
}
