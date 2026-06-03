// Package budget provides a token-bucket retry/request budget for todoku — the
// AWS Builders' Library "retry budget as the primary storm-control" pattern
// (more load-bearing than the circuit breaker: retries spend tokens, the bucket
// refills over time, and an exhausted bucket sheds retry load).
//
// It lives in a sub-package because it carries the golang.org/x/time/rate
// dependency (BOREALIS Law 6: dep-bearing features are clearly scoped, the core
// todoku package stays zero-dep and offline-buildable). It satisfies todoku's
// todoku.RetryBudget interface, so it drops in via todoku.WithRetryBudget:
//
//	b := budget.NewTokenBucket(budget.Default())
//	cli, _ := todoku.New(todoku.WithRetryBudget(b))
//
// golang.org/x/time/rate is the single justified external dependency here: it
// is the Go-team-owned, de-facto-standard token-bucket limiter (already named
// in the BOREALIS elevation plan as the one optional dep for todoku). Hand-
// rolling a concurrent token bucket would duplicate well-tested stdlib-adjacent
// code for no benefit — exactly what the limiter is for.
package budget

import "golang.org/x/time/rate"

// Config is the typed token-bucket configuration (yaml-tagged for shikumi).
type Config struct {
	// RetriesPerSecond is the sustained refill rate of retry tokens.
	RetriesPerSecond float64 `yaml:"retriesPerSecond"`
	// Burst is the bucket capacity — the most retries that may fire in a tight
	// cluster before the rate limit bites.
	Burst int `yaml:"burst"`
}

// Default returns the storm-control default: 5 retry tokens/sec sustained, a
// burst of 10. Conservative enough to prevent a retry storm while permitting a
// normal transient blip to recover.
func Default() Config {
	return Config{RetriesPerSecond: 5, Burst: 10}
}

// TokenBucket is a concurrency-safe token-bucket retry budget backed by
// golang.org/x/time/rate. It implements todoku.RetryBudget.
type TokenBucket struct {
	lim *rate.Limiter
}

// NewTokenBucket builds a [TokenBucket] from cfg, applying [Default] for any
// non-positive field.
func NewTokenBucket(cfg Config) *TokenBucket {
	d := Default()
	if cfg.RetriesPerSecond <= 0 {
		cfg.RetriesPerSecond = d.RetriesPerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = d.Burst
	}
	return &TokenBucket{lim: rate.NewLimiter(rate.Limit(cfg.RetriesPerSecond), cfg.Burst)}
}

// FromConfig constructs a [TokenBucket] from an already-loaded [Config]
// sub-struct (BOREALIS §3.5 — does not call shikumi.Load).
func FromConfig(cfg Config) (*TokenBucket, error) {
	return NewTokenBucket(cfg), nil
}

// AllowRetry reports whether a retry token is available right now, spending one
// if so. It satisfies todoku.RetryBudget — a false result tells the client to
// stop retrying and shed load.
func (b *TokenBucket) AllowRetry() bool {
	return b.lim.Allow()
}

// Tokens reports the (approximate) number of retry tokens currently available,
// for diagnostics.
func (b *TokenBucket) Tokens() float64 {
	return b.lim.Tokens()
}
