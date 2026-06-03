package todoku

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a shared HTTP client with pluggable authentication and
// exponential-backoff retry — the Go analog of the Rust `todoku::HttpClient`.
// Build one with [New] plus functional [Option]s and share it across goroutines
// (it is safe for concurrent use). All retry behaviour flows through the single
// [RetryWithBackoff] primitive; there is no second backoff implementation.
type Client struct {
	baseURL    string
	auth       Auth
	retry      RetryConfig
	httpClient *http.Client
	userAgent  string
	// timeout is captured by WithTimeout and applied at construction time only
	// when no explicit *http.Client is supplied. Storing it on the struct makes
	// WithTimeout / WithHTTPClient order-independent.
	timeout time.Duration

	// checkRetry is the per-attempt retry decision. nil → DefaultCheckRetry
	// over c.retry.RetryStatuses, installed lazily in New.
	checkRetry CheckRetry
	// honorRetryAfter, when true (the default), overrides the computed backoff
	// with a server-supplied Retry-After delay on the failing attempt.
	honorRetryAfter bool
	// now is an injectable clock for Retry-After date math (tests). nil →
	// time.Now.
	now func() time.Time

	// keyFunc, when non-nil, injects a stable Idempotency-Key header reused
	// across retries of one logical request.
	keyFunc KeyFunc
	// allowNonIdempotentRetry permits retrying POST/PATCH even without an
	// idempotency key. Independent of strictIdempotency.
	allowNonIdempotentRetry bool
	// strictIdempotency, when true, opts INTO the safety gate: non-idempotent
	// verbs (POST/PATCH) are then retried only when made safe by an idempotency
	// key or an explicit allowNonIdempotentRetry. It defaults to FALSE to
	// preserve the library's historical behaviour (every method was retried),
	// so the gate is additive and back-compatible.
	strictIdempotency bool

	// breaker is the opt-in circuit breaker. nil → no breaker.
	breaker *CircuitBreaker
	// budget is the opt-in retry budget (token bucket). nil → unlimited
	// retries. Wire one via WithRetryBudget (todoku/budget sub-package).
	budget RetryBudget
}

// RetryBudget is the storm-control admission gate consulted before each RETRY
// (not the first attempt). It is an interface so the token-bucket implementation
// can live in the todoku/budget sub-package behind the golang.org/x/time/rate
// dependency (BOREALIS Law 6) without the core importing it. AllowRetry reports
// whether a retry token is available; an exhausted budget stops the loop.
type RetryBudget interface {
	// AllowRetry reports whether the budget permits spending a token on a
	// retry right now. It must be safe for concurrent use.
	AllowRetry() bool
}

// ErrBudgetExhausted is returned when the configured [RetryBudget] denies a
// retry token, so the [Client] stopped retrying to shed load (the AWS
// Builders' Library "retry budget as primary storm-control" pattern). It
// carries Retryable()=false.
var ErrBudgetExhausted = &budgetExhaustedError{}

type budgetExhaustedError struct{}

func (*budgetExhaustedError) Error() string { return "todoku: retry budget exhausted" }

// Retryable reports false — a budget rejection is the load-shedding decision.
func (*budgetExhaustedError) Retryable() bool { return false }

// Option configures a [Client] in the functional-options style. Pass any number
// of options to [New]; later options win on conflict.
type Option func(*Client)

// WithBaseURL sets the base URL that relative request paths are joined against.
// With a base URL set, Do("GET", "/widgets", …) targets "<base>/widgets"; an
// empty base URL leaves the path used verbatim (treat it as a full URL).
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = url }
}

// WithAuth sets the authentication strategy applied to every request. The
// default is [NoAuth].
func WithAuth(a Auth) Option {
	return func(c *Client) {
		if a != nil {
			c.auth = a
		}
	}
}

// WithTimeout sets the per-request timeout on the underlying *http.Client. It is
// ignored if [WithHTTPClient] supplies a client (configure the timeout there).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithRetry sets the retry configuration. The default is [DefaultRetry].
func WithRetry(cfg RetryConfig) Option {
	return func(c *Client) { c.retry = cfg }
}

