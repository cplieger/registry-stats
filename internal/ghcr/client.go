package ghcr

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/cplieger/httpx"
	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/model"
)

// Options configures GHCR-specific scraper policy. Its zero value
// selects production defaults (DefaultPacingMin + DefaultPacingJitter)
// so main.go can pass ghcr.Options{} and preserve the pre-c3 2-5s
// per-package pacing byte-for-byte.
type Options struct {
	PacingMin    time.Duration
	PacingJitter time.Duration
}

// DefaultPacingMin and DefaultPacingJitter are the production pacing
// values applied when an Options field is zero. collect() adds a
// uniformly distributed jitter in [0, DefaultPacingJitter) to
// DefaultPacingMin to space out consecutive GHCR scrape requests.
const (
	DefaultPacingMin    = 2 * time.Second
	DefaultPacingJitter = 3 * time.Second
)

// Client implements api.RegistrySource for the GitHub Container
// Registry. Construct via NewClient; the zero value is not usable.
type Client struct {
	http      *http.Client
	logger    *slog.Logger
	retryOpts []httpx.Option
	opts      Options
}

// NewClient returns a Client that uses the provided *http.Client for
// all outbound requests, applying retryOpts to each call via
// httpx.Retry. opts configures GHCR-specific pacing; its zero value
// selects DefaultPacingMin + DefaultPacingJitter. A nil logger falls
// back to slog.Default.
func NewClient(client *http.Client, retryOpts []httpx.Option, opts Options, logger *slog.Logger) *Client {
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
// transient error cannot corrupt the daily-delta calculation.
//
// entries carry only the GHCR-relevant fields (Name, DownloadCount);
// PullCount / LastUpdated / Tags stay zero-valued. attempted counts
// per-package scrape attempts (listing failures do not contribute to
// the per-package health ratio). healthy mirrors the legacy formula:
// no per-package failures OR fewer failures than successes.
func (c *Client) Collect(
	ctx context.Context,
	refs []model.RepoRef,
) (entries []model.RegistryEntry, attempted int, healthy bool) {
	results, attempted, healthy := collect(ctx, c, refs)
	entries = make([]model.RegistryEntry, 0, len(results))
	for _, r := range results {
		entries = append(entries, model.RegistryEntry{
			Name:          r.Package,
			DownloadCount: r.DownloadCount,
		})
	}
	return entries, attempted, healthy
}

// Compile-time assertion: *Client satisfies api.RegistrySource.
var _ api.RegistrySource = (*Client)(nil)

// collect is the shared implementation behind Client.Collect. Returns
// the pre-refactor result shape plus attempted count (total scrapes
// across successes and failures) so the caller can decide its return
// values.
func collect(ctx context.Context, c *Client, refs []model.RepoRef) (results []model.GhcrStats, attempted int, healthy bool) {
	failures := 0
	parseFailures := 0
	total := 0
	packages, listingFailures, listingParseFailures := buildPackageList(ctx, c.http, refs, c.retryOpts)
	failures += listingFailures
	parseFailures += listingParseFailures

	for _, ref := range packages {
		// Space out every request (including the first) with randomized
		// delay to avoid rate limits. The Docker Hub pagination that
		// usually runs just before GHCR can queue many consecutive
		// requests, so leading pacing smooths the transition between
		// registries.
		pacingMin := c.opts.PacingMin
		if pacingMin <= 0 {
			pacingMin = DefaultPacingMin
		}
		pacingJitter := c.opts.PacingJitter
		if pacingJitter <= 0 {
			pacingJitter = DefaultPacingJitter
		}
		var jitter time.Duration
		if pacingJitter > 0 {
			jitter = time.Duration(rand.Int64N(int64(pacingJitter))) //nolint:gosec // G404: jitter, not crypto
		}
		delay := pacingMin + jitter
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			c.logger.Warn("ghcr collection interrupted by context cancellation",
				"collected", len(results), "remaining", len(packages)-total,
				"error", ctx.Err())
			pkgFailures := failures - listingFailures
			return results, total, pkgFailures == 0 || (total > 0 && pkgFailures < total)
		case <-timer.C:
		}

		total++
		downloads, err := scrapeDownloads(ctx, c.http, ref.Owner, ref.Repo, c.retryOpts)
		if err != nil {
			c.logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo, "error", err)
			if errors.Is(err, httpx.ErrRateLimited) {
				c.logger.Warn("ghcr rate limited", "package", ref.Owner+"/"+ref.Repo,
					"hint", "consider increasing pacing delay or reducing package count")
			}
			failures++
			if errors.Is(err, ErrHTMLFormatChanged) {
				parseFailures++
			}
			// Skip on failure: previous day's snapshot already has the
			// real count, so the daily-delta handler carries it forward
			// implicitly rather than treating a transient 429 as
			// "this package went to 0".
			continue
		}

		results = append(results, model.GhcrStats{
			Package:       ref.Owner + "/" + ref.Repo,
			DownloadCount: downloads,
		})
		c.logger.Debug("ghcr package collected", "package", ref.Owner+"/"+ref.Repo, "downloads", downloads)
	}

	// Surface listing-page format changes even when no packages were
	// scraped. The per-package majority check below requires total > 0,
	// which is false when the listing itself failed, so listing format
	// changes need their own signal.
	if listingParseFailures > 0 {
		c.logger.Error("ghcr package listing HTML format may have changed",
			"listing_parse_failures", listingParseFailures)
	}

	// Surface a format-change ERROR as soon as a majority of scrapes
	// hit format errors, not only when all of them do. Log-based
	// alerting can then trigger proactively before the registry goes
	// fully dark.
	if total > 0 && parseFailures*2 > total {
		c.logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", total, "parse_failures", parseFailures)
	}

	// Listing failures are logged but excluded from the per-package
	// health ratio: when no packages could be listed, there is no data
	// to judge health by, so we default to healthy. The degraded log
	// and empty snapshot-save logic handle the listing-failure case
	// separately.
	pkgFailures := failures - listingFailures
	healthy = pkgFailures == 0 || (total > 0 && pkgFailures < total)
	return results, total, healthy
}
