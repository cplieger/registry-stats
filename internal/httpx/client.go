// Package httpx re-exports github.com/cplieger/httpx with an Options
// struct adapter for backward compatibility with existing callers.
package httpx

import (
	"context"
	"io"
	"net/http"
	"time"

	lib "github.com/cplieger/httpx"
)

// Re-export sentinels and types from the library.
var (
	ErrRateLimited = lib.ErrRateLimited
	ErrServerError = lib.ErrServerError
)

// StatusError is the library's StatusError.
type StatusError = lib.StatusError

// DefaultBaseDelay is the production base for the exponential-backoff retry.
const DefaultBaseDelay = lib.DefaultBaseDelay

// DefaultMaxAttempts caps Retry at three tries.
const DefaultMaxAttempts = lib.DefaultMaxAttempts

// DefaultMaxBodyBytes caps response bodies at 10 MB.
const DefaultMaxBodyBytes = lib.DefaultMaxBodyBytes

// Options configures a single Retry call. Preserved for backward
// compatibility with existing callers (dockerhub, ghcr).
type Options struct {
	SetHeaders   func(*http.Request)
	BaseDelay    time.Duration
	MaxAttempts  int
	MaxBodyBytes int64
}

// Retry performs an HTTP GET with bounded exponential-backoff retry.
// Adapts the Options struct to the library's functional-option API.
func Retry(ctx context.Context, client *http.Client, reqURL string, opts Options) ([]byte, error) {
	var libOpts []lib.Option
	if opts.MaxAttempts > 0 {
		libOpts = append(libOpts, lib.WithMaxAttempts(opts.MaxAttempts))
	}
	if opts.BaseDelay > 0 {
		libOpts = append(libOpts, lib.WithBaseDelay(opts.BaseDelay))
	}
	if opts.MaxBodyBytes > 0 {
		libOpts = append(libOpts, lib.WithMaxBodyBytes(opts.MaxBodyBytes))
	}
	if opts.SetHeaders != nil {
		libOpts = append(libOpts, lib.WithHeaders(opts.SetHeaders))
	}
	return lib.Retry(ctx, client, reqURL, libOpts...)
}

// NewClient returns an *http.Client with DockerGitHubRedirectPolicy.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: RedirectPolicy,
	}
}

// Close drains idle connections on the client's transport.
func Close(c *http.Client) {
	lib.Close(c)
}

// RedirectPolicy is the Docker/GitHub redirect allowlist.
var RedirectPolicy = lib.DockerGitHubRedirectPolicy

// Drain reads and discards up to 64 KB of a response body for connection reuse.
func Drain(body io.ReadCloser) {
	lib.Drain(body)
}

// ParseRetryAfter interprets the Retry-After response header.
func ParseRetryAfter(h string) time.Duration {
	return lib.ParseRetryAfter(h)
}