// WithHTTPClient supplies a custom *http.Client (for custom transports, proxies,
// or TLS config). When set it takes precedence over [WithTimeout].
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithUserAgent overrides the default User-Agent header sent on every request.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithCheckRetry installs a custom [CheckRetry] policy, replacing the default
// status-code decision. The hook runs per attempt and may convert any condition
// into a hard stop by returning a non-nil error.
func WithCheckRetry(cr CheckRetry) Option {
	return func(c *Client) {
		if cr != nil {
			c.checkRetry = cr
		}
	}
}

// WithRetryAfter toggles honouring the server's Retry-After header (default on).
// When enabled, a 429/503 (or any failing response) carrying Retry-After
// overrides the computed exponential backoff for that wait.
func WithRetryAfter(honor bool) Option {
	return func(c *Client) { c.honorRetryAfter = honor }
}

// WithIdempotencyKeys injects a stable Idempotency-Key header (from keyFunc,
// e.g. [DefaultKeyFunc]) reused across every retry of one logical request,
// making retries of non-idempotent verbs (POST/PATCH) safe per the IETF
// Idempotency-Key draft. Supplying a key func also implicitly permits retrying
// those verbs.
func WithIdempotencyKeys(keyFunc KeyFunc) Option {
	return func(c *Client) {
		if keyFunc != nil {
			c.keyFunc = keyFunc
		}
	}
}

// WithAllowNonIdempotentRetry permits retrying non-idempotent verbs (POST/PATCH)
// even without an idempotency key. Only meaningful together with
// [WithStrictIdempotency]; with the default (non-strict) gate every method is
// already retried.
func WithAllowNonIdempotentRetry(allow bool) Option {
	return func(c *Client) { c.allowNonIdempotentRetry = allow }
}

// WithStrictIdempotency opts into the idempotency safety gate: when enabled,
// non-idempotent verbs (POST/PATCH) are retried only when an [WithIdempotencyKeys]
// key makes them safe or [WithAllowNonIdempotentRetry] explicitly permits it.
//
// It is OFF by default to preserve the library's historical behaviour (all
// methods retried); enable it for at-most-once semantics on un-deduped servers.
func WithStrictIdempotency(strict bool) Option {
	return func(c *Client) { c.strictIdempotency = strict }
}

// WithCircuitBreaker installs an opt-in [CircuitBreaker]. Off by default
// (BOREALIS: the breaker is opt-in; the retry budget is the default
// storm-control). A nil breaker is ignored.
func WithCircuitBreaker(cb *CircuitBreaker) Option {
	return func(c *Client) {
		if cb != nil {
			c.breaker = cb
		}
	}
}

// WithRetryBudget installs a [RetryBudget] storm-control gate consulted before
// each retry. The token-bucket implementation lives in the todoku/budget
// sub-package (gated behind golang.org/x/time/rate per Law 6). A nil budget is
// ignored (unlimited retries).
func WithRetryBudget(b RetryBudget) Option {
	return func(c *Client) {
		if b != nil {
			c.budget = b
		}
	}
}

// WithTransport installs the supplied [http.RoundTripper] as the underlying
// client's transport, composing storm-control transport tuning (per-host pool
// caps, ResponseHeaderTimeout) into any client. It allocates a fresh
// *http.Client if none was set, preserving any captured timeout. This is the
// Law-2 composition seam: build a transport with [TunedTransport] (or the
// todoku/h2 sub-package for h2 ping knobs) and drop it in here.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) {
		if rt == nil {
			return
		}
		if c.httpClient == nil {
			c.httpClient = &http.Client{Timeout: c.timeout}
		}
		c.httpClient.Transport = rt
	}
}

