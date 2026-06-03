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
package todoku

// Version is the library version, surfaced in the default User-Agent header.
const Version = "0.1.0"
