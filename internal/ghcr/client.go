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
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx defaults.
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
	// opts.Logger is resolved by NewClient, so reads need no nil check.
	opts Options
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
// entries without a string compare.
func (c *Client) Source() registry.ID { return registry.GHCR }

// Collect gathers download counts for every ref in refs. Wildcard refs are
// expanded through the owner's packages listing before scraping; explicit
// refs are scraped as-is. A package whose scrape fails is NOT appended, so
// a transient error cannot inject a false zero into the exposed gauge.
// Successful entries leave TagCount nil because GHCR exposes no tag count.
// attempted counts per-package scrape attempts; a package a shutdown
// interrupted counts as neither attempt nor failure. listingFailed reports
// a wildcard listing that failed without yielding any usable package names.
func (c *Client) Collect(
	ctx context.Context,
	refs []registry.RepoRef,
) (entries []registry.Entry, attempted int, listingFailed bool) {
	pkgParseFailures := 0
	packages, listingWhollyFailed := c.buildPackageList(ctx, refs)

	for _, ref := range packages {
		if ctx.Err() != nil {
			c.logInterrupted(len(entries), len(packages)-attempted, ctx.Err())
			return entries, attempted, listingWhollyFailed
		}
		res := c.scrapePackage(ctx, ref)
		if res.cancelled {
			c.logInterrupted(len(entries), len(packages)-attempted, ctx.Err())
			return entries, attempted, listingWhollyFailed
		}

		attempted++
		if !res.ok {
			if res.parseFailed {
				pkgParseFailures++
			}
			// A missing entry leaves the gauge unset for this cycle (the
			// dashboard's spanNulls bridges the gap); a 0 would inject a false
			// drop into the cumulative pull count.
			continue
		}
		entries = append(entries, res.stat)
	}

	// A majority rather than all, so alerting can fire before the registry
	// goes fully dark.
	if pkgParseFailures*2 > attempted {
		c.opts.Logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", attempted, "parse_failures", pkgParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	return entries, attempted, listingWhollyFailed
}

// logInterrupted records the packages already collected and the ones a
// cancelled cycle never reached.
func (c *Client) logInterrupted(collected, remaining int, err error) {
	c.opts.Logger.Warn("ghcr collection interrupted by context cancellation",
		"collected", collected, "remaining", remaining, "error", err)
}

// pacingDelay returns the inter-request delay for GHCR requests: the
// configured minimum (DefaultMinPacing when unset) plus uniform jitter in
// [0, jitter) (DefaultPacingJitter when unset). Both defaults are positive,
// so rand.N always receives a positive argument.
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

// scrapeResult is the outcome of one package scrape: stat is valid only
// when ok is true; parseFailed marks an errHTMLFormatChanged; cancelled
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
// is false so the caller leaves the package out of results.
func (c *Client) scrapePackage(ctx context.Context, ref registry.RepoRef) scrapeResult {
	downloads, err := c.scrapeDownloads(ctx, ref.Owner, ref.Repo)
	if err != nil {
		if ctx.Err() != nil {
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
