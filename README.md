# todoku-go

Go representation of pleme-io's HTTP client framework (**届く**, *"to reach / to
be delivered"*). The Go counterpart to the Rust
[`todoku`](https://github.com/pleme-io/todoku) crate: the same model, so every
Go service and tool makes authenticated, retrying API calls the same way.

> **Zero-dep core.** The core package is built on `net/http`, `context`, and
> generics — nothing else, offline-buildable. The two dependency-bearing
> resilience features are confined to leaf sub-packages (BOREALIS Law 6):
> `todoku/h2` (`golang.org/x/net/http2`) and `todoku/budget`
> (`golang.org/x/time/rate`). No hand-rolled retry loops, no ad-hoc auth-header
> plumbing, no map-of-options constructors.

## What

One shared outbound-HTTP `Client` + one generic `RetryWithBackoff` primitive,
plus the AWS-Builders'-Library storm-control surface (full-jitter backoff,
`Retry-After`, idempotency keys, a circuit breaker, a token-bucket retry budget,
transport tuning), a service-reachability `Probe`, and single-flight /
result-cache dedup middleware. All on `net/http`, composable into any
`*http.Client`.

## Why

The fleet must make resilient API calls the *same shape* everywhere — a second
tool that hand-rolls a retry loop, an auth header, or a request-dedup cache is a
bug (PRIME DIRECTIVE: duplication budget zero). `todoku-go` is the single home
for that concern: build one `Client` with functional options, share it across
goroutines, and inherit retries/backoff/dedup everywhere — including inside
third-party SDKs via `RoundTripper()` / `StandardClient()`.

## Install

```bash
go get github.com/pleme-io/todoku-go
```

Requires Go 1.25+. The core package pulls no external modules; `todoku/h2` and
`todoku/budget` pull only the named `golang.org/x/*` modules when imported.

## Model

- **Client** — a `net/http`-backed [`Client`](client.go) built with functional
  options ([`WithBaseURL`], [`WithAuth`], [`WithTimeout`], [`WithRetry`],
  [`WithHTTPClient`]). Safe for concurrent use; share one everywhere.
- **Auth** — a pluggable [`Auth`](auth.go) interface (`Apply(*http.Request)`)
  with `NoAuth`, `BearerAuth(token)`, `BasicAuth(user, pass)`, and
  `HeaderAuth(name, value)`.
- **Retry** — one generic backoff primitive,
  [`RetryWithBackoff[T]`](retry.go): exponential backoff + jitter + max
  attempts, classifying retryable (5xx / network) vs. permanent failures. The
  `Client` retry loop is *built on top of it* — there is exactly one backoff
  implementation in the package, never a duplicate.

## Why one retry primitive

Any flaky operation in the fleet — a NATS publish, a DB write, a subprocess
call — should consume `RetryWithBackoff` rather than hand-roll a loop. The HTTP
client is just its first consumer.

```go
val, err := todoku.RetryWithBackoff(ctx, todoku.DefaultRetry(),
    func(ctx context.Context) (Result, error) {
        return doFlakyThing(ctx)
    })
```

Need to distinguish transient from permanent failures? Use
`RetryWithBackoffClass` with a classifier `func(error) bool`.

## Usage

```go
package main

import (
    "context"

    "github.com/pleme-io/todoku-go"
)

type Widget struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

func main() {
    cli, _ := todoku.New(
        todoku.WithBaseURL("https://api.example.com/v1"),
        todoku.WithAuth(todoku.BearerAuth("my-token")),
        todoku.WithRetry(todoku.DefaultRetry()),
    )

    ctx := context.Background()

    // Typed JSON helpers (free functions so T is inferred):
    var w Widget
    if err := todoku.GetJSON(ctx, cli, "/widgets/1", &w); err != nil {
        // *todoku.HTTPError on non-retryable status; context error on cancel;
        // *todoku.ExhaustedError when retries run out.
    }

    var created Widget
    _ = todoku.PostJSON(ctx, cli, "/widgets", Widget{Name: "bolt"}, &created)

    // Or drop to the raw response (caller closes Body):
    resp, _ := cli.Do(ctx, "GET", "/raw", nil)
    defer resp.Body.Close()
}
```

### URL joining

With a base URL set, request paths are joined to it, collapsing the boundary
slash so neither a trailing slash on the base nor a leading slash on the path
produces a double slash:

| base | path | result |
|---|---|---|
| `…/v1` | `/items` | `…/v1/items` |
| `…/v1/` | `items` | `…/v1/items` |
| `…///` | `/items` | `…/items` |

With no base URL, the path is used verbatim (treat it as a full URL).

### Errors

