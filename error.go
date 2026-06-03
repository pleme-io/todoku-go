package todoku

import (
	stderrors "errors"
	"fmt"
	"time"
)

// HTTPError is returned when a request completes but the server responds with a
// non-2xx status code that is not (or no longer) retryable. It is the Go analog
// of the Rust `TodokuError::Http` variant.
type HTTPError struct {
	// Status is the HTTP status code (e.g. 404, 500).
	Status int
	// Body is the response body, captured for diagnostics. It may be truncated
	// or empty if the body could not be read.
	Body string
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// statusError is the lightweight sentinel returned by the internal request loop
// for a retryable status code, carrying the code so [Client.Do]'s classifier can
// inspect it. It never escapes the package: a non-retryable status surfaces as
// an [*HTTPError] instead.
type statusError struct {
	status int
	body   string
	// retryAfter carries a parsed Retry-After delay so the client's retry loop
	// can override the computed backoff. retryAfterSet distinguishes a genuine
	// zero ("retry now") from "no header".
	retryAfter    time.Duration
	retryAfterSet bool
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d (retryable): %s", e.status, e.body)
}

// retryableCarrier is a behaviour-classifying interface (BOREALIS Law 5): any
// error that knows whether it is transient can opt into the retry decision
// WITHOUT depending on this package's concrete types — it merely implements
// Retryable() bool. This mirrors net.Error.Temporary() and the errors-go
// severityCarrier/codeCarrier pattern. The retry classifier honours it before
// falling back to status/transport heuristics.
type retryableCarrier interface {
	error
	// Retryable reports whether the operation that produced this error may be
	// safely retried. true means transient; false means permanent.
	Retryable() bool
}

// temporaryCarrier matches the long-standing net.Error / stdlib convention of a
// Temporary() bool method. It is honoured as a synonym for Retryable() so
// external errors (e.g. *net.OpError) participate without any todoku import.
type temporaryCarrier interface {
	error
	Temporary() bool
}

// RetryableError wraps a cause and forces a retry classification, so callers of
// the generic [RetryWithBackoff] can mark an arbitrary error transient (or
// permanent) via the Law-5 carrier rather than threading a bespoke classifier.
// It implements [retryableCarrier].
type RetryableError struct {
	Err   error
	Retry bool
}

// Error implements the error interface.
func (e *RetryableError) Error() string {
	if e.Err == nil {
		return "retryable error"
	}
	return e.Err.Error()
}

// Unwrap exposes the cause for [errors.Is] / [errors.As].
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable reports the forced classification, satisfying [retryableCarrier].
func (e *RetryableError) Retryable() bool { return e.Retry }

// retryableViaCarrier consults the Law-5 carriers in err's chain.
//
// The explicit Retryable() carrier is AUTHORITATIVE in both directions: a type
// that implements it has opted fully into the retry decision, so its verdict
// (true or false) is returned with classified=true.
//
// Temporary() is consulted only as a POSITIVE hint and never as a veto: the
// stdlib net.Error.Temporary() convention is deprecated and famously returns
// false for genuinely transient failures (e.g. connection-refused), so a false
// Temporary() must NOT demote an error to non-retryable. Only Temporary()==true
// is honoured (promote to retry); a false leaves the decision unclassified so
// the caller's heuristics (transport-errors-are-transient) still apply.
func retryableViaCarrier(err error) (retry bool, classified bool) {
	var rc retryableCarrier
	if stderrors.As(err, &rc) {
		return rc.Retryable(), true
	}
	var tc temporaryCarrier
	if stderrors.As(err, &tc) && tc.Temporary() {
		return true, true
	}
	return false, false
}
