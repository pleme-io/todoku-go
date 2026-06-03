package todoku

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// CheckRetry is the canonical retry-decision hook (the go-retryablehttp shape):
// given the per-attempt context, the response (nil on a transport error), and
// the transport error (nil on a response), it reports whether the request
// should be retried. A non-nil returned error halts the loop immediately and
// surfaces as the final error — use it to convert an otherwise-retryable
// condition into a hard stop.
//
// CheckRetry generalises the old status-code-only decision: the [Client]
// installs [DefaultCheckRetry] unless [WithCheckRetry] overrides it. It composes
// with — and runs before — the retry budget and circuit breaker.
type CheckRetry func(ctx context.Context, resp *http.Response, err error) (bool, error)

// DefaultCheckRetry is the default policy used by the [Client]. It never retries
// once the context is done; it retries any transport error that is not a
// context error (a Law-5 carrier on the error still wins, see
// [retryableViaCarrier]); and it retries a response whose status is in
// retryStatuses. Everything else (2xx, non-retryable 4xx/5xx) is not retried.
func DefaultCheckRetry(retryStatuses []int) CheckRetry {
	set := make(map[int]struct{}, len(retryStatuses))
	for _, s := range retryStatuses {
		set[s] = struct{}{}
	}
	return func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if ctx.Err() != nil {
			return false, nil
		}
		if err != nil {
			// Carrier classification (Retryable()/Temporary()) is authoritative.
			if decision, ok := retryableViaCarrier(err); ok {
				return decision, nil
			}
			// Context errors are permanent; any other transport error is transient.
			return !isContextErr(err), nil
		}
		if resp == nil {
			return false, nil
		}
		_, retry := set[resp.StatusCode]
		return retry, nil
	}
}

// RetryAfter parses an HTTP Retry-After header value (RFC 7231 §7.1.3), which
// may be either delta-seconds or an HTTP-date. It returns the delay relative to
// now and ok=true when the header is present and parseable; otherwise ok=false.
// A past HTTP-date clamps to zero. now defaults to time.Now when nil — pass a
// fixed clock in tests.
func RetryAfter(resp *http.Response, now func() time.Time) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if now == nil {
		now = time.Now
	}
	// delta-seconds form.
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	// HTTP-date form.
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now())
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
