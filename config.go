package todoku

import "time"

// Config is the typed, yaml-tagged configuration surface for a [Client]
// (BOREALIS Law 3: every knob a typed yaml struct). It is the already-loaded
// sub-struct a caller obtains from shikumi at main, then hands to [FromConfig].
// Construction NEVER loads config itself — that is shikumi's exclusive job.
//
// Secrets (auth tokens) are deliberately absent: a live credential lives only
// in an [Auth] passed via [WithAuth], never in config (CFG-09). Wire auth as an
// option alongside [FromConfig] when needed.
type Config struct {
	// BaseURL is joined to relative request paths.
	BaseURL string `yaml:"baseURL"`
	// UserAgent overrides the default User-Agent header.
	UserAgent string `yaml:"userAgent"`
	// Timeout is the per-request timeout. 0 → 30s default.
	Timeout time.Duration `yaml:"timeout"`
	// Retry is the retry/backoff policy.
	Retry RetryConfigSpec `yaml:"retry"`
	// Transport is the transport-level storm-control tuning.
	Transport TransportConfigSpec `yaml:"transport"`
	// HonorRetryAfter toggles Retry-After header honouring (default true; the
	// pointer distinguishes "unset" from "explicitly false").
	HonorRetryAfter *bool `yaml:"honorRetryAfter"`
	// AllowNonIdempotentRetry permits retrying POST/PATCH without a key (only
	// meaningful together with StrictIdempotency).
	AllowNonIdempotentRetry bool `yaml:"allowNonIdempotentRetry"`
	// StrictIdempotency opts into the idempotency safety gate (off by default,
	// preserving the historical retry-everything behaviour).
	StrictIdempotency bool `yaml:"strictIdempotency"`
	// IdempotencyKeys, when true, injects a generated Idempotency-Key per
	// request, making non-idempotent retries safe.
	IdempotencyKeys bool `yaml:"idempotencyKeys"`
	// Breaker, when non-nil, configures the opt-in circuit breaker.
	Breaker *BreakerConfigSpec `yaml:"breaker"`
}

// RetryConfigSpec is the yaml-friendly projection of [RetryConfig]. It maps onto
// the runtime struct via [RetryConfigSpec.toRetryConfig], applying
// [DefaultRetry] for zero fields so a partial yaml block stays sane.
type RetryConfigSpec struct {
	MaxRetries     int           `yaml:"maxRetries"`
	InitialBackoff time.Duration `yaml:"initialBackoff"`
	MaxBackoff     time.Duration `yaml:"maxBackoff"`
	Multiplier     float64       `yaml:"multiplier"`
	// Jitter selects the typed strategy: "full" (default), "none", or "equal".
	Jitter        string `yaml:"jitter"`
	RetryStatuses []int  `yaml:"retryStatuses"`
}

// toRetryConfig materialises a [RetryConfig], starting from [DefaultRetry] and
// overlaying any non-zero spec field.
func (s RetryConfigSpec) toRetryConfig() RetryConfig {
	cfg := DefaultRetry()
	if s.MaxRetries != 0 {
		cfg.MaxRetries = s.MaxRetries
	}
	if s.InitialBackoff != 0 {
		cfg.InitialBackoff = s.InitialBackoff
	}
	if s.MaxBackoff != 0 {
		cfg.MaxBackoff = s.MaxBackoff
	}
	if s.Multiplier != 0 {
		cfg.Multiplier = s.Multiplier
	}
	if len(s.RetryStatuses) != 0 {
		cfg.RetryStatuses = s.RetryStatuses
	}
	switch s.Jitter {
	case "none":
		cfg.JitterMode = JitterNone
	case "equal":
		cfg.JitterMode = JitterEqual
	default:
		cfg.JitterMode = JitterFull
	}
	// The typed enum supersedes the legacy fraction.
	cfg.Jitter = 0
	return cfg
}

