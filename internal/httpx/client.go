// Package httpx holds the HTTP primitives shared by registry-stats'
// registry clients: a preconfigured *http.Client, the hub.docker.com /
// github.com redirect allowlist, exponential-backoff-with-jitter retry,
// Retry-After parsing, and a connection-reuse drain helper.
//
// The package has no knowledge of Docker Hub or GHCR response shapes;
// it speaks only in url strings, headers, and bytes. Registry clients
// (internal/dockerhub, internal/ghcr) import this as their sole
// transport dependency.
//
// Tests pass short BaseDelay values via Options so the retry burn time
// stays in the millisecond range; production code leaves BaseDelay at
// DefaultBaseDelay (1s).
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrRateLimited is the typed sentinel callers use with errors.Is when
// they want to detect a 429 response without string-matching. Surfaces
// through *StatusError.Is so wrapping with fmt.Errorf(... %w ...) is
// preserved across the retry loop's exhaustion wrap.
var ErrRateLimited = errors.New("rate limited")

// ErrServerError is the typed sentinel for upstream 5xx responses. Same
// errors.Is semantics as ErrRateLimited; preserved for callers that
// want to distinguish "retry burned out on server errors" from "client
// got 4xx we don't retry on".
var ErrServerError = errors.New("server error")

// StatusError represents a non-2xx response from Retry (either the
// surfaced 4xx that aborts immediately, or the last failure wrapped
// in "retries exhausted" after a 429/5xx run exhausts). Error() format
// is "HTTP <code> from <url>" — preserved byte-for-byte from the
// pre-typed implementation so callers that string-match on "HTTP 503"
// or "HTTP 429" in their error assertions keep working.
type StatusError struct {
	URL  string
	Code int
}

// Error returns the byte-identical message shape the pre-typed
// fmt.Errorf("HTTP %d from %s", ...) produced.
func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.Code, e.URL)
}

// Is reports whether this StatusError matches one of the two typed
// sentinels. 429 matches ErrRateLimited; any 5xx (500-599) matches
// ErrServerError. Unknown targets return false so errors.Is chains
// continue walking the error tree.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.Code == http.StatusTooManyRequests
	case ErrServerError:
		return e.Code >= 500 && e.Code < 600
	}
	return false
}

// DefaultBaseDelay is the production base for the exponential-backoff
// retry in Retry (1s × 2^attempt → 2s, 4s at attempts 1 and 2). Tests
// override via Options.BaseDelay to shrink the burn to milliseconds.
const DefaultBaseDelay = time.Second

// DefaultMaxAttempts caps Retry at three tries (one initial + two
// retries). The two retry waits are ~2s + 4s plus up to 500ms jitter,
// so a full burn stays under ~7s of sleep plus per-request time and
// always aborts early on ctx cancellation.
const DefaultMaxAttempts = 3

// DefaultMaxBodyBytes caps the LimitReader applied to successful response
// bodies at 10 MB. Matches the pre-refactor doGet ceiling: Docker Hub
// tag pages and GHCR HTML both fit well under this.
const DefaultMaxBodyBytes int64 = 10 << 20

// drainLimit is the cap io.CopyN enforces when draining a failed
// response body for connection reuse. 64 KB covers Docker Hub JSON
// errors and the truncated HTML GitHub returns on rate limits; larger
// bodies are abandoned and the transport drops the connection.
const drainLimit = 64 << 10

// redirectCap is the maximum number of hops RedirectPolicy allows per
// request. 5 is comfortably below Go's default of 10 and above any
// redirect chain these registries actually use (typically 0-2 hops).
const redirectCap = 5

// retryAfterCap is the maximum wait RedirectPolicy will honor from an
// upstream's Retry-After header. Caps at 60s so a misconfigured
// upstream cannot stall the collector past the next poll interval.
const retryAfterCap = 60 * time.Second

// NewClient returns an *http.Client preconfigured with the given
// request timeout and the RedirectPolicy allowlist. Callers share a
// single client across all outbound requests for connection pooling.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: RedirectPolicy,
	}
}

// Close drains idle connections on the client's transport. Call during
// graceful shutdown to avoid TIME_WAIT socket accumulation across
// restarts.
func Close(c *http.Client) {
	c.CloseIdleConnections()
}

