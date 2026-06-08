package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/registry-stats/internal/api"
	"github.com/cplieger/registry-stats/internal/httpx"
)

// Compile-time assertion that *http.Client satisfies api.HTTPDoer.
// Kept in the httpx test file because httpx.NewClient is the canonical
// constructor; a signature change to HTTPDoer trips this build before
// any caller tries to pass the client in.
var _ api.HTTPDoer = (*http.Client)(nil)

// shortOpts returns an Options with a 1 ms base delay so retry-bearing
// tests finish in milliseconds instead of ~6s of real backoff.
func shortOpts() httpx.Options {
	return httpx.Options{BaseDelay: time.Millisecond, MaxBodyBytes: 10 << 20}
}

func TestRetry_success_first_try(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
}

func TestRetry_error_status_fails_fast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts()); err == nil {
		t.Error("expected error for 404")
	}
}

func TestRetry_recovers_after_retryable_status(t *testing.T) {
	tests := []struct {
		name       string
		failStatus int
	}{
		{"429 then 200", http.StatusTooManyRequests},
		{"500 then 200", http.StatusInternalServerError},
		{"503 then 200", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(tt.failStatus)
					_, _ = w.Write([]byte("transient"))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok-body"))
			}))
			defer srv.Close()

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
			if err != nil {
				t.Fatalf("Retry after retry = %v, want nil", err)
			}
			if string(body) != "ok-body" {
				t.Errorf("body = %q, want %q", body, "ok-body")
			}
			if got := calls.Load(); got != 2 {
				t.Errorf("server call count = %d, want 2 (1 fail + 1 success)", got)
			}
		})
	}
}

func TestRetry_exhausts_on_persistent_failure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	if err == nil {
		t.Fatalf("Retry after all retries = nil, want error")
	}
	if body != nil {
		t.Errorf("body = %v, want nil after exhausted retries", body)
	}
	if !strings.Contains(err.Error(), "retries exhausted") {
		t.Errorf("error = %v, want containing %q", err, "retries exhausted")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %v, want wrapped HTTP 503 cause", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("server call count = %d, want 3 attempts", got)
	}
}

func TestRetry_aborts_on_context_cancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	opts := httpx.Options{BaseDelay: 100 * time.Millisecond, MaxBodyBytes: 10 << 20}
	_, err := httpx.Retry(ctx, srv.Client(), srv.URL, opts)
	if err == nil {
		t.Fatalf("Retry after ctx cancel = nil, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled wrapped", err)
	}
	if got := calls.Load(); got > 2 {
		t.Errorf("server call count = %d, want <= 2 (stopped mid-retry)", got)
	}
}

func TestRetry_body_size_limit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, 1024)
		// write 11 MB; Retry should truncate at MaxBodyBytes.
		for range 11 * 1024 {
			_, _ = w.Write(buf)
		}
	}))
	defer srv.Close()

	opts := httpx.Options{BaseDelay: time.Millisecond, MaxBodyBytes: 10 << 20}
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, opts)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if len(body) != 10<<20 {
		t.Errorf("body size = %d, want exactly %d (MaxBodyBytes)", len(body), 10<<20)
	}
}