// TransportConfigSpec is the yaml projection of [TransportConfig].
type TransportConfigSpec struct {
	MaxIdleConns          int           `yaml:"maxIdleConns"`
	MaxIdleConnsPerHost   int           `yaml:"maxIdleConnsPerHost"`
	MaxConnsPerHost       int           `yaml:"maxConnsPerHost"`
	IdleConnTimeout       time.Duration `yaml:"idleConnTimeout"`
	ResponseHeaderTimeout time.Duration `yaml:"responseHeaderTimeout"`
	TLSHandshakeTimeout   time.Duration `yaml:"tlsHandshakeTimeout"`
	ExpectContinueTimeout time.Duration `yaml:"expectContinueTimeout"`
}

func (s TransportConfigSpec) toTransportConfig() TransportConfig {
	return TransportConfig{
		MaxIdleConns:          s.MaxIdleConns,
		MaxIdleConnsPerHost:   s.MaxIdleConnsPerHost,
		MaxConnsPerHost:       s.MaxConnsPerHost,
		IdleConnTimeout:       s.IdleConnTimeout,
		ResponseHeaderTimeout: s.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   s.TLSHandshakeTimeout,
		ExpectContinueTimeout: s.ExpectContinueTimeout,
	}
}

// isZero reports whether the transport spec is entirely default (so we leave
// the client's transport untouched).
func (s TransportConfigSpec) isZero() bool {
	return s == TransportConfigSpec{}
}

// BreakerConfigSpec is the yaml projection of [BreakerSettings] (the trip
// thresholds and timings; the predicate/callback hooks are wired in code, not
// yaml).
type BreakerConfigSpec struct {
	Name        string        `yaml:"name"`
	MaxRequests uint32        `yaml:"maxRequests"`
	Interval    time.Duration `yaml:"interval"`
	Timeout     time.Duration `yaml:"timeout"`
	// ConsecutiveFailures is the trip threshold; 0 → breaker default (5).
	ConsecutiveFailures uint32 `yaml:"consecutiveFailures"`
}

func (s BreakerConfigSpec) toSettings() BreakerSettings {
	st := BreakerSettings{
		Name:        s.Name,
		MaxRequests: s.MaxRequests,
		Interval:    s.Interval,
		Timeout:     s.Timeout,
	}
	if s.ConsecutiveFailures > 0 {
		threshold := s.ConsecutiveFailures
		st.ReadyToTrip = func(c Counts) bool { return c.ConsecutiveFailures >= threshold }
	}
	return st
}

// FromConfig constructs a [Client] from an already-loaded [Config] sub-struct
// (BOREALIS §3.5). It MUST NOT call shikumi.Load — the caller loads config once
// at main and hands the sub-struct here. Extra options (auth, a custom
// transport, a retry budget) compose on top via opts and win over config.
func FromConfig(cfg Config, opts ...Option) (*Client, error) {
	base := []Option{
		WithRetry(cfg.Retry.toRetryConfig()),
	}
	if cfg.BaseURL != "" {
		base = append(base, WithBaseURL(cfg.BaseURL))
	}
	if cfg.UserAgent != "" {
		base = append(base, WithUserAgent(cfg.UserAgent))
	}
	if cfg.Timeout != 0 {
		base = append(base, WithTimeout(cfg.Timeout))
	}
	if cfg.HonorRetryAfter != nil {
		base = append(base, WithRetryAfter(*cfg.HonorRetryAfter))
	}
	if cfg.AllowNonIdempotentRetry {
		base = append(base, WithAllowNonIdempotentRetry(true))
	}
	if cfg.StrictIdempotency {
		base = append(base, WithStrictIdempotency(true))
	}
	if cfg.IdempotencyKeys {
		base = append(base, WithIdempotencyKeys(DefaultKeyFunc()))
	}
	if !cfg.Transport.isZero() {
		base = append(base, WithTransport(TunedTransport(cfg.Transport.toTransportConfig())))
	}
	if cfg.Breaker != nil {
		base = append(base, WithCircuitBreaker(NewCircuitBreaker(cfg.Breaker.toSettings())))
	}
	// Caller-supplied options compose last so they win on conflict.
	return New(append(base, opts...)...)
}
