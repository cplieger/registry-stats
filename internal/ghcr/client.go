package ghcr

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/api"
	"github.com/cplieger/registry-stats/v2/internal/model"
)

// Options configures GHCR-specific scraper policy. Its zero value
// selects production defaults (DefaultMinPacing + DefaultPacingJitter)
// so main.go can pass ghcr.Options{} and preserve the pre-c3 2-5s
// per-package pacing byte-for-byte.
type Options struct {
	MinPacing    time.Duration
	PacingJitter time.Duration
}

// DefaultMinPacing and DefaultPacingJitter are the production pacing
// values applied when an Options field is zero. collect() adds a
// uniformly distributed jitter in [0, DefaultPacingJitter) to
// DefaultMinPacing to space out consecutive GHCR scrape requests.
const (
	DefaultMinPacing    = 2 * time.Second
	DefaultPacingJitter = 3 * time.Second
)

// Client implements api.RegistrySource for the GitHub Container
// Registry. Construct via NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.GetOption
	opts      Options
}

// NewClient returns a Client that uses the provided *http.Client for
// all outbound requests, applying retryOpts to each call via
// httpx.GetBytes. opts configures GHCR-specific pacing; its zero value
// selects DefaultMinPacing + DefaultPacingJitter. A nil logger falls
// back to slog.Default.
func NewClient(client *http.Client, retryOpts []httpx.GetOption, opts Options, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http:      client,
		retryOpts: retryOpts,
		logger:    logger,
		opts:      opts,
	}
}

// Name identifies this source in logs and in the per-source health
// ratio. Matches the "ghcr" const used on the HTTP API surface.
func (c *Client) Name() string { return c.Source().String() }

// Source returns the typed model.RegistrySource the orchestrator
// uses to route entries into snap.GHCR without a string compare.
// model.SourceGHCR.String() must stay equal to Name() — both read
// as "ghcr" on the wire.
func (c *Client) Source() model.RegistrySource { return model.SourceGHCR }

// Collect gathers download counts for every ref in refs. Wildcard refs
// are expanded via buildPackageList before scraping; explicit refs are
// scraped as-is. Packages whose scrape fails are NOT appended so a
// transient error cannot inject a false zero into the exposed gauge
// (the per-day delta is computed downstream by Prometheus/Mimir, not here).
//
// entries carry the GHCR-relevant fields (Owner, Repo, and the scraped
// download count in Pulls); TagCount stays 0 — GHCR exposes no tag
// count, so no image_tags series is emitted for its packages. attempted
// counts per-package scrape attempts (listing failures do not contribute
// to the per-package health ratio). healthy is true when there were no
// per-package scrape failures, or package failures were at most half
// of the attempts (a minority or a tie); see pkgHealthy.
func (c *Client) Collect(
	ctx context.Context,
	refs []model.RepoRef,
) (entries []model.RegistryEntry, attempted int, healthy bool) {
	return collect(ctx, c, refs)
}

// Compile-time assertion: *Client satisfies api.RegistrySource.
var _ api.RegistrySource = (*Client)(nil)

