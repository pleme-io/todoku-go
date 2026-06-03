// Package todoku is the Go representation of pleme-io's HTTP client framework
// (届く, "to reach / to arrive / to be delivered"). It mirrors the Rust `todoku`
// crate so every pleme-io app with API calls follows the same patterns: a
// builder-style [Client] with pluggable [Auth], exponential-backoff retry, and
// JSON convenience helpers — all on pure net/http with zero external deps.
//
// The mandate, like the Rust crate: no hand-rolled retry loops, no ad-hoc
// auth-header plumbing, no map-of-options constructors. Construct one [Client]
// with functional options, share it everywhere.
//
//	cli, _ := todoku.New(
//		todoku.WithBaseURL("https://api.example.com/v1"),
//		todoku.WithAuth(todoku.BearerAuth("tok")),
//		todoku.WithRetry(todoku.DefaultRetry()),
//	)
//
//	var out struct{ Name string `json:"name"` }
//	err := todoku.GetJSON(ctx, cli, "/widgets/1", &out)
//
// The generic [RetryWithBackoff] is the canonical fleet retry primitive: any
// flaky operation (NATS publish, DB write, subprocess call) consumes it instead
// of hand-rolling a backoff loop. The [Client] uses it internally — backoff
// lives in exactly one place.
//
// # Resilience surface (BOREALIS §2.6)
//
// The client composes the AWS Builders' Library storm-control patterns over the
// single backoff primitive, all opt-in and additive:
//
//   - Typed [Jitter] strategy ([JitterFull] default, plus None/Equal).
//   - Server [RetryAfter] honoring (overrides the computed backoff on 429/503).
//   - A [CheckRetry] policy hook (the go-retryablehttp decision shape) plus the
//     Law-5 Retryable() carrier so external errors classify themselves.
//   - An opt-in [CircuitBreaker] (sony/gobreaker vocabulary, in-package).
//   - An idempotency-key gate ([WithStrictIdempotency] + [WithIdempotencyKeys]).
//   - Transport storm controls ([TunedTransport]: per-host pool caps,
//     ResponseHeaderTimeout) and HTTP/2 health-check pings (todoku/h2).
//   - A token-bucket retry budget (todoku/budget) — the primary storm control.
//   - [Client.RoundTripper] / [Client.StandardClient] adapters to drop the whole
//     stack into any *http.Client (akeyless-go, cloud SDKs) — Law 2 composition.
//   - Typed [Config] + [FromConfig] (BOREALIS Law 3 / §3.5).
//
// Dependency-bearing features are quarantined in leaf sub-packages (Law 6): the
// core todoku package is zero-dep and offline-buildable; todoku/budget carries
// golang.org/x/time/rate and todoku/h2 carries golang.org/x/net/http2.
package todoku

// Version is the library version, surfaced in the default User-Agent header.
const Version = "0.2.0"
