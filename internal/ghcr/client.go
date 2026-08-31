package ghcr

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

// Options configures GHCR-specific scraper policy. Its zero value selects
// production defaults (DefaultMinPacing + DefaultPacingJitter).
type Options struct {
	// Logger receives the client's warnings; nil falls back to slog.Default.
	Logger *slog.Logger
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx
	// defaults.
	RetryOpts []httpx.GetOption
	// MinPacing and PacingJitter space out consecutive GHCR requests;
	// zero selects DefaultMinPacing / DefaultPacingJitter.
	MinPacing    time.Duration
	PacingJitter time.Duration
}

// DefaultMinPacing and DefaultPacingJitter are the production pacing
// values applied when an Options field is zero. Each request waits
// DefaultMinPacing plus a uniformly distributed jitter in
// [0, DefaultPacingJitter).
const (
	DefaultMinPacing    = 2 * time.Second
	DefaultPacingJitter = 3 * time.Second
)

// Client is the GitHub Container Registry source (it satisfies
// collect.Source at the wiring site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http *http.Client
	// opts is the ONE place an optional lives. Its Logger is resolved by
	// NewClient, so reads need no nil check.
	opts Options
	// pageCap overrides maxListingPages when non-zero; 0 = the default.
	// Written only by the in-package test that exercises the listing bound.
	pageCap int
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{http: client, opts: opts}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare and to derive the "ghcr" log label
// (registry.GHCR.String()).
func (c *Client) Source() registry.ID { return registry.GHCR }

