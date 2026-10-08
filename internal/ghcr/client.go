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

	"github.com/cplieger/registry-stats/internal/pacing"
	"github.com/cplieger/registry-stats/internal/registry"
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
// collect.Source at the wiring site in main). Construct via NewClient; the
// zero value is not usable.
type Client struct {
	http *http.Client
	// opts.Logger is required, so reads need no nil check.
	opts Options
}

// NewClient returns a Client that uses the provided *http.Client for all
// outbound requests, configured by opts. Every request it sends, retries and
// redirect hops included, starts DefaultMinPacing plus jitter in
// [0, DefaultPacingJitter) after the one before it.
func NewClient(client *http.Client, opts Options) *Client {
	//nolint:gosec // G404: jitter, not crypto
	gap := func() time.Duration { return DefaultMinPacing + rand.N(DefaultPacingJitter) }
	return &Client{http: pacing.Client(client, gap), opts: opts}
}

// Source returns the typed registry.ID the orchestrator uses to route
// entries without a string compare.
func (c *Client) Source() registry.ID { return registry.GHCR }

// Collect gathers download counts for every ref in refs. A failed scrape is
// absent rather than published as zero. A package interrupted by shutdown is
// neither attempted nor fetched.
func (c *Client) Collect(ctx context.Context, refs []registry.RepoRef) registry.Collection {
	var pkgParseFailures int
	packages, listed, listingFailed := c.buildPackageList(ctx, refs)
	out := registry.Collection{Listed: listed, ListingFailed: listingFailed}
	var cancelled bool

	for _, ref := range packages {
		stat, err := c.scrapePackage(ctx, ref)
		if err != nil && ctx.Err() != nil {
			cancelled = true
			break
		}

		out.Attempted++
		if err != nil {
			c.opts.Logger.Warn("ghcr scrape failed", "package", ref.Owner+"/"+ref.Repo,
				"error", errTextForLog(err))
			if errors.Is(err, errHTMLFormatChanged) {
				pkgParseFailures++
			}
			// A missing entry leaves the gauge unset for this cycle (the
			// dashboard's spanNulls bridges the gap); a 0 would inject a false
			// drop into the cumulative pull count.
			if isNotFound(err) {
				out.Absent = append(out.Absent, ref)
			} else {
				out.Unread = append(out.Unread, ref)
			}
			continue
		}
		out.Entries = append(out.Entries, stat)
	}

	// A majority rather than all, so alerting can fire before the registry
	// goes fully dark.
	if !cancelled && pkgParseFailures*2 > out.Attempted {
		c.opts.Logger.Error("ghcr HTML format may be changing, majority of scrapes hit format errors",
			"total", out.Attempted, "parse_failures", pkgParseFailures,
			"report_at", "https://github.com/cplieger/registry-stats/issues")
	}

	out.Fetched = len(out.Entries)
	out.Definitive = len(out.Entries) + len(out.Absent)
	return out
}

// ReadDetail reads one package's tagged and untagged version counts from its
// versions page, paced with Collect's requests, and logs a failed read at
// its cause's level. A 429 the retries could not clear satisfies
// errors.Is(err, httpx.ErrRateLimited).
func (c *Client) ReadDetail(ctx context.Context, ref registry.RepoRef) (registry.Detail, error) {
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/%s/versions?filters%%5Bversion_type%%5D=tagged",
		ref.Owner, url.PathEscape(ref.Repo))
	html, err := c.fetchHTML(ctx, pageURL)
	var tagged, untagged int64
	if err == nil {
		tagged, untagged, err = parseVersionCounts(html)
	}
	if err != nil {
		if ctx.Err() == nil {
			level := slog.LevelWarn
			if errors.Is(err, errHTMLFormatChanged) {
				level = slog.LevelError
			}
			c.opts.Logger.Log(ctx, level, "ghcr version read failed",
				"package", ref.Owner+"/"+ref.Repo, "error", errTextForLog(err))
		}
		return registry.Detail{}, err
	}
	return registry.Detail{Tagged: tagged, Untagged: untagged}, nil
}

// scrapePackage scrapes one package's download count and its "Last
// published" time. The caller reports and classifies failures. The error is
// errHTMLFormatChanged for format drift in the download count and carries
// ctx.Err() when a shutdown interrupted the scrape; on any error the entry is
// zero and the caller leaves the package out of results. An unreadable
// publish time is logged and leaves LastPushed zero.
func (c *Client) scrapePackage(ctx context.Context, ref registry.RepoRef) (registry.Entry, error) {
	// GitHub redirects an organization's /users/ URL to the repo-scoped package page
	// through the allowlisted policy, so this fetch needs no owner kind.
	pageURL := fmt.Sprintf("https://github.com/users/%s/packages/container/package/%s", ref.Owner, url.PathEscape(ref.Repo))
	html, err := c.fetchHTML(ctx, pageURL)
	if err != nil {
		return registry.Entry{}, err
	}
	downloads, err := parseDownloads(html)
	if err != nil {
		return registry.Entry{}, err
	}
	name := ref.Owner + "/" + ref.Repo
	published, err := parsePublished(html)
	if err != nil {
		c.opts.Logger.Warn("ghcr package field unreadable", "package", name,
			"field", "last_published", "error", errTextForLog(err))
	}
	c.opts.Logger.Debug("ghcr package collected", "package", name, "downloads", downloads)
	return registry.Entry{Owner: ref.Owner, Repo: ref.Repo, Pulls: downloads, LastPushed: published}, nil
}