func TestRetry_honors_retry_after(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	start := time.Now()
	body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Retry = %v, want nil", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	// Retry-After of 1s should make the second attempt land at ~1s, not
	// the ~2ms exponential backoff (1ms << 1).
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 900ms (Retry-After: 1 honored)", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"delta seconds", "5", 5 * time.Second},
		{"delta seconds with whitespace", "  10  ", 10 * time.Second},
		{"delta seconds capped at 60", "120", 60 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"malformed", "soon", 0},
		{"http-date future capped", now.Add(5 * time.Minute).UTC().Format(http.TimeFormat), 60 * time.Second},
		{"http-date past", now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := httpx.ParseRetryAfter(tt.in)
			// http-date future: accept ±5s jitter vs the 60s cap because
			// ParseTime rounds to the second and time.Now has sub-second
			// precision.
			if strings.Contains(tt.name, "http-date future") {
				if got < 55*time.Second || got > 60*time.Second {
					t.Errorf("ParseRetryAfter(%q) = %v, want ~60s", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedirectPolicy(t *testing.T) {
	makeReq := func(host string) *http.Request {
		u, _ := url.Parse("https://" + host + "/some/path")
		return &http.Request{URL: u}
	}
	makeVia := func(n int) []*http.Request {
		via := make([]*http.Request, n)
		for i := range n {
			via[i] = &http.Request{}
		}
		return via
	}

	tests := []struct {
		name    string
		host    string
		viaLen  int
		wantErr bool
	}{
		{"hub.docker.com allowed", "hub.docker.com", 0, false},
		{"subdomain of docker.com allowed", "auth.docker.com", 0, false},
		{"github.com allowed", "github.com", 0, false},
		{"subdomain of github.com allowed", "api.github.com", 0, false},
		{"githubusercontent.com allowed", "raw.githubusercontent.com", 0, false},
		{"evil.com refused", "evil.com", 0, true},
		{"localhost refused", "localhost", 0, true},
		{"127.0.0.1 refused", "127.0.0.1", 0, true},
		{"too many redirects", "hub.docker.com", 5, true},
		{"4 redirects still ok", "hub.docker.com", 4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpx.RedirectPolicy(makeReq(tt.host), makeVia(tt.viaLen))
			if tt.wantErr && err == nil {
				t.Errorf("RedirectPolicy(%q, via=%d) = nil, want error", tt.host, tt.viaLen)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("RedirectPolicy(%q, via=%d) = %v, want nil", tt.host, tt.viaLen, err)
			}
		})
	}
}

func TestNewClient_wires_timeout_and_redirect_policy(t *testing.T) {
	c := httpx.NewClient(42 * time.Second)
	if c.Timeout != 42*time.Second {
		t.Errorf("Timeout = %v, want 42s", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil, want RedirectPolicy")
	}
	// Smoke: a rejected host returns an error of the "refusing redirect" form.
	u, _ := url.Parse("https://evil.com/x")
	if err := c.CheckRedirect(&http.Request{URL: u}, nil); err == nil {
		t.Error("CheckRedirect(evil.com) = nil, want error")
	}
}

func TestDrain_small_body(t *testing.T) {
	httpx.Drain(io.NopCloser(strings.NewReader("small"))) // must not panic
}

func TestDrain_eof(t *testing.T) {
	// A reader that returns exactly 100 bytes then EOF
	httpx.Drain(io.NopCloser(strings.NewReader(strings.Repeat("x", 100))))
}

func TestDrain_truncated_at_limit(t *testing.T) {
	// A 128 KB reader: Drain caps at 64 KB and returns without error.
	httpx.Drain(io.NopCloser(strings.NewReader(strings.Repeat("y", 128<<10))))
}

// --- Migrated from main_test.go in chain step 5 ---
//
// The Retry / ParseRetryAfter / RedirectPolicy / Drain test families
// are already covered above from cycle 1; the migrating main-package
// tests (TestDoGet*, TestHttpGetWithRetry_*, TestParseRetryAfter,
// TestDrainBody*, TestRegistryRedirectPolicy) duplicated that
// coverage via the main.go doGet / httpGetWithRetry / parseRetryAfter
// / drainBody / registryRedirectPolicy shims. Rather than re-importing
// those byte-for-byte duplicates, step 5 adds only the net-new
// assertions each main-package test carried:
//   - 4xx/5xx table coverage (TestRetry_non200_statusCodes) pins the
//     mutation-kill envelope TestDoGetNon200StatusCodes held.
//   - Typed StatusError tests (TestStatusError_*) exercise the
//     runtime-rs-p2 refactor that landed alongside this migration.
//   - TestRetry_returns_typed_StatusError pins the errors.As contract
//     end-to-end through the retry loop's "retries exhausted after X: %w"
//     wrap.
//   - TestRetry_4xx_returns_typed_StatusError pins the same contract on
//     the fast-fail 4xx path (no retry wrap).

// TestRetry_non200_statusCodes tables the same 4xx/5xx envelope
// TestDoGetNon200StatusCodes held in main_test.go: each status returns
// an error with "HTTP <code>" in the message and a nil body. Kills
// CONDITIONALS_NEGATION mutations that flip the non-200 guard.
func TestRetry_non200_statusCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"400 Bad Request", http.StatusBadRequest},
		{"403 Forbidden", http.StatusForbidden},
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
		{"503 Service Unavailable", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("error body"))
			}))
			defer srv.Close()

			body, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
			if err == nil {
				t.Fatalf("Retry() returned nil error for status %d", tt.status)
			}
			if body != nil {
				t.Errorf("Retry() returned non-nil body for error status %d", tt.status)
			}
			wantMsg := fmt.Sprintf("HTTP %d", tt.status)
			if !strings.Contains(err.Error(), wantMsg) {
				t.Errorf("error = %v, want containing %q", err, wantMsg)
			}
		})
	}
}