// Collect gathers download counts for every ref in refs. Wildcard refs are
// expanded through the owner's packages listing before scraping; explicit
// refs are scraped as-is. A package whose scrape fails is NOT appended, so
// a transient error cannot inject a false zero into the exposed gauge, and
// TagCount stays 0 (GHCR exposes no tag count). attempted counts
// per-package scrape attempts; a package a shutdown interrupted counts as
// neither attempt nor failure. healthy is false when a wildcard listing
// wholly failed, or per-package failures were a majority (see cycleHealthy).
func (c *Client) Collect(
	ctx context.Context,
	refs []registry.RepoRef,
) (entries []registry.Entry, attempted int, healthy bool) {
	pkgFailures := 0
	pkgParseFailures := 0
	packages, listingWhollyFailed, listingParseFailures := c.buildPackageList(ctx, refs)

	for _, ref := range packages {
		if ctx.Err() != nil {
			c.logInterrupted(len(entries), len(packages)-attempted, ctx.Err())
			return entries, attempted, cycleHealthy(pkgFailures, attempted, listingWhollyFailed)
		}
		res := c.scrapePackage(ctx, ref)
		if res.cancelled {
			// The stop landed inside this scrape (its pacing wait or the
			// request itself), so it is not an attempt that failed.
			c.logInterrupted(len(entries), len(packages)-attempted, ctx.Err())
			return entries, attempted, cycleHealthy(pkgFailures, attempted, listingWhollyFailed)
		}

		attempted++
		if !res.ok {
			pkgFailures++
			if res.parseFailed {
				pkgParseFailures++
			}
			// Skip on failure rather than emitting a zero: a missing entry leaves this
			// package's gauge unset for the cycle (a brief series gap that the
			// dashboard's spanNulls bridges and Mimir's prior samples retain), whereas a
			// 0 would inject a false drop into the cumulative pull count and a large
			// negative daily delta.
			continue
		}
		entries = append(entries, res.stat)
	}

	// Surface listing-page format changes even when no package was scraped:
	// the per-package majority check below counts scrapes only, so it cannot
	// fire when none was attempted.
	if listingParseFailures > 0 {
		c.opts.Logger.Error("ghcr owner listing yielded no packages: owner has none, or the listing markup changed",
			"listing_parse_failures", listingParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	// A MAJORITY rather than all, so log-based alerting can trigger before
	// the registry goes fully dark. Listing drift has its own ERROR above.
	if pkgParseFailures*2 > attempted {
		c.opts.Logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", attempted, "parse_failures", pkgParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	return entries, attempted, cycleHealthy(pkgFailures, attempted, listingWhollyFailed)
}

// logInterrupted records what a cancelled cycle cost: the packages already
// collected, and the ones it never reached.
func (c *Client) logInterrupted(collected, remaining int, err error) {
	c.opts.Logger.Warn("ghcr collection interrupted by context cancellation",
		"collected", collected, "remaining", remaining, "error", err)
}

// pacingDelay returns the inter-request delay for GHCR requests: the
// configured minimum (DefaultMinPacing when unset) plus uniform jitter
// in [0, jitter) (DefaultPacingJitter when unset). Because both defaults
// are positive and replace any non-positive configured value, the jitter
// bound is always positive, so rand.N always receives a positive
// argument.
func (c *Client) pacingDelay() time.Duration {
	pacingMin := c.opts.MinPacing
	if pacingMin <= 0 {
		pacingMin = DefaultMinPacing
	}
	pacingJitter := c.opts.PacingJitter
	if pacingJitter <= 0 {
		pacingJitter = DefaultPacingJitter
	}
	jitter := rand.N(pacingJitter) //nolint:gosec // G404: jitter, not crypto
	return pacingMin + jitter
}

// listingPageCap resolves c.pageCap against the default listing bound.
func (c *Client) listingPageCap() int {
	if c.pageCap > 0 {
		return c.pageCap
	}
	return maxListingPages
}

// scrapeResult is the outcome of one package scrape: stat is valid only
// when ok is true; parseFailed marks an errHTMLFormatChanged so the caller
// can tally format drift separately from transport failures, and cancelled
// marks a shutdown, which is neither an attempt nor a failure.
type scrapeResult struct {
	stat        registry.Entry
	ok          bool
	parseFailed bool
	cancelled   bool
}

// scrapePackage scrapes a single package's download count and classifies
// the outcome, logging the failure (and a rate-limit hint) on error. On
// success it returns the populated stat with ok=true; on any failure ok
// is false so the caller leaves the package out of results — a transient
// error must not inject a false zero into the exposed gauge (the per-day
// delta is computed downstream by Prometheus/Mimir, not here).
func (c *Client) scrapePackage(ctx context.Context, ref registry.RepoRef) scrapeResult {
	downloads, err := c.scrapeDownloads(ctx, ref.Owner, ref.Repo)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Cancelled by shutdown/deadline, either waiting to be paced or in
			// flight; expected, so it is recorded at Debug and the caller ends
			// the cycle rather than counting a failure.
			c.opts.Logger.Debug("ghcr scrape cancelled", "package", ref.Owner+"/"+ref.Repo, "error", err)
			return scrapeResult{cancelled: true}
		}
		c.opts.Logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo, "error", err)
		if errors.Is(err, httpx.ErrRateLimited) {
			c.opts.Logger.Warn("ghcr rate limited", "package", ref.Owner+"/"+ref.Repo,
				"hint", "consider increasing pacing delay or reducing package count")
		}
		return scrapeResult{parseFailed: errors.Is(err, errHTMLFormatChanged)}
	}
	c.opts.Logger.Debug("ghcr package collected", "package", ref.Owner+"/"+ref.Repo, "downloads", downloads)
	return scrapeResult{
		stat: registry.Entry{Owner: ref.Owner, Repo: ref.Repo, Pulls: downloads},
		ok:   true,
	}
}

// cycleHealthy reports the cycle verdict: healthy when no wildcard owner
// listing wholly failed and per-package scrape failures were at most half
// of the attempts. A wholly-failed listing is unhealthy because an unknown
// number of packages went uncollected and no exported series can show its
// own absence — the state internal/dockerhub already reports unhealthy.
func cycleHealthy(pkgFailures, total int, listingWhollyFailed bool) bool {
	return !listingWhollyFailed && pkgFailures*2 <= total
}
