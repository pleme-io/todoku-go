package todoku

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file absorbs the request-deduplication + short-lived result-cache
// pattern (the akeyless-funnel shape, generalised): two cures for the same
// disease — a burst of identical outbound calls hammering a backend. Both are
// expressed as transport middleware (BOREALIS Law 2: a decorator over the
// universal http.RoundTripper interface, never a bespoke client variant), so
// they compose with retry/backoff/breaker/budget and with ANY *http.Client.
//
//   - Single-flight collapses concurrent IN-FLIGHT identical requests onto one
//     real round-trip whose response is shared (the funnel dedup core).
//   - Result-cache memoises a completed response for a TTL window so repeated
//     calls within the window skip the network entirely.
//
// Neither names a backend; the keying is generic (method + URL + a canonical
// projection of the configured Vary headers). A public primitive never imports
// or names akeyless (WORLDS-SEPARATE).

// Middleware is a transport decorator in the canonical func(RoundTripper)
// RoundTripper shape (BOREALIS Law 2). It wraps an inner [http.RoundTripper]
// and returns a new one, so middleware compose by ordinary function
// composition and drop into any *http.Client.
type Middleware func(http.RoundTripper) http.RoundTripper

// requestKey computes the dedup/cache identity of a request: METHOD + " " +
// URL, optionally suffixed by a canonical projection of the headers named in
// vary (sorted, so header order never changes the key). It is intentionally
// independent of the request body — single-flight and result-cache target
// idempotent reads (GET/HEAD by default), where the body is empty.
func requestKey(req *http.Request, vary []string) string {
	var b strings.Builder
	b.WriteString(req.Method)
	b.WriteByte(' ')
	b.WriteString(req.URL.String())
	if len(vary) > 0 {
		names := make([]string, len(vary))
		copy(names, vary)
		sort.Strings(names)
		for _, name := range names {
			b.WriteByte('\n')
			b.WriteString(http.CanonicalHeaderKey(name))
			b.WriteByte('=')
			b.WriteString(strings.Join(req.Header.Values(name), ","))
		}
	}
	return b.String()
}

// isCacheableMethod reports whether a method is a safe read to dedup/cache by
// default (GET/HEAD). Other methods pass through the middleware untouched so a
// POST is never silently collapsed or memoised.
func isCacheableMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// bufferedResponse is a fully-read response captured so it can be replayed to
// many callers (single-flight sharers, cache hits) without any caller seeing
// another's consumed Body. cloneResponse mints a fresh *http.Response with a
// new Body reader from the captured bytes on each replay.
type bufferedResponse struct {
	status        int
	header        http.Header
	body          []byte
	proto         string
	protoMajor    int
	protoMinor    int
	contentLength int64
}

// captureResponse drains resp.Body into a [bufferedResponse] and closes the
// original body. The original resp is consumed and must not be used afterwards.
func captureResponse(resp *http.Response) (*bufferedResponse, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &bufferedResponse{
		status:        resp.StatusCode,
		header:        resp.Header.Clone(),
		body:          body,
		proto:         resp.Proto,
		protoMajor:    resp.ProtoMajor,
		protoMinor:    resp.ProtoMinor,
		contentLength: int64(len(body)),
	}, nil
}

// clone mints a fresh, independently-consumable *http.Response for req from the
// captured bytes, so every sharer / cache hit gets its own Body.
func (b *bufferedResponse) clone(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode:    b.status,
		Status:        http.StatusText(b.status),
		Header:        b.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(b.body)),
		Proto:         b.proto,
		ProtoMajor:    b.protoMajor,
		ProtoMinor:    b.protoMinor,
		ContentLength: b.contentLength,
		Request:       req,
	}
}

// --- single-flight ---

// SingleFlight collapses concurrent IN-FLIGHT identical requests onto a single
// underlying round-trip whose buffered response is shared among all waiters
// (the request-deduplication / funnel pattern). It is an [http.RoundTripper]
// middleware (Law 2): the first caller for a key performs the real round-trip;
// concurrent callers for the same key block on it and receive an independent
// clone of the same response. Once that round-trip completes the key is
// released — SingleFlight does NOT cache across time (compose [ResultCache] for
// that). It is safe for concurrent use.
//
// Only methods reported cacheable by default (GET/HEAD) are deduped; other
// methods pass straight through. Errors are NOT shared — a failed flight
// surfaces only to the caller that performed it, and a retry by another waiter
// re-dials, so a transient failure never poisons the group.
type SingleFlight struct {
	vary []string

	mu     sync.Mutex
	flying map[string]*flight
}

