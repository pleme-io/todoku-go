# todoku-go

Go representation of pleme-io's HTTP client framework (**届く**, *"to reach / to
be delivered"*). The Go counterpart to the Rust
[`todoku`](https://github.com/pleme-io/todoku) crate: the same model, so every
Go service and tool makes authenticated, retrying API calls the same way.

> **Pure stdlib. Zero dependencies.** Built on `net/http`, `context`, and
> generics — nothing else. No hand-rolled retry loops, no ad-hoc auth-header
> plumbing, no map-of-options constructors.

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

## Build & test

```bash
GOTOOLCHAIN=local go build ./...
GOTOOLCHAIN=local go test ./...
```

[`WithBaseURL`]: client.go
[`WithAuth`]: client.go
[`WithTimeout`]: client.go
[`WithRetry`]: client.go
[`WithHTTPClient`]: client.go
