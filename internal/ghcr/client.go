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

// Options configures GHCR-specific scraper policy. Zero pacing fields select
// DefaultMinPacing and DefaultPacingJitter.
type Options struct {
	// Logger receives the client's logs; required.
	Logger *slog.Logger
	// RetryOpts apply to each call via httpx.GetBytes; nil means the httpx defaults.
	RetryOpts []httpx.GetOption
	// MinPacing and PacingJitter space out consecutive GHCR requests;
	// zero selects DefaultMinPacing / DefaultPacingJitter.
	MinPacing    time.Duration
	PacingJitter time.Duration
}

// DefaultMinPacing and DefaultPacingJitter are the production pacing
// values applied when an Options field is zero. Each paced interval is
// DefaultMinPacing plus a uniformly distributed jitter in
// [0, DefaultPacingJitter).
const (
	DefaultMinPacing    = 2 * time.Second
	DefaultPacingJitter = 3 * time.Second
)

// RequestTimeout bounds each attempt made by the shared production client.
const RequestTimeout = 30 * time.Second

const (
	documentedPackagesAtPageCap = 1500
	maximumGHCRFetches           = 1 + maxListingPages + documentedPackagesAtPageCap
	maximumDockerHubFetches      = 2
	maximumFetchDuration         = time.Duration(httpx.DefaultMaxAttempts)*RequestTimeout + time.Duration(httpx.DefaultMaxAttempts-1)*httpx.RetryAfterCap

	// MaximumCycleDuration bounds one cycle at the documented registry ceilings.
	// Docker Hub's two listing fetches run before GHCR; publication follows both.
	MaximumCycleDuration = time.Duration(maximumGHCRFetches+maximumDockerHubFetches)*maximumFetchDuration + time.Duration(maximumGHCRFetches-1)*(DefaultMinPacing+DefaultPacingJitter)
)

// Client is the GitHub Container Registry source (it satisfies
// collect.Source at the wiring site in main). Construct via NewClient; the zero value is not usable.
type Client struct {
	http *http.Client
	// opts.Logger is required, so reads need no nil check.
	opts Options
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts.
func NewClient(client *http.Client, opts Options) *Client {
	return &Client{http: client, opts: opts}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare.
func (c *Client) Source() registry.ID { return registry.GHCR }

// Collect gathers download counts for every ref in refs. A failed scrape is
// absent rather than published as zero. A package interrupted by shutdown is
// neither attempted nor fetched.
func (c *Client) Collect(ctx context.Context, refs []registry.RepoRef) registry.Collection {
	p := &pacer{delay: c.pacingDelay}
	var pkgParseFailures int
	packages, listingFailed := c.buildPackageList(ctx, p, refs)
	var entries []registry.Entry
	var attempted int

	for _, ref := range packages {
		stat, err := c.scrapePackage(ctx, p, ref)
		if err != nil && ctx.Err() != nil {
			return registry.Collection{
				Entries:       entries,
				Fetched:       len(entries),
				Attempted:     attempted,
				ListingFailed: listingFailed,
			}
		}

		attempted++
		if err != nil {
			if errors.Is(err, errHTMLFormatChanged) {
				pkgParseFailures++
			}
			// A missing entry leaves the gauge unset for this cycle (the
			// dashboard's spanNulls bridges the gap); a 0 would inject a false
			// drop into the cumulative pull count.
			continue
		}
		entries = append(entries, stat)
	}

	// A majority rather than all, so alerting can fire before the registry
	// goes fully dark.
	if pkgParseFailures*2 > attempted {
		c.opts.Logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", attempted, "parse_failures", pkgParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	return registry.Collection{
		Entries:       entries,
		Fetched:       len(entries),
		Attempted:     attempted,
		ListingFailed: listingFailed,
	}
}

// pacer spaces consecutive GHCR requests inside one Collect. The first
// wait of a sequence returns immediately: pacing is a property of the
// sequence, and a fresh cycle has no predecessor to space against.
// One Collect is sequential, so the started bit needs no lock.
type pacer struct {
	delay   func() time.Duration
	started bool
}

func (p *pacer) wait(ctx context.Context) error {
	if !p.started {
		p.started = true
		return nil
	}
	return httpx.SleepCtx(ctx, p.delay())
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

// scrapePackage scrapes one package's download count, reporting the failure
// itself so the caller only classifies it. The error is errHTMLFormatChanged
// for format drift and carries ctx.Err() when a shutdown interrupted the
// scrape; on any error the entry is zero and the caller leaves the package out
// of results.
func (c *Client) scrapePackage(ctx context.Context, p *pacer, ref registry.RepoRef) (registry.Entry, error) {
	downloads, err := c.scrapeDownloads(ctx, p, ref)
	if err != nil {
		if ctx.Err() != nil {
			// collect.Run logs cancellation once for the cycle; suppress the
			// alert-keyed source failure warning here.
			return registry.Entry{}, err
		}
		c.opts.Logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo,
			"error", errTextForLog(err))
		return registry.Entry{}, err
	}
	c.opts.Logger.Debug("ghcr package collected", "package", ref.Owner+"/"+ref.Repo, "downloads", downloads)
	return registry.Entry{Owner: ref.Owner, Repo: ref.Repo, Pulls: downloads}, nil
}
