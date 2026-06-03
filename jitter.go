package todoku

import (
	"math/rand/v2"
	"time"
)

// Jitter is the typed jitter strategy applied to each computed backoff delay,
// mirroring the AWS Builders' Library taxonomy. It replaces the legacy
// [RetryConfig.Jitter] ±fraction field as the preferred knob — that field still
// works for back-compat (see [RetryConfig.effectiveJitter]), but new code should
// set [RetryConfig.JitterMode].
//
// The AWS recommendation — and this package's default — is [JitterFull].
type Jitter int

const (
	// jitterUnset is the zero value: no typed strategy was selected, so the
	// legacy [RetryConfig.Jitter] ±fraction band applies (preserving exact
	// back-compat for callers that never touch the typed enum). It is
	// unexported because new code should pick an explicit strategy; the
	// documented default for the typed enum is [JitterFull].
	jitterUnset Jitter = iota

	// JitterFull is the AWS-recommended strategy: the actual delay is a uniform
	// random draw in [0, computed]. Full jitter maximally decorrelates retrying
	// clients — the most effective thundering-herd mitigation (AWS Builders'
	// Library, "Timeouts, retries, and backoff with jitter"). It is the
	// documented default ([DefaultRetryJitter] selects it).
	JitterFull

	// JitterNone disables jitter: the delay is exactly the computed backoff.
	// Use for deterministic tests or when an external scheduler already
	// decorrelates clients.
	JitterNone

	// JitterEqual splits the delay into a fixed half plus a random half:
	// computed/2 + random(0, computed/2). It guarantees a minimum spacing
	// (half the computed backoff) while still decorrelating, trading some
	// herd-mitigation for a tighter lower bound.
	JitterEqual
)

// DefaultRetryJitter is the documented default jitter strategy: [JitterFull].
const DefaultRetryJitter = JitterFull

// String returns the lowercase strategy name ("full", "none", "equal").
func (j Jitter) String() string {
	switch j {
	case JitterFull:
		return "full"
	case JitterNone:
		return "none"
	case JitterEqual:
		return "equal"
	default:
		return "full"
	}
}

// RandSource is the minimal random surface the jitter strategies need. The
// stdlib *math/rand/v2.Rand satisfies it, and so does any deterministic stub a
// test wants to thread in via [RetryConfig.Rand]. A nil source falls back to
// the package-global math/rand/v2 generator.
type RandSource interface {
	// Float64 returns a pseudo-random float64 in [0.0, 1.0).
	Float64() float64
}

// applyJitter perturbs a computed (pre-jitter) delay according to the typed
// strategy, drawing randomness from src (or the global generator if src is
// nil). It never returns a negative duration.
func applyJitter(mode Jitter, d time.Duration, src RandSource) time.Duration {
	if d <= 0 {
		return 0
	}
	f := rand.Float64
	if src != nil {
		f = src.Float64
	}
	switch mode {
	case JitterNone:
		return d
	case JitterEqual:
		half := d / 2
		out := half + time.Duration(float64(half)*f())
		if out < 0 {
			return 0
		}
		return out
	case JitterFull:
		fallthrough
	default:
		out := time.Duration(float64(d) * f())
		if out < 0 {
			return 0
		}
		return out
	}
}
