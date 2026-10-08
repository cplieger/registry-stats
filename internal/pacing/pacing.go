// Package pacing spaces the requests one registry reader sends.
package pacing

import (
	"net/http"
	"sync"
	"time"

	"github.com/cplieger/httpx/v5"
)

// Client returns a copy of c whose every request, retries and redirect hops
// included, starts at least gap() after the start of the one before it. The
// first request starts at once. Each call gets its own schedule, so two
// readers wrapping one client keep separate budgets. The wait counts against
// c.Timeout, and a request cancelled while it waits fails with its context's
// error.
func Client(c *http.Client, gap func() time.Duration) *http.Client {
	next := c.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	paced := *c
	paced.Transport = &transport{next: next, gap: gap}
	return &paced
}

type transport struct {
	next http.RoundTripper
	gap  func() time.Duration
	last time.Time
	mu   sync.Mutex
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := httpx.SleepCtx(req.Context(), time.Until(t.reserve())); err != nil {
		// http.RoundTripper requires closing the body on every path.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.next.RoundTrip(req)
}

// reserve claims the next start time, so concurrent requests queue behind it.
func (t *transport) reserve() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := time.Now()
	if !t.last.IsZero() {
		if due := t.last.Add(t.gap()); due.After(start) {
			start = due
		}
	}
	t.last = start
	return start
}
