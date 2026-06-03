package todoku

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// RetryConfig is the canonical retry/backoff configuration across pleme-io fleet
// binaries (the Go analog of the Rust `todoku::RetryPolicy`). It drives both
// HTTP retries inside the [Client] and any other flaky operation via the generic
// [RetryWithBackoff].
//
// The backoff for attempt n (0-indexed) is:
//
//	min(InitialBackoff * Multiplier^n, MaxBackoff)
//
// optionally perturbed by up to ±Jitter (as a fraction in [0,1]) to avoid
// thundering-herd synchronisation across clients.
type RetryConfig struct {
	// MaxRetries is the number of retries *after* the first attempt (0 = the
	// operation runs exactly once). The operation runs up to MaxRetries+1 times.
	MaxRetries int
	// InitialBackoff is the delay before the first retry.
	InitialBackoff time.Duration
	// MaxBackoff caps the computed exponential backoff.
	MaxBackoff time.Duration
	// Multiplier is the exponential growth factor between attempts.
	Multiplier float64
	// Jitter is the LEGACY ±fraction knob, retained for back-compat: the
	// maximum random perturbation applied to each backoff as a fraction of the
	// computed delay in [0,1]. 0.2 means each delay is scaled by a random
	// factor in [0.8,1.2]. New code should leave this zero and set [JitterMode]
	// instead; this field is only consulted when JitterMode is the default
	// [JitterFull] AND it is non-zero, so the typed enum always wins when set
	// to a non-default value.
	//
	// Deprecated: prefer [RetryConfig.JitterMode]. Kept working for existing
	// callers; see [RetryConfig.effectiveJitter].
	Jitter float64
	// JitterMode is the typed jitter strategy (the preferred knob). The zero
	// value is [JitterFull], the AWS-recommended default.
	JitterMode Jitter
	// Rand is an optional deterministic randomness source for jitter, primarily
	// for tests. A nil Rand uses the package-global math/rand/v2 generator.
	Rand RandSource
	// RetryStatuses is the set of HTTP status codes the [Client] treats as
	// retryable. It has no effect on bare [RetryWithBackoff] calls, which
	// classify retryability via the operation's returned error instead.
	RetryStatuses []int

	// DelayOverride, when non-nil, lets a caller replace the computed
	// (jittered) backoff for the wait after a failed attempt — returning
	// (d, true) uses d verbatim (still respecting MaxBackoff), (_, false)
	// keeps the exponential backoff. The [Client] uses it to honour
	// Retry-After. It does NOT introduce a second backoff implementation: the
	// single loop in [RetryWithBackoff] still owns sleeping and cancellation.
	DelayOverride func(attempt int, err error) (time.Duration, bool)

	// BeforeRetry, when non-nil, is consulted before each retry is admitted
	// (after the decision to retry, before the backoff sleep). Returning a
	// non-nil error stops the loop immediately and surfaces that error — the
	// [Client] uses it to gate retries through the retry budget and circuit
	// breaker (BOREALIS storm controls).
	BeforeRetry func(attempt int, err error) error
}

// DefaultRetry returns the standard fleet retry configuration: 3 retries,
// 500ms initial backoff doubling up to 30s, 20% jitter, retrying on the usual
// transient HTTP statuses (429, 500, 502, 503, 504).
func DefaultRetry() RetryConfig {
	return RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2.0,
		Jitter:         0.2,
		RetryStatuses:  []int{429, 500, 502, 503, 504},
	}
}

// NoRetry returns a configuration that runs the operation exactly once. The
// other fields keep their [DefaultRetry] values so a later tweak of MaxRetries
// still produces sane backoff.
func NoRetry() RetryConfig {
	c := DefaultRetry()
	c.MaxRetries = 0
	return c
}

// AggressiveRetry returns a configuration tuned for critical operations: 5
// retries with a shorter 200ms initial backoff and a 60s cap.
func AggressiveRetry() RetryConfig {
	c := DefaultRetry()
	c.MaxRetries = 5
	c.InitialBackoff = 200 * time.Millisecond
	c.MaxBackoff = 60 * time.Second
	return c
}

// BackoffFor computes the (pre-jitter) backoff for a 0-indexed attempt number,
// clamped to MaxBackoff. Negative attempts are treated as 0.
func (c RetryConfig) BackoffFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := float64(c.InitialBackoff)
	for i := 0; i < attempt; i++ {
		base *= c.Multiplier
		// Stop early once we have blown past the cap to avoid overflowing the
		// float into +Inf on pathological multipliers / attempt counts.
		if base >= float64(c.MaxBackoff) {
			return c.MaxBackoff
		}
	}
	d := time.Duration(base)
	if d > c.MaxBackoff {
		return c.MaxBackoff
	}
	if d < 0 {
		return c.MaxBackoff
	}
	return d
}

// ShouldRetryStatus reports whether the given HTTP status code is in the
// configured retryable set.
func (c RetryConfig) ShouldRetryStatus(status int) bool {
	for _, s := range c.RetryStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// jitter applies the configured jitter strategy to a computed delay. When
// [RetryConfig.JitterMode] is a non-default value ([JitterNone]/[JitterEqual])
// it wins. Otherwise, for back-compat, a non-zero legacy [RetryConfig.Jitter]
// applies the historical ±fraction band; if that too is zero the default
// [JitterFull] strategy is used.
func (c RetryConfig) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// A typed strategy was explicitly selected — it is the forward knob and
	// always wins.
	if c.JitterMode != jitterUnset {
		return applyJitter(c.JitterMode, d, c.Rand)
	}
	// No typed strategy: honour the legacy ±fraction band when the caller set
	// it (DefaultRetry uses 0.2). A zero band preserves the historical "0
	// disables jitter" contract — deterministic backoff — so existing
	// zero-value callers are byte-for-byte unaffected.
	if c.Jitter > 0 {
		return c.legacyBandJitter(d)
	}
	return d
}

