package todoku

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// drainReqBody reads and closes an outgoing request body so it can be replayed
// across retry attempts.
func drainReqBody(req *http.Request) ([]byte, error) {
	defer req.Body.Close()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// newBytesReadCloser wraps a byte slice as a fresh io.ReadCloser for a replayed
// request body.
func newBytesReadCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}

// retryRoundTripper wraps a [Client] as an [http.RoundTripper], so the full
// todoku resilience stack (retry + backoff + Retry-After + idempotency +
// budget + breaker) composes into ANY *http.Client — the BOREALIS Law-2
// composition seam (drop into akeyless-go, cloud SDKs, etc.). It is the dual of
// [Client.do]: the request is already built, so it replays the buffered body
// across attempts and applies the client's auth/headers/policy.
type retryRoundTripper struct {
	client *Client
}

// RoundTripper returns an [http.RoundTripper] backed by this client's policy.
// Install it on any transport-consuming SDK to inherit todoku's retries:
//
//	sdk.HTTPClient = &http.Client{Transport: cli.RoundTripper()}
func (c *Client) RoundTripper() http.RoundTripper {
	return &retryRoundTripper{client: c}
}

// StandardClient returns a plain *http.Client whose Transport is this client's
// [RoundTripper], for drop-in into APIs that demand a concrete *http.Client.
// The returned client's Timeout mirrors the configured per-request timeout.
func (c *Client) StandardClient() *http.Client {
	return &http.Client{
		Transport: c.RoundTripper(),
		Timeout:   c.timeout,
	}
}

// RoundTrip implements [http.RoundTripper], running the request through the
// client's retry/backoff/storm-control machinery. It buffers the body so it can
// be replayed; auth and User-Agent are applied per attempt exactly as in
// [Client.Do].
func (rt *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c := rt.client
	ctx := req.Context()

	// Buffer the body for replay across attempts.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := drainReqBody(req)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}

	method := req.Method
	url := req.URL.String()
	idemKey := ""
	if c.keyFunc != nil {
		idemKey = c.keyFunc(method, url)
	}
	retryAllowedForMethod := c.retryAllowedForMethod(method, idemKey)

	attempt := func(ctx context.Context) (*http.Response, error) {
		r := req.Clone(ctx)
		if bodyBytes != nil {
			r.Body = newBytesReadCloser(bodyBytes)
			r.ContentLength = int64(len(bodyBytes))
		}
		if r.Header.Get("User-Agent") == "" {
			r.Header.Set("User-Agent", c.userAgent)
		}
		if idemKey != "" {
			r.Header.Set("Idempotency-Key", idemKey)
		}
		c.auth.Apply(r)

		var breakerDone func(bool)
		if c.breaker != nil {
			done, berr := c.breaker.Allow()
			if berr != nil {
				return nil, &NonRetryableError{Err: berr}
			}
			breakerDone = done
		}

		resp, err := c.transport().RoundTrip(r)
		retry, policyErr := c.checkRetry(ctx, resp, err)
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
			return nil, err
		}
		if !retry {
			// Success or a permanent status: hand the live response back to
			// the caller (RoundTripper contract — caller owns the body).
			return resp, nil
		}
		// Retryable status: drain so the connection is reusable, capture the
		// Retry-After hint, and signal a retry.
		bodyStr := drainBody(resp)
		se := &statusError{status: resp.StatusCode, body: bodyStr}
		if d, ok := RetryAfter(resp, c.now); ok {
			se.retryAfter, se.retryAfterSet = d, true
		}
		return nil, se
	}

	resp, err := RetryWithBackoffClass(ctx, c.callRetryConfig(retryAllowedForMethod), clientRetryable, attempt)
	if err != nil {
		// On a retryable-status exhaustion the caller still expects an
		// (resp,err) pair from a RoundTripper; surface the typed error.
		return nil, unwrapClientError(err)
	}
	return resp, nil
}

// transport returns the client's underlying RoundTripper, defaulting to
// http.DefaultTransport when none is configured.
func (c *Client) transport() http.RoundTripper {
	if c.httpClient != nil && c.httpClient.Transport != nil {
		return c.httpClient.Transport
	}
	return http.DefaultTransport
}

// TransportConfig captures the transport-level storm controls the AWS Builders'
// Library treats as co-equal with backoff. The zero value is sensible: it
// leaves Go's defaults in place except where a field is non-zero.
type TransportConfig struct {
	// MaxIdleConns caps total idle keep-alive connections. 0 keeps the default.
	MaxIdleConns int
	// MaxIdleConnsPerHost caps idle connections per host — the key storm
	// control against a single hot backend. 0 keeps the default (2).
	MaxIdleConnsPerHost int
	// MaxConnsPerHost caps total connections per host (active + idle). 0 = no
	// limit (the default).
	MaxConnsPerHost int
	// IdleConnTimeout bounds how long an idle keep-alive connection lingers.
	IdleConnTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers after the
	// request is fully written — caps a server that accepts the connection but
	// never answers.
	ResponseHeaderTimeout time.Duration
	// TLSHandshakeTimeout bounds the TLS handshake.
	TLSHandshakeTimeout time.Duration
	// ExpectContinueTimeout bounds the wait for a 100-continue.
	ExpectContinueTimeout time.Duration
}

// TunedTransport clones http.DefaultTransport and applies the [TransportConfig]
// storm controls, returning a ready *http.Transport. Compose it via
// [WithTransport]. For HTTP/2 health-check pings (the "h2 silent dead
// connection" cure) layer the todoku/h2 sub-package on top of the result.
func TunedTransport(cfg TransportConfig) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	var t *http.Transport
	if ok {
		t = base.Clone()
	} else {
		t = &http.Transport{}
	}
	if cfg.MaxIdleConns != 0 {
		t.MaxIdleConns = cfg.MaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost != 0 {
		t.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	}
	if cfg.MaxConnsPerHost != 0 {
		t.MaxConnsPerHost = cfg.MaxConnsPerHost
	}
	if cfg.IdleConnTimeout != 0 {
		t.IdleConnTimeout = cfg.IdleConnTimeout
	}
	if cfg.ResponseHeaderTimeout != 0 {
		t.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	}
	if cfg.TLSHandshakeTimeout != 0 {
		t.TLSHandshakeTimeout = cfg.TLSHandshakeTimeout
	}
	if cfg.ExpectContinueTimeout != 0 {
		t.ExpectContinueTimeout = cfg.ExpectContinueTimeout
	}
	return t
}
