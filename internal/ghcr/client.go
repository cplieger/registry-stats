package ghcr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/registry-stats/v2/internal/registry"
)

// Options configures NewClient beyond the required HTTP client.
type Options struct {
	// Logger receives the client's logs; required.
	Logger *slog.Logger
}

// DefaultMinPacing and DefaultPacingJitter are the production pacing
// values. Each paced interval is DefaultMinPacing plus a uniformly
// distributed jitter in [0, DefaultPacingJitter).
const (
	DefaultMinPacing    = 2 * time.Second
	DefaultPacingJitter = 3 * time.Second
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
	p := &pacer{}
	var pkgParseFailures int
	packages, listingFailed := c.buildPackageList(ctx, p, refs)
	var entries []registry.Entry
	var attempted int
	var cancelled bool

	for _, ref := range packages {
		stat, err := c.scrapePackage(ctx, p, ref)
		if err != nil && ctx.Err() != nil {
			cancelled = true
			break
		}

		attempted++
		if err != nil {
			c.opts.Logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo,
				"error", errTextForLog(err))
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
	if !cancelled && pkgParseFailures*2 > attempted {
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

// pacer spaces consecutive GHCR requests inside one Collect by
// DefaultMinPacing plus uniform jitter in [0, DefaultPacingJitter).
// The first wait of a sequence returns immediately: pacing is a
// property of the sequence, and a fresh cycle has no predecessor to
// space against. One Collect is sequential, so the started bit needs
// no lock, and the zero value paces correctly.
type pacer struct{ started bool }

func (p *pacer) wait(ctx context.Context) error {
	if !p.started {
		p.started = true
		return nil
	}
	//nolint:gosec // G404: jitter, not crypto
	return httpx.SleepCtx(ctx, DefaultMinPacing+rand.N(DefaultPacingJitter))
}

// scrapePackage scrapes one package's download count. The caller reports and
// classifies failures. The error is errHTMLFormatChanged for format drift and
// carries ctx.Err() when a shutdown interrupted the scrape; on any error the
// entry is zero and the caller leaves the package out of results.
func (c *Client) scrapePackage(ctx context.Context, p *pacer, ref registry.RepoRef) (registry.Entry, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", ref.Owner, url.PathEscape(ref.Repo))
	html, err := c.fetchHTML(ctx, p, pageURL)
	if err != nil {
		return registry.Entry{}, err
	}
	downloads, err := parseDownloads(html)
	if err != nil {
		return registry.Entry{}, err
	}
	c.opts.Logger.Debug("ghcr package collected", "package", ref.Owner+"/"+ref.Repo, "downloads", downloads)
	return registry.Entry{Owner: ref.Owner, Repo: ref.Repo, Pulls: downloads}, nil
}