// RedirectPolicy is the CheckRedirect hook for the registry HTTP
// client. Refuses more than redirectCap hops and any redirect to a
// host outside the docker.com / github.com family.
//
// CheckRedirect restricts redirect targets to the hub.docker.com /
// github.com suffixes the registry clients explicitly call. Go's
// default would follow up to 10 redirects to any host; if an upstream
// registry were compromised or served a poisoned Location header
// (cache attack), an unbounded policy would let this process be used
// as an SSRF pivot against LAN-only admin endpoints on the same proxy
// network (Mimir, Caddy admin, etc.). No auth headers flow over
// redirects either way (stdlib strips them across origins), so the
// policy is defense-in-depth rather than patching a live leak.
// Matches the PLEX-SEC-01 / PLEX-LS-SEC-01 posture applied to the
// plex apps.
func RedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= redirectCap {
		return errors.New("too many redirects")
	}
	host := req.URL.Hostname()
	switch {
	case host == "hub.docker.com",
		strings.HasSuffix(host, ".docker.com"),
		host == "github.com",
		strings.HasSuffix(host, ".github.com"),
		strings.HasSuffix(host, ".githubusercontent.com"):
		return nil
	default:
		return fmt.Errorf("refusing redirect to %s", host)
	}
}

// Options configures a single Retry call. Zero values mean "use the
// Default* constant": BaseDelay=0 → DefaultBaseDelay,
// MaxAttempts=0 → DefaultMaxAttempts, MaxBodyBytes=0 →
// DefaultMaxBodyBytes. SetHeaders (optional) lets callers add
// request-specific headers (e.g. User-Agent / Accept for GitHub HTML).
type Options struct {
	SetHeaders   func(*http.Request)
	BaseDelay    time.Duration
	MaxAttempts  int
	MaxBodyBytes int64
}

// Retry performs an HTTP GET with bounded exponential-backoff retry on
// 429 and 5xx responses. 4xx-other-than-429 and transport errors are
// returned immediately; hard failures don't warrant retrying.
//
// 429 responses with a Retry-After header honor that hint (capped at
// 60s) so callers don't hammer past an upstream's requested cooldown.
// Response bodies are capped via io.LimitReader and failed-body drains
// use Drain to preserve connection reuse on retry.
func Retry(ctx context.Context, client *http.Client, reqURL string, opts Options) ([]byte, error) {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	baseDelay := opts.BaseDelay
	if baseDelay <= 0 {
		baseDelay = DefaultBaseDelay
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	start := time.Now()
	var lastErr error
	var overrideWait time.Duration
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := overrideWait
			if delay <= 0 {
				delay = time.Duration(1<<attempt)*baseDelay +
					time.Duration(rand.IntN(500))*time.Millisecond //nolint:gosec // G404: backoff jitter
			}
			overrideWait = 0
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if opts.SetHeaders != nil {
			opts.SetHeaders(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			slog.Debug("http request failed, will retry",
				"url", reqURL, "attempt", attempt+1, "max_attempts", maxAttempts, "error", err)
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			overrideWait = ParseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Debug("rate limited by upstream",
				"url", reqURL, "attempt", attempt+1, "retry_after", overrideWait)
			Drain(resp.Body)
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, URL: reqURL}
			continue
		}
		if resp.StatusCode >= 500 {
			Drain(resp.Body)
			resp.Body.Close()
			lastErr = &StatusError{Code: resp.StatusCode, URL: reqURL}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			Drain(resp.Body)
			resp.Body.Close()
			return nil, &StatusError{Code: resp.StatusCode, URL: reqURL}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			slog.Warn("slow upstream response", "url", reqURL, "duration", elapsed.Round(time.Millisecond))
		}
		return body, nil
	}
	elapsed := time.Since(start)
	slog.Warn("http retries exhausted",
		"url", reqURL, "attempts", maxAttempts, "elapsed", elapsed.Round(time.Millisecond), "error", lastErr)
	return nil, fmt.Errorf("retries exhausted after %s: %w", elapsed.Round(time.Millisecond), lastErr)
}

// Drain reads and discards up to 64 KB of a response body to enable
// HTTP connection reuse. 64 KB covers Docker Hub JSON errors and the
// truncated HTML GitHub returns on rate limits; larger bodies are
// abandoned and the transport will drop the connection.
func Drain(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, drainLimit); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body", "error", err)
	}
}

// ParseRetryAfter interprets the Retry-After response header. Supports
// both the delta-seconds and HTTP-date forms. Caps the result at 60s
// so a misconfigured upstream can't stall the collector past the next
// poll. Returns 0 for missing, malformed, or past-dated values so the
// caller falls back to the default exponential backoff.
func ParseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		d := time.Duration(n) * time.Second
		if d <= 0 {
			// Overflow: n * time.Second wrapped past int64 max.
			return retryAfterCap
		}
		d = min(d, retryAfterCap)
		return d
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			d = min(d, retryAfterCap)
			return d
		}
	}
	return 0
}
