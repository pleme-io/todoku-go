package todoku

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestKey(t *testing.T) {
	mk := func(method, url string, hdr map[string]string) *http.Request {
		req, _ := http.NewRequest(method, url, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return req
	}
	tests := []struct {
		name string
		a, b *http.Request
		vary []string
		same bool
	}{
		{
			name: "same method+url collapse",
			a:    mk("GET", "https://x/a", nil),
			b:    mk("GET", "https://x/a", nil),
			same: true,
		},
		{
			name: "different url distinct",
			a:    mk("GET", "https://x/a", nil),
			b:    mk("GET", "https://x/b", nil),
			same: false,
		},
		{
			name: "header ignored without vary",
			a:    mk("GET", "https://x/a", map[string]string{"Authorization": "t1"}),
			b:    mk("GET", "https://x/a", map[string]string{"Authorization": "t2"}),
			same: true,
		},
		{
			name: "header splits with vary",
			a:    mk("GET", "https://x/a", map[string]string{"Authorization": "t1"}),
			b:    mk("GET", "https://x/a", map[string]string{"Authorization": "t2"}),
			vary: []string{"Authorization"},
			same: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ka := requestKey(tc.a, tc.vary)
			kb := requestKey(tc.b, tc.vary)
			if (ka == kb) != tc.same {
				t.Errorf("keys same=%v, want %v (a=%q b=%q)", ka == kb, tc.same, ka, kb)
			}
		})
	}
}

// Single-flight collapses concurrent identical GETs onto one round-trip; all
// callers see the same body, and the backend is hit exactly once.
func TestSingleFlightCollapsesConcurrent(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold the response open so all callers are in-flight together
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("shared-body"))
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithSingleFlight(nil))

	const n = 8
	var wg sync.WaitGroup
	bodies := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := c.Get(context.Background(), "/x")
			if err != nil {
				errs[i] = err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodies[i] = string(b)
		}(i)
	}
	// Give the goroutines a moment to all enter the flight, then release.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("backend hits = %d, want 1 (collapsed)", got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Errorf("caller %d err: %v", i, errs[i])
		}
		if bodies[i] != "shared-body" {
			t.Errorf("caller %d body = %q, want shared-body", i, bodies[i])
		}
	}
}

// Sequential (non-concurrent) requests are NOT collapsed by single-flight — the
// flight is released once complete, so each subsequent call re-dials.
func TestSingleFlightSequentialNotCollapsed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithSingleFlight(nil))
	for range 3 {
		resp, err := c.Get(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("backend hits = %d, want 3 (no cross-time dedup)", got)
	}
}

// Non-cacheable methods (POST) pass through single-flight untouched.
func TestSingleFlightPassesThroughPost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithSingleFlight(nil))
	for range 2 {
		resp, err := c.Post(context.Background(), "/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("POST hits = %d, want 2 (not deduped)", got)
	}
}

// Result-cache memoises a 2xx GET for the TTL window: a second identical call
// within TTL skips the network; after expiry it re-fetches.
func TestResultCacheTTL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cached"))
	}))
	defer srv.Close()

	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rc := NewResultCache(time.Minute, resultCacheClock(clock.Now))
	c, _ := New(WithBaseURL(srv.URL), WithMiddleware(rc.Wrap))

	get := func() string {
		resp, err := c.Get(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	if got := get(); got != "cached" {
		t.Fatalf("body = %q, want cached", got)
	}
	// Within TTL: served from cache, no new hit.
	clock.advance(30 * time.Second)
	if got := get(); got != "cached" {
		t.Fatalf("cached body = %q", got)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("hits within TTL = %d, want 1", h)
	}
	// After TTL: re-fetch.
	clock.advance(2 * time.Minute)
	_ = get()
	if h := hits.Load(); h != 2 {
		t.Errorf("hits after expiry = %d, want 2", h)
	}
}

// A non-2xx response is never cached.
func TestResultCacheSkipsNon2xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewResultCache(time.Minute)
	// Drop strict retries so the 404 surfaces immediately and only once.
	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(0)), WithMiddleware(rc.Wrap))
	for range 2 {
		_, _ = c.Get(context.Background(), "/missing")
	}
	if h := hits.Load(); h != 2 {
		t.Errorf("404 hits = %d, want 2 (not cached)", h)
	}
	if rc.Len() != 0 {
		t.Errorf("cache len = %d, want 0", rc.Len())
	}
}

// A non-positive TTL installs a pass-through cache that stores nothing.
func TestResultCacheZeroTTLPassThrough(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithResultCache(0))
	for range 3 {
		resp, _ := c.Get(context.Background(), "/x")
		if resp != nil {
			resp.Body.Close()
		}
	}
	if h := hits.Load(); h != 3 {
		t.Errorf("zero-TTL hits = %d, want 3 (pass-through)", h)
	}
}

// Single-flight + result-cache compose: a burst of concurrent calls collapses
// to one backend hit AND populates the cache for subsequent calls.
func TestSingleFlightAndResultCacheCompose(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v"))
	}))
	defer srv.Close()

	// Inner cache, outer single-flight (last option = outermost).
	c, _ := New(WithBaseURL(srv.URL),
		WithResultCache(time.Minute),
		WithSingleFlight(nil),
	)

	const n = 5
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(context.Background(), "/x")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	// One real hit from the collapsed burst.
	if h := hits.Load(); h != 1 {
		t.Fatalf("burst hits = %d, want 1", h)
	}
	// A later sequential call is served from the cache (still 1 hit).
	resp, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := hits.Load(); h != 1 {
		t.Errorf("post-burst hits = %d, want 1 (cache served it)", h)
	}
}

// fakeClock is a deterministic, advanceable clock for TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
