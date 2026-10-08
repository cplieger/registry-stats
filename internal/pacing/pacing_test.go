package pacing

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Errorf("Setup: build request: %v", err)
		return
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Errorf("GET %s error = %v", url, err)
		return
	}
	_ = resp.Body.Close()
}

func TestClient_SpacesConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var at []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		}))
		c := Client(srv.Client(), func() time.Duration { return time.Second })
		start := time.Now()

		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { get(t, c, "https://example.test/") })
		}
		wg.Wait()

		slices.SortFunc(at, time.Time.Compare)
		if len(at) != 4 || !at[0].Equal(start) {
			t.Fatalf("requests started at %v, want 4 with the first at %v", at, start)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < time.Second {
				t.Errorf("request %d started %v after request %d, want at least 1s", i, gap, i-1)
			}
		}
	})
}

func TestClient_EachWrapKeepsItsOwnSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		shared := srv.Client()
		a := Client(shared, func() time.Duration { return time.Hour })
		b := Client(shared, func() time.Duration { return time.Hour })
		start := time.Now()

		get(t, a, "https://example.test/a")
		get(t, b, "https://example.test/b")

		if waited := time.Since(start); waited != 0 {
			t.Errorf("the first request of a second wrap waited %v, want 0", waited)
		}
		if shared.Transport == a.Transport {
			t.Error("Client changed the transport of the client it wraps")
		}
	})
}
