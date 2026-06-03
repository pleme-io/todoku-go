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
	// Jitter is the maximum random perturbation applied to each backoff, as a
	// fraction of the computed delay in [0,1]. 0 disables jitter (deterministic
	// backoff); 0.2 means each delay is scaled by a random factor in [0.8,1.2].
	Jitter float64
	// RetryStatuses is the set of HTTP status codes the [Client] treats as
	// retryable. It has no effect on bare [RetryWithBackoff] calls, which
	// classify retryability via the operation's returned error instead.
	RetryStatuses []int
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

// jitter applies up to ±Jitter random perturbation (as a fraction) to a delay.
// A non-positive Jitter returns the delay unchanged.
func (c RetryConfig) jitter(d time.Duration) time.Duration {
	if c.Jitter <= 0 || d <= 0 {
		return d
	}
	// factor in [1-Jitter, 1+Jitter].
	factor := 1 + c.Jitter*(2*rand.Float64()-1)
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

		// Permanent error → stop now.
		if retryable != nil && !retryable(err) {
			return zero, &NonRetryableError{Err: err}
		}

		// Out of retries → exhausted.
		if attempt >= maxRetries {
			return zero, &ExhaustedError{Attempts: attempt + 1, Err: err}
		}

		// Sleep with backoff before the next attempt, respecting cancellation.
		delay := cfg.jitter(cfg.BackoffFor(attempt))
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
