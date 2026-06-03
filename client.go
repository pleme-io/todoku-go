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
}

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

// New constructs a [Client] from the given options. It never fails: with no
// options it returns a usable client with [NoAuth], [DefaultRetry], a 30s
// timeout, and a default User-Agent.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		auth:      NoAuth(),
		retry:     DefaultRetry(),
		timeout:   30 * time.Second,
		userAgent: fmt.Sprintf("pleme-io/todoku-go %s", Version),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: c.timeout}
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
		c.auth.Apply(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Transport error. Context cancellation is permanent; everything
			// else (DNS, connection reset, timeout) is transient.
			if isContextErr(err) {
				return nil, &NonRetryableError{Err: err}
			}
			return nil, err
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		// Non-2xx. Read + close the body so the connection can be reused, then
		// classify by status code.
		bodyStr := drainBody(resp)
		if c.retry.ShouldRetryStatus(resp.StatusCode) {
			return nil, &statusError{status: resp.StatusCode, body: bodyStr}
		}
		return nil, &NonRetryableError{Err: &HTTPError{Status: resp.StatusCode, Body: bodyStr}}
	}

	resp, err := RetryWithBackoffClass(ctx, c.retry, clientRetryable, attempt)
	if err != nil {
		return nil, unwrapClientError(err)
	}
	return resp, nil
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