// legacyBandJitter reproduces the historical ±Jitter fraction band so existing
// callers that set [RetryConfig.Jitter] keep their exact prior behaviour.
func (c RetryConfig) legacyBandJitter(d time.Duration) time.Duration {
	f := rand.Float64
	if c.Rand != nil {
		f = c.Rand.Float64
	}
	// factor in [1-Jitter, 1+Jitter].
	factor := 1 + c.Jitter*(2*f()-1)
	out := time.Duration(float64(d) * factor)
	if out < 0 {
		return 0
	}
	return out
}

// ErrNonRetryable wraps an error that the caller's classifier deemed permanent,
// so [RetryWithBackoff] stopped immediately instead of retrying. Inspect with
// [errors.As] to recover the [*NonRetryableError] and its underlying cause.
type NonRetryableError struct{ Err error }

// Error implements the error interface.
func (e *NonRetryableError) Error() string {
	return fmt.Sprintf("non-retryable error: %v", e.Err)
}

// Unwrap exposes the underlying error for [errors.Is] / [errors.As].
func (e *NonRetryableError) Unwrap() error { return e.Err }

// ExhaustedError is returned by [RetryWithBackoff] when every attempt failed and
// retries were exhausted. Attempts is the total number of times the operation
// ran (>= 1); Err is the error from the final attempt.
type ExhaustedError struct {
	Attempts int
	Err      error
}

// Error implements the error interface.
func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("retry exhausted after %d attempts: %v", e.Attempts, e.Err)
}

// Unwrap exposes the final-attempt error for [errors.Is] / [errors.As].
func (e *ExhaustedError) Unwrap() error { return e.Err }

// Classifier decides, given the error from one attempt, whether the operation
// should be retried. Returning true means "transient, retry"; false means
// "permanent, stop now". A nil Classifier passed to [RetryWithBackoff] is
// treated as "retry every error".
type Classifier func(error) bool

// RetryWithBackoff runs fn up to cfg.MaxRetries+1 times with exponential backoff
// and jitter, returning the first successful typed value. It is the single
// backoff implementation in this package — the [Client] retry loop is built on
// top of it, and any other flaky fleet operation should consume it rather than
// hand-rolling a loop.
//
// Between failed attempts it sleeps for cfg.BackoffFor(attempt) (jittered) and
// consults retryable: if retryable returns false for an error the loop stops
// immediately and returns a [*NonRetryableError]. A nil retryable retries every
// error. If every attempt fails while retryable keeps allowing retries, it
// returns an [*ExhaustedError] carrying the final error.
//
// The context governs both the sleeps and (by convention) fn itself: if ctx is
// cancelled during a backoff sleep, RetryWithBackoff returns the zero value of T
// and ctx.Err() immediately, without running another attempt.
func RetryWithBackoff[T any](ctx context.Context, cfg RetryConfig, fn func(context.Context) (T, error)) (T, error) {
	return retryWithClassifier(ctx, cfg, nil, fn)
}

// RetryWithBackoffClass is [RetryWithBackoff] with an explicit retry classifier,
// for operations that distinguish transient from permanent failures (e.g. retry
// a 503 but not a 400). The [Client] uses this internally.
func RetryWithBackoffClass[T any](ctx context.Context, cfg RetryConfig, retryable Classifier, fn func(context.Context) (T, error)) (T, error) {
	return retryWithClassifier(ctx, cfg, retryable, fn)
}

func retryWithClassifier[T any](ctx context.Context, cfg RetryConfig, retryable Classifier, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	maxRetries := cfg.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 0; ; attempt++ {
		// Honour cancellation before spending an attempt.
		if err := ctx.Err(); err != nil {
			return zero, err
		}

		value, err := fn(ctx)
		if err == nil {
			return value, nil
		}

		// Law-5 carrier wins over the heuristic: an error that classifies
		// itself (Retryable()/Temporary()) is authoritative even when no
		// classifier was supplied.
		if decision, ok := retryableViaCarrier(err); ok {
			if !decision {
				return zero, &NonRetryableError{Err: err}
			}
			// carrier says transient → fall through to the retry path.
		} else if retryable != nil && !retryable(err) {
			// No carrier; defer to the explicit classifier.
			return zero, &NonRetryableError{Err: err}
		}

		// Out of retries → exhausted.
		if attempt >= maxRetries {
			return zero, &ExhaustedError{Attempts: attempt + 1, Err: err}
		}

		// Storm-control gate (budget / breaker): a non-nil error stops here.
		if cfg.BeforeRetry != nil {
			if stop := cfg.BeforeRetry(attempt, err); stop != nil {
				return zero, stop
			}
		}

		// Compute the wait. A DelayOverride (e.g. server Retry-After) replaces
		// the exponential backoff but is still clamped to MaxBackoff; otherwise
		// the single backoff impl applies.
		var delay time.Duration
		if cfg.DelayOverride != nil {
			if d, ok := cfg.DelayOverride(attempt, err); ok {
				if cfg.MaxBackoff > 0 && d > cfg.MaxBackoff {
					d = cfg.MaxBackoff
				}
				delay = d
			} else {
				delay = cfg.jitter(cfg.BackoffFor(attempt))
			}
		} else {
			delay = cfg.jitter(cfg.BackoffFor(attempt))
		}

		// Sleep with backoff before the next attempt, respecting cancellation.
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// isContextErr reports whether err is (or wraps) a context cancellation or
// deadline error. Such errors are never retried by the [Client].
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