// TestStatusError_Error pins the Error() format string byte-for-byte
// against the pre-typed fmt.Errorf("HTTP %d from %s", ...) shape so
// callers that string-match on "HTTP 503" or similar (both test
// suites and any downstream log parsing) keep working.
func TestStatusError_Error(t *testing.T) {
	err := &httpx.StatusError{Code: 503, URL: "http://example.com/x"}
	want := "HTTP 503 from http://example.com/x"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestStatusError_IsRateLimited pins errors.Is(err, ErrRateLimited)
// matching only a 429 response. Other statuses must not match.
func TestStatusError_IsRateLimited(t *testing.T) {
	tests := []struct {
		name string
		code int
		want bool
	}{
		{"429", http.StatusTooManyRequests, true},
		{"500 not rate limited", http.StatusInternalServerError, false},
		{"503 not rate limited", http.StatusServiceUnavailable, false},
		{"400 not rate limited", http.StatusBadRequest, false},
		{"200 not rate limited", http.StatusOK, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &httpx.StatusError{Code: tt.code}
			if got := errors.Is(err, httpx.ErrRateLimited); got != tt.want {
				t.Errorf("errors.Is(%d, ErrRateLimited) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestStatusError_IsServerError pins errors.Is(err, ErrServerError)
// matching only 5xx responses (500-599 inclusive). 429 and 4xx must
// not match.
func TestStatusError_IsServerError(t *testing.T) {
	tests := []struct {
		name string
		code int
		want bool
	}{
		{"500", http.StatusInternalServerError, true},
		{"502", http.StatusBadGateway, true},
		{"503", http.StatusServiceUnavailable, true},
		{"504", http.StatusGatewayTimeout, true},
		{"599", 599, true},
		{"429 not server error", http.StatusTooManyRequests, false},
		{"400 not server error", http.StatusBadRequest, false},
		{"404 not server error", http.StatusNotFound, false},
		{"200 not server error", http.StatusOK, false},
		{"600 out of 5xx band", 600, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &httpx.StatusError{Code: tt.code}
			if got := errors.Is(err, httpx.ErrServerError); got != tt.want {
				t.Errorf("errors.Is(%d, ErrServerError) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestRetry_returns_typed_StatusError_on_exhaustion verifies that a
// persistent 503 surface exhausts retries AND preserves the typed
// *httpx.StatusError through the "retries exhausted after X: %w" wrap.
// errors.As must unwrap to the StatusError; errors.Is(err,
// ErrServerError) must hold. This is the end-to-end contract the ghcr
// classify-site rewrite depends on.
func TestRetry_returns_typed_StatusError_on_exhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	if err == nil {
		t.Fatalf("Retry() = nil, want error")
	}
	var target *httpx.StatusError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(err, *StatusError) = false, want true; err = %v", err)
	}
	if target.Code != http.StatusServiceUnavailable {
		t.Errorf("StatusError.Code = %d, want 503", target.Code)
	}
	if !errors.Is(err, httpx.ErrServerError) {
		t.Errorf("errors.Is(err, ErrServerError) = false, want true; err = %v", err)
	}
	if errors.Is(err, httpx.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = true, want false for 503 err = %v", err)
	}
}

// TestRetry_returns_typed_StatusError_rate_limited verifies that a
// persistent 429 surface exhausts retries AND the typed *StatusError
// makes errors.Is(err, ErrRateLimited) true, which is the contract
// ghcr.Client.Collect's classify site depends on post-refactor.
func TestRetry_returns_typed_StatusError_rate_limited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	if err == nil {
		t.Fatalf("Retry() = nil, want error")
	}
	if !errors.Is(err, httpx.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; err = %v", err)
	}
	var target *httpx.StatusError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(err, *StatusError) = false, want true; err = %v", err)
	}
	if target.Code != http.StatusTooManyRequests {
		t.Errorf("StatusError.Code = %d, want 429", target.Code)
	}
}

// TestRetry_4xx_returns_typed_StatusError verifies the fast-fail 4xx
// path (non-429, non-5xx) returns the typed StatusError directly, not
// wrapped in "retries exhausted". errors.As still unwraps; neither
// ErrRateLimited nor ErrServerError match.
func TestRetry_4xx_returns_typed_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := httpx.Retry(t.Context(), srv.Client(), srv.URL, shortOpts())
	if err == nil {
		t.Fatalf("Retry() = nil, want error")
	}
	if strings.Contains(err.Error(), "retries exhausted") {
		t.Errorf("error = %v, want fast-fail (no 'retries exhausted' wrap)", err)
	}
	var target *httpx.StatusError
	if !errors.As(err, &target) {
		t.Fatalf("errors.As(err, *StatusError) = false, want true; err = %v", err)
	}
	if target.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want 404", target.Code)
	}
	if errors.Is(err, httpx.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = true, want false for 404")
	}
	if errors.Is(err, httpx.ErrServerError) {
		t.Errorf("errors.Is(err, ErrServerError) = true, want false for 404")
	}
}