// flight is one in-progress shared round-trip. Waiters block on done, then read
// resp/err. err is surfaced to the owner only (see RoundTrip), not the sharers.
type flight struct {
	done chan struct{}
	resp *bufferedResponse
	err  error
}

// SingleFlightOption configures a [SingleFlight] in the functional-options
// style (BOREALIS §3.5).
type SingleFlightOption func(*SingleFlight)

// SingleFlightVary adds the named request headers to the dedup key, so requests
// that differ only in those headers are NOT collapsed (e.g. a per-tenant
// Authorization). Header name matching is case-insensitive.
func SingleFlightVary(headers ...string) SingleFlightOption {
	return func(s *SingleFlight) { s.vary = append(s.vary, headers...) }
}

// NewSingleFlight constructs a [SingleFlight] decorator factory. The returned
// value is itself a [Middleware]-producer via [SingleFlight.Wrap]; or pass it
// to [WithSingleFlight] to install it on a [Client].
func NewSingleFlight(opts ...SingleFlightOption) *SingleFlight {
	s := &SingleFlight{flying: make(map[string]*flight)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Wrap returns a [Middleware] that decorates next with this single-flight
// group. The same *SingleFlight may wrap multiple transports; the group is
// keyed by request identity, not by transport.
func (s *SingleFlight) Wrap(next http.RoundTripper) http.RoundTripper {
	return &singleFlightRT{group: s, next: next}
}

type singleFlightRT struct {
	group *SingleFlight
	next  http.RoundTripper
}

// RoundTrip implements [http.RoundTripper], collapsing concurrent identical
// requests onto one underlying round-trip.
func (rt *singleFlightRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCacheableMethod(req.Method) {
		return rt.next.RoundTrip(req)
	}
	key := requestKey(req, rt.group.vary)

	rt.group.mu.Lock()
	if f, ok := rt.group.flying[key]; ok {
		// A flight for this key is already in progress: become a sharer.
		rt.group.mu.Unlock()
		<-f.done
		if f.err != nil {
			// The owner's transport error is not shared (it may be a
			// per-caller condition); re-dial so this caller gets its own
			// outcome rather than inheriting a stranger's failure.
			return rt.next.RoundTrip(req)
		}
		return f.resp.clone(req), nil
	}
	// We are the owner: register the flight and perform the real round-trip.
	f := &flight{done: make(chan struct{})}
	rt.group.flying[key] = f
	rt.group.mu.Unlock()

	resp, err := rt.next.RoundTrip(req)
	if err == nil {
		buf, capErr := captureResponse(resp)
		if capErr != nil {
			err = capErr
		} else {
			f.resp = buf
		}
	}
	f.err = err

	// Release the flight before signalling so a sharer that wakes immediately
	// and re-dials does not collide with this completed entry.
	rt.group.mu.Lock()
	delete(rt.group.flying, key)
	rt.group.mu.Unlock()
	close(f.done)

	if err != nil {
		return nil, err
	}
	return f.resp.clone(req), nil
}

// --- result cache ---

// ResultCache memoises a completed response for a TTL window so repeated
// identical reads within the window skip the network entirely (the second half
// of the funnel pattern: dedup over time, not just concurrency). It is an
// [http.RoundTripper] middleware (Law 2) and is safe for concurrent use.
//
// Only cacheable methods (GET/HEAD by default) and only successful responses
// (2xx) are stored; everything else passes through and is never cached. Entries
// expire lazily on read (a stale entry is evicted and re-fetched), so an idle
// cache never serves stale data and needs no background sweeper.
type ResultCache struct {
	ttl  time.Duration
	vary []string
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	resp    *bufferedResponse
	expires time.Time
}

// ResultCacheOption configures a [ResultCache] (functional options, §3.5).
type ResultCacheOption func(*ResultCache)

// ResultCacheVary adds the named request headers to the cache key, so responses
// that depend on those headers (e.g. Accept, a tenant header) are cached
// per-distinct-value rather than conflated. Case-insensitive.
func ResultCacheVary(headers ...string) ResultCacheOption {
	return func(rc *ResultCache) { rc.vary = append(rc.vary, headers...) }
}

// resultCacheClock injects a clock for deterministic TTL tests. Unexported: the
// public surface keeps the wall clock.
func resultCacheClock(now func() time.Time) ResultCacheOption {
	return func(rc *ResultCache) {
		if now != nil {
			rc.now = now
		}
	}
}

// NewResultCache constructs a [ResultCache] with the given TTL. A non-positive
// TTL yields a pass-through cache (nothing is ever stored), which keeps the
// middleware safe to install unconditionally from config. Install it on a
// [Client] via [WithResultCache] or wrap a transport with [ResultCache.Wrap].
func NewResultCache(ttl time.Duration, opts ...ResultCacheOption) *ResultCache {
	rc := &ResultCache{ttl: ttl, now: time.Now, entries: make(map[string]cacheEntry)}
	for _, opt := range opts {
		opt(rc)
	}
	return rc
}

// Wrap returns a [Middleware] decorating next with this result cache.
func (rc *ResultCache) Wrap(next http.RoundTripper) http.RoundTripper {
	return &resultCacheRT{cache: rc, next: next}
}

type resultCacheRT struct {
	cache *ResultCache
	next  http.RoundTripper
}

// RoundTrip implements [http.RoundTripper], serving a fresh cached response
// when one is live, otherwise round-tripping and storing a 2xx result.
func (rt *resultCacheRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rc := rt.cache
	if rc.ttl <= 0 || !isCacheableMethod(req.Method) {
		return rt.next.RoundTrip(req)
	}
	key := requestKey(req, rc.vary)
	now := rc.now()

	rc.mu.Lock()
	if e, ok := rc.entries[key]; ok {
		if now.Before(e.expires) {
			rc.mu.Unlock()
			return e.resp.clone(req), nil
		}
		delete(rc.entries, key) // stale: evict and re-fetch.
	}
	rc.mu.Unlock()

	resp, err := rt.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Only successful reads are cacheable; hand non-2xx straight back.
		return resp, nil
	}
	buf, capErr := captureResponse(resp)
	if capErr != nil {
		return nil, capErr
	}
	rc.mu.Lock()
	rc.entries[key] = cacheEntry{resp: buf, expires: now.Add(rc.ttl)}
	rc.mu.Unlock()
	return buf.clone(req), nil
}

// Len reports the number of (possibly-expired-but-not-yet-evicted) cache
// entries, for diagnostics and tests.
func (rc *ResultCache) Len() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.entries)
}