// collect is the shared implementation behind Client.Collect. Returns
// the pre-refactor result shape plus attempted count (total scrapes
// across successes and failures) so the caller can decide its return
// values.
func collect(ctx context.Context, c *Client, refs []model.RepoRef) (results []model.RegistryEntry, attempted int, healthy bool) {
	failures := 0
	pkgParseFailures := 0
	total := 0
	packages, listingFailures, listingParseFailures := buildPackageList(ctx, c.http, c.logger, refs, c.retryOpts)
	failures += listingFailures

	for _, ref := range packages {
		// Space out every request (including the first) with randomized
		// delay to avoid rate limits. The Docker Hub pagination that
		// usually runs just before GHCR can queue many consecutive
		// requests, so leading pacing smooths the transition between
		// registries.
		timer := time.NewTimer(c.pacingDelay())
		select {
		case <-ctx.Done():
			timer.Stop()
			c.logger.Warn("ghcr collection interrupted by context cancellation",
				"collected", len(results), "remaining", len(packages)-total,
				"error", ctx.Err())
			return results, total, pkgHealthy(failures, listingFailures, total)
		case <-timer.C:
		}

		total++
		res := c.scrapePackage(ctx, ref)
		if !res.ok {
			failures++
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
		results = append(results, res.stat)
	}

	// Surface listing-page format changes even when no packages were
	// scraped. The per-package majority check below requires total > 0,
	// which is false when the listing itself failed, so listing format
	// changes need their own signal.
	if listingParseFailures > 0 {
		c.logger.Error("ghcr package listing HTML format may have changed",
			"listing_parse_failures", listingParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	// Surface a format-change ERROR as soon as a majority of scrapes
	// hit format errors, not only when all of them do. Log-based
	// alerting can then trigger proactively before the registry goes
	// fully dark.
	// Per-package parse failures only: listing parse failures have their own
	// dedicated ERROR above and are not counted in total. Mirrors pkgHealthy,
	// which already subtracts listingFailures from its ratio.
	if total > 0 && pkgParseFailures*2 > total {
		c.logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", total, "parse_failures", pkgParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	return results, total, pkgHealthy(failures, listingFailures, total)
}

// pacingDelay returns the inter-request delay for GHCR scrapes: the
// configured minimum (DefaultMinPacing when unset) plus uniform jitter
// in [0, jitter) (DefaultPacingJitter when unset). Because both defaults
// are positive and replace any non-positive configured value, the jitter
// bound is always positive, so rand.Int64N always receives a positive
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
	jitter := time.Duration(rand.Int64N(int64(pacingJitter))) //nolint:gosec // G404: jitter, not crypto
	return pacingMin + jitter
}

// scrapeResult is the outcome of one package scrape: stat is valid only
// when ok is true; parseFailed marks an ErrHTMLFormatChanged so the
// caller can tally format drift separately from transport failures.
type scrapeResult struct {
	stat        model.RegistryEntry
	ok          bool
	parseFailed bool
}

// scrapePackage scrapes a single package's download count and classifies
// the outcome, logging the failure (and a rate-limit hint) on error. On
// success it returns the populated stat with ok=true; on any failure ok
// is false so the caller leaves the package out of results — a transient
// error must not inject a false zero into the exposed gauge (the per-day
// delta is computed downstream by Prometheus/Mimir, not here).
func (c *Client) scrapePackage(ctx context.Context, ref model.RepoRef) scrapeResult {
	downloads, err := scrapeDownloads(ctx, c.http, ref.Owner, ref.Repo, c.retryOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// In-flight scrape cancelled by shutdown/deadline; expected, log at Debug not
			// WARN (mirrors the expandWildcard listing site). Failure classification unchanged.
			c.logger.Debug("ghcr scrape cancelled", "package", ref.Owner+"/"+ref.Repo, "error", err)
			return scrapeResult{parseFailed: errors.Is(err, ErrHTMLFormatChanged)}
		}
		c.logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo, "error", err)
		if errors.Is(err, httpx.ErrRateLimited) {
			c.logger.Warn("ghcr rate limited", "package", ref.Owner+"/"+ref.Repo,
				"hint", "consider increasing pacing delay or reducing package count")
		}
		return scrapeResult{parseFailed: errors.Is(err, ErrHTMLFormatChanged)}
	}
	c.logger.Debug("ghcr package collected", "package", ref.Owner+"/"+ref.Repo, "downloads", downloads)
	return scrapeResult{
		stat: model.RegistryEntry{Owner: ref.Owner, Repo: ref.Repo, Pulls: downloads},
		ok:   true,
	}
}

// pkgHealthy reports the per-package health verdict. Listing failures are
// excluded from the ratio (they are folded into failures, then subtracted
// here): when no packages could be listed there is no per-package data to
// judge by, so the verdict defaults to healthy and the empty-snapshot
// logic handles the listing-failure case separately. Otherwise a cycle is
// healthy when there were no package failures, or they were a minority.
func pkgHealthy(failures, listingFailures, total int) bool {
	pkgFailures := failures - listingFailures
	// Healthy when package failures are not a majority (at most half),
	// matching the sibling dockerhub.Degraded boundary (healthy when
	// len(results)*2 >= attempted, i.e. failures are at most half) and the
	// in-file per-package parse-majority check (pkgParseFailures*2 > total),
	// both feeding the same collect_errors_total ratio. The prior
	// pkgFailures<total only flagged a TOTAL GHCR outage, so
	// collect_errors_total{source="ghcr"} silently under-reported a
	// majority-but-not-total scrape failure.
	return pkgFailures == 0 || (total > 0 && pkgFailures*2 <= total)
}
