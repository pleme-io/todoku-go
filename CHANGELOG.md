# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-06-03

### Added
- **Service-reachability `Probe`** — `Client.Probe(ctx, target, opts…)`,
  `Client.ProbeWithConfig(ctx, target, cfg)`, and the package-level `Probe`
  convenience. Autodetects the working scheme (https → http) and the API path
  prefix a service exposes (`/api/v2` vs `/v2`, configurable via `ProbePaths`)
  over a candidate grid, returning a typed `Reachability` (`Scheme`, `Host`,
  `PathPrefix`, `BaseURL`, `StatusCode`, `Latency`, `Attempts`, `Err`) ready to
  hand to `WithBaseURL`. A status below 500 counts as reachable (a 401/404 still
  proves the host answered). Backend-agnostic — names no service.
- `ProbeConfig` (yaml-tagged) + `ProbeOption`s (`ProbeSchemes`, `ProbePaths`,
  `ProbeHealthPath`, `ProbeTimeout`, `ProbeMethod`) and `DefaultProbePaths`.
- **`WithSingleFlight(sf)`** — transport middleware that collapses concurrent
  in-flight identical GET/HEAD requests onto one underlying round-trip whose
  buffered response is shared among all waiters (the request-deduplication /
  funnel pattern). Pass a shared `*SingleFlight` (with `SingleFlightVary`) or
  nil for a fresh default group.
- **`WithResultCache(ttl, opts…)`** — transport middleware that memoises a
  successful (2xx) GET/HEAD response for a TTL window so repeated identical reads
  within the window skip the network. Lazy expiry (no background sweeper); a
  non-positive TTL installs a safe pass-through. `ResultCacheVary` tunes the key.
- `Middleware` (the canonical `func(http.RoundTripper) http.RoundTripper` shape),
  `WithMiddleware` (install an arbitrary transport decorator), and the public
  `SingleFlight` / `ResultCache` types with their `Wrap` middleware producers.

### Notes
- All additions are **additive and pure-stdlib**; the existing `Client`,
  `RetryWithBackoff`, auth, idempotency, breaker, and transport surfaces are
  unchanged. The new code composes with retry/backoff/breaker/budget and drops
  into any `*http.Client` via `RoundTripper()` / `StandardClient()` (Law 2).
- Added the missing GSDS Biblioteca files (`flake.nix`, `caixa.lisp`,
  `CHANGELOG.md`) and the Why / Install / Configuration / Release README anchors.

## [0.2.0] - 2026-06-03

### Added
- Resilience surface (BOREALIS §2.6): typed `Jitter` strategy (`JitterFull`
  default), server `Retry-After` honoring, a `CheckRetry` policy hook plus the
  Law-5 `Retryable()` carrier, an opt-in in-package `CircuitBreaker`, an
  idempotency-key gate, transport storm controls (`TunedTransport`), the
  `todoku/h2` HTTP/2-ping and `todoku/budget` token-bucket leaf sub-packages,
  `RoundTripper()` / `StandardClient()` adapters, and typed `Config` +
  `FromConfig`.

## [0.1.0] - 2026-06-03

### Added
- A `net/http`-backed `Client` built with functional options
  (`WithBaseURL`, `WithAuth`, `WithTimeout`, `WithRetry`, `WithHTTPClient`),
  safe for concurrent use.
- Pluggable `Auth` (`NoAuth`, `BearerAuth`, `BasicAuth`, `HeaderAuth`).
- The single generic backoff primitive `RetryWithBackoff[T]` (and
  `RetryWithBackoffClass`), with `GetJSON` / `PostJSON` typed helpers and the
  `HTTPError` / `ExhaustedError` / `NonRetryableError` error model.

[Unreleased]: https://github.com/pleme-io/todoku-go/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/pleme-io/todoku-go/releases/tag/v0.3.0
[0.2.0]: https://github.com/pleme-io/todoku-go/releases/tag/v0.2.0
[0.1.0]: https://github.com/pleme-io/todoku-go/releases/tag/v0.1.0