// New constructs a [Client] from the given options. It never fails: with no
// options it returns a usable client with [NoAuth], [DefaultRetry], a 30s
// timeout, and a default User-Agent.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		auth:            NoAuth(),
		retry:           DefaultRetry(),
		timeout:         30 * time.Second,
		userAgent:       fmt.Sprintf("pleme-io/todoku-go %s", Version),
		honorRetryAfter: true,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: c.timeout}
	}
	if c.checkRetry == nil {
		c.checkRetry = DefaultCheckRetry(c.retry.RetryStatuses)
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// resolveURL joins a request path against the base URL, collapsing the
// boundary slash so neither a trailing slash on the base nor a leading slash on
// the path produces a double slash. With no base URL the path is returned as-is
// (treat it as an absolute URL).
func (c *Client) resolveURL(path string) string {
	if c.baseURL == "" {
		return path
	}
	base := strings.TrimRight(c.baseURL, "/")
	p := strings.TrimLeft(path, "/")
	if p == "" {
		return base + "/"
	}
	return base + "/" + p
}

// Do executes an HTTP request against path (joined to the base URL), applying
// authentication and the retry policy, and returns the raw *http.Response. The
// caller owns the response body and must Close it.
//
// Retries are governed entirely by [RetryWithBackoff]: a transport (network)
// error or a response whose status is in RetryConfig.RetryStatuses is retried
// after backoff; a non-retryable status surfaces as an [*HTTPError]; a context
// cancellation aborts immediately. body, if non-nil, is buffered so it can be
// replayed on each attempt.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.do(ctx, method, path, body, "")
}

// do is the shared request engine. contentType, when non-empty, sets the
// Content-Type header (used by [PostJSON]).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	url := c.resolveURL(path)

	// Buffer the body once so every retry attempt can replay it.
	var bodyBytes []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("todoku: read request body: %w", err)
		}
		bodyBytes = b
	}

	// Resolve a stable idempotency key once, so every retry of this logical
	// request carries the same Idempotency-Key header (Stripe / IETF semantics).
	idemKey := ""
	if c.keyFunc != nil {
		idemKey = c.keyFunc(method, url)
	}
	retryAllowedForMethod := c.retryAllowedForMethod(method, idemKey)

	attempt := func(ctx context.Context) (*http.Response, error) {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			// A malformed method/URL is permanent — do not retry.
			return nil, &NonRetryableError{Err: fmt.Errorf("todoku: build request: %w", err)}
		}
		req.Header.Set("User-Agent", c.userAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		c.auth.Apply(req)

		// Circuit breaker (opt-in) gates the attempt itself: an open breaker
		// rejects without dialing. ErrBreakerOpen carries Retryable()=false so
		// the retry loop stops immediately.
		var breakerDone func(bool)
		if c.breaker != nil {
			done, berr := c.breaker.Allow()
			if berr != nil {
				return nil, &NonRetryableError{Err: berr}
			}
			breakerDone = done
		}

		resp, err := c.httpClient.Do(req)

		// Run the CheckRetry policy with the raw (resp, err) pair. A non-nil
		// policy error is a hard stop.
		retry, policyErr := c.checkRetry(ctx, resp, err)
		// Report the outcome to the breaker: a retryable failure (transport
		// error or retryable status) counts as a failure; success/permanent
		// 4xx count as a success (the server is reachable and answering).
		if breakerDone != nil {
			breakerDone(!(retry && (err != nil || (resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300)))))
		}
		if policyErr != nil {
			if resp != nil {
				drainBody(resp)
			}
			return nil, &NonRetryableError{Err: policyErr}
		}

		if err != nil {
			if !retry {
				return nil, &NonRetryableError{Err: err}
			}
			return nil, err // transient transport error
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		// Non-2xx. Read + close the body so the connection can be reused, then
		// classify per the CheckRetry decision.
		bodyStr := drainBody(resp)
		if retry {
			se := &statusError{status: resp.StatusCode, body: bodyStr}
			if d, ok := RetryAfter(resp, c.now); ok {
				se.retryAfter, se.retryAfterSet = d, true
			}
			return nil, se
		}
		return nil, &NonRetryableError{Err: &HTTPError{Status: resp.StatusCode, Body: bodyStr}}
	}

	resp, err := RetryWithBackoffClass(ctx, c.callRetryConfig(retryAllowedForMethod), clientRetryable, attempt)
	if err != nil {
		return nil, unwrapClientError(err)
	}
	return resp, nil
}

// retryAllowedForMethod reports whether retries are permitted for a request of
// the given method with the resolved idempotency key. With the default
// (non-strict) gate every method is retryable, preserving historical behaviour.
// Under [WithStrictIdempotency], a non-idempotent verb is retryable only when a
// key makes it safe or it is explicitly allowed.
func (c *Client) retryAllowedForMethod(method, idemKey string) bool {
	if !c.strictIdempotency {
		return true
	}
	return IsIdempotentMethod(method) || idemKey != "" || c.allowNonIdempotentRetry
}