| Error | Meaning |
|---|---|
| `*HTTPError` | A non-retryable status (e.g. 400/404), or the final status after retries are exhausted. Carries `Status` and `Body`. |
| `*ExhaustedError` | Every attempt failed (transport error / retryable status) and retries ran out. Wraps the final error and reports `Attempts`. |
| `*NonRetryableError` | (from bare `RetryWithBackoffClass`) the classifier deemed the error permanent. |
| `context.Canceled` / `context.DeadlineExceeded` | The context was cancelled; retries abort immediately. |

Inspect with `errors.As` / `errors.Is`.

### Service reachability — `Probe`

Autodetect how to reach a service before doing real work: `Probe` walks a
candidate grid of schemes (`https` → `http`) × API path prefixes
(`/api/v2` → `/v2` → root, configurable) and returns a typed `Reachability`
whose `BaseURL` is ready to feed `WithBaseURL`. A status below 500 counts as
reachable (a `401`/`404` still proves the host answered). It is backend-agnostic
— it names no service.

```go
cli, _ := todoku.New()
r := cli.Probe(ctx, "gateway.example.com",
    todoku.ProbePaths("/api/v2", "/v2"),
    todoku.ProbeTimeout(2*time.Second))
if r.Reachable {
    api, _ := todoku.New(todoku.WithBaseURL(r.BaseURL)) // e.g. https://gateway.example.com/api/v2
}
```

### Concurrency dedup — `WithSingleFlight` + `WithResultCache`

Two transport middleware (the `func(http.RoundTripper) http.RoundTripper` shape)
that cure a burst of identical outbound reads — the request-deduplication /
funnel pattern, generalised. They compose with the retry stack and any
`*http.Client`.

```go
cli, _ := todoku.New(
    todoku.WithBaseURL("https://api.example.com/v1"),
    todoku.WithResultCache(5*time.Second),  // memoise 2xx GET/HEAD for 5s (inner)
    todoku.WithSingleFlight(nil),           // collapse concurrent identical reads (outer)
)
```

- `WithSingleFlight` collapses **concurrent in-flight** identical `GET`/`HEAD`
  requests onto one underlying round-trip whose response is shared.
- `WithResultCache(ttl)` memoises a **completed 2xx** read for the TTL window so
  repeated reads skip the network. A non-positive TTL is a safe pass-through.
- Only `GET`/`HEAD` are deduped/cached; other methods pass through untouched.
  Use `SingleFlightVary` / `ResultCacheVary` to key on specific headers.

## Configuration

Every load-bearing knob is a typed, `yaml`-tagged struct (BOREALIS Law 3),
loaded once at `main` through `shikumi-go` and handed to a `FromConfig`
constructor — `todoku-go` never loads config itself.

```go
// cfg is the already-loaded todoku.Config sub-struct from shikumi.
cli, err := todoku.FromConfig(cfg, todoku.WithAuth(todoku.BearerAuth(tok)))
```

| Struct | Surface |
|---|---|
| `Config` | base URL, user-agent, timeout, retry, transport, Retry-After, idempotency, breaker |
| `RetryConfigSpec` | max retries, backoff, multiplier, jitter mode, retryable statuses |
| `TransportConfigSpec` | per-host pool caps, response-header / TLS / idle timeouts |
| `BreakerConfigSpec` | trip thresholds + timings for the opt-in circuit breaker |
| `ProbeConfig` | probe schemes, path prefixes, health path, per-attempt timeout, method |
| `budget.Config` | token-bucket retry budget (`todoku/budget` sub-package) |
| `h2.Config` | HTTP/2 health-check ping cadence (`todoku/h2` sub-package) |

Secrets are deliberately absent from config: a live credential lives only in an
`Auth` passed via `WithAuth`, never in config (CFG-09).

## Build & test

```bash
GOTOOLCHAIN=local go build ./...
GOTOOLCHAIN=local go test ./...
```

## Release

`todoku-go` is a Go library released by the **pull model**: a semver git tag is
pushed (TAG-ONLY — no artifact upload), and `proxy.golang.org` fetches the
module lazily on the next `go get`. The flake's `apps.{release,bump}` delegate
to `forge tool <verb> --language go`; `caixa.lisp` (`:kind :Biblioteca`,
`:workflows [ :auto-release ]`) drives the SDLC surface. Version history lives
in [`CHANGELOG.md`](CHANGELOG.md); the current version is the `Version` const in
[`todoku.go`](todoku.go).

[`WithBaseURL`]: client.go
[`WithAuth`]: client.go
[`WithTimeout`]: client.go
[`WithRetry`]: client.go
[`WithHTTPClient`]: client.go