// --- Client wiring ---

// WithSingleFlight installs a request-deduplication [Middleware] on the
// [Client]'s transport (BOREALIS Law 2 composition): concurrent identical
// GET/HEAD requests are collapsed onto one underlying round-trip. Pass an
// existing [SingleFlight] to share a dedup group across clients, or nil to
// create a fresh default group. The middleware decorates whatever transport is
// already configured, so it composes with [WithTransport] and the retry stack.
func WithSingleFlight(sf *SingleFlight) Option {
	if sf == nil {
		sf = NewSingleFlight()
	}
	return withTransportMiddleware(sf.Wrap)
}

// WithResultCache installs a TTL result-cache [Middleware] on the [Client]'s
// transport: successful GET/HEAD responses are memoised for ttl so repeated
// identical reads within the window skip the network. A non-positive ttl
// installs a pass-through (caches nothing), so this is safe to wire
// unconditionally from config. Extra [ResultCacheOption]s (e.g.
// [ResultCacheVary]) tune the cache key.
func WithResultCache(ttl time.Duration, opts ...ResultCacheOption) Option {
	rc := NewResultCache(ttl, opts...)
	return withTransportMiddleware(rc.Wrap)
}

// WithMiddleware installs an arbitrary transport [Middleware], the open seam
// for any future func(RoundTripper) RoundTripper decorator (caching, tracing,
// rate-limiting) without a new Option per concern. Middleware are applied in
// the order the options are passed: the LAST option added is the OUTERMOST
// wrapper (closest to the caller), so a single-flight added after a result
// cache sees cache hits first.
func WithMiddleware(mw Middleware) Option {
	if mw == nil {
		return func(*Client) {}
	}
	return withTransportMiddleware(mw)
}

// withTransportMiddleware is the shared installer: it wraps the client's
// current transport (defaulting to http.DefaultTransport) with mw, allocating
// an *http.Client if needed and preserving any captured timeout. Keeping this
// in one place means every middleware-installing Option composes identically.
func withTransportMiddleware(mw Middleware) Option {
	return func(c *Client) {
		if c.httpClient == nil {
			c.httpClient = &http.Client{Timeout: c.timeout}
		}
		base := c.httpClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		c.httpClient.Transport = mw(base)
	}
}