// callRetryConfig clones the client's retry config and installs the per-call
// hooks: Retry-After delay override, the budget/breaker storm-control gate, and
// the non-idempotent-method retry guard. The single backoff loop in
// [RetryWithBackoff] still owns sleeping — these hooks only override the wait
// duration and admit/deny retries.
func (c *Client) callRetryConfig(retryAllowedForMethod bool) RetryConfig {
	cfg := c.retry
	// Non-idempotent verb with no key and no opt-in → run exactly once. The
	// last attempt's error (HTTPError / transport error) surfaces normally.
	if !retryAllowedForMethod {
		cfg.MaxRetries = 0
	}
	cfg.DelayOverride = func(_ int, err error) (time.Duration, bool) {
		if !c.honorRetryAfter {
			return 0, false
		}
		var se *statusError
		if errors.As(err, &se) && se.retryAfterSet {
			return se.retryAfter, true
		}
		return 0, false
	}
	if c.budget != nil {
		cfg.BeforeRetry = func(_ int, _ error) error {
			// Retry budget storm-control: an exhausted budget stops the loop
			// and surfaces ErrBudgetExhausted.
			if !c.budget.AllowRetry() {
				return ErrBudgetExhausted
			}
			return nil
		}
	}
	return cfg
}

// clientRetryable is the retry classifier for [Client.Do]. Only the synthetic
// [*statusError] (a retryable HTTP status) and bare transport errors are
// retried; anything already wrapped in [*NonRetryableError] (context errors,
// non-retryable HTTP statuses, request-build failures) stops the loop.
func clientRetryable(err error) bool {
	var nre *NonRetryableError
	if errors.As(err, &nre) {
		return false
	}
	return true
}

// unwrapClientError translates the retry loop's wrapper errors back into the
// public surface: a [*NonRetryableError] yields its cause (e.g. [*HTTPError] or
// a context error); an [*ExhaustedError] whose final failure was a retryable
// status yields the corresponding [*HTTPError].
func unwrapClientError(err error) error {
	var nre *NonRetryableError
	if errors.As(err, &nre) {
		return nre.Err
	}
	var exh *ExhaustedError
	if errors.As(err, &exh) {
		var se *statusError
		if errors.As(exh.Err, &se) {
			return &HTTPError{Status: se.status, Body: se.body}
		}
		return exh
	}
	return err
}

// drainBody reads and closes a response body, returning it as a string for
// diagnostics. It tolerates read errors (returning whatever was read).
func drainBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Get issues a GET request and returns the raw response (caller closes Body).
func (c *Client) Get(ctx context.Context, path string) (*http.Response, error) {
	return c.Do(ctx, http.MethodGet, path, nil)
}

// Post issues a POST request with the given body and returns the raw response.
func (c *Client) Post(ctx context.Context, path string, body io.Reader) (*http.Response, error) {
	return c.Do(ctx, http.MethodPost, path, body)
}

// GetJSON issues a GET request and decodes a successful JSON response into out.
// out must be a non-nil pointer. It is a free function rather than a method so
// the response type can be inferred generically without method type parameters.
func GetJSON[T any](ctx context.Context, c *Client, path string, out *T) error {
	resp, err := c.Get(ctx, path)
	if err != nil {
		return err
	}
	return decodeJSON(resp, out)
}

// PostJSON marshals body to JSON, POSTs it with a JSON Content-Type, and decodes
// a successful JSON response into out. out must be a non-nil pointer.
func PostJSON[B any, T any](ctx context.Context, c *Client, path string, body B, out *T) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("todoku: marshal request body: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(buf), "application/json")
	if err != nil {
		return err
	}
	return decodeJSON(resp, out)
}

// decodeJSON reads and JSON-decodes a response body into out, always closing the
// body. A nil out discards the body (useful for 204-style responses).
func decodeJSON[T any](resp *http.Response, out *T) error {
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("todoku: decode JSON response: %w", err)
	}
	return nil
}
