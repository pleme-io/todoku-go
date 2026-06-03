package todoku

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file adds a service-reachability Probe: given a host (or a partial
// base URL), it autodetects the working scheme (https → http) and the API path
// prefix a service exposes (e.g. "/api/v2" vs "/v2"), returning a typed
// [Reachability]. Many fleet tools open by asking "can I reach the gateway, and
// what is its real base URL?" before doing real work; this collapses that
// preflight into one primitive instead of a hand-rolled probe loop per tool.
//
// It is deliberately GENERIC (WORLDS-SEPARATE): the path candidates are
// configurable and default to the two common REST conventions; the probe names
// no backend and imports nothing backend-specific. A consumer that wants a
// specific service's prefixes passes them via [ProbePaths].

// Reachability is the typed outcome of a [Probe]: whether the service was
// reachable and, if so, the scheme and API path prefix that worked, the
// assembled base URL, the status code observed, and how long the successful
// probe took. It is a plain value (golden-testable, rendered the uniform way).
type Reachability struct {
	// Reachable reports whether any candidate (scheme, path) combination
	// answered with an acceptable status.
	Reachable bool
	// Scheme is the URL scheme that worked ("https" or "http"). Empty when not
	// reachable.
	Scheme string
	// Host is the host (and optional :port) that was probed.
	Host string
	// PathPrefix is the API path prefix that answered (e.g. "/api/v2" or
	// "/v2"). Empty when not reachable or when no path candidates were probed.
	PathPrefix string
	// BaseURL is the fully-assembled base URL (scheme://host + path prefix)
	// ready to hand to [WithBaseURL]. Empty when not reachable.
	BaseURL string
	// StatusCode is the HTTP status the successful probe observed. A probe
	// counts as reachable on any status the acceptance predicate admits (by
	// default any response below 500 — the host answered), so a 401/404 still
	// proves reachability even though it is not 2xx.
	StatusCode int
	// Latency is how long the successful probe round-trip took.
	Latency time.Duration
	// Attempts is the total number of (scheme, path) combinations tried.
	Attempts int
	// Err is the last transport error seen when nothing was reachable; nil on
	// success.
	Err error
}

// DefaultProbePaths is the default set of API path prefixes a [Probe] tries, in
// order: the two prevailing REST conventions. The empty prefix ("") means
// "probe the host root", tried last so an explicit API prefix wins.
var DefaultProbePaths = []string{"/api/v2", "/v2", ""}

// ProbeConfig is the typed, yaml-tagged configuration for a [Probe] (BOREALIS
// Law 3). The zero value is usable: it probes https-then-http over
// [DefaultProbePaths] with a short per-attempt timeout.
type ProbeConfig struct {
	// Schemes is the ordered list of schemes to try. Empty → ["https","http"].
	Schemes []string `yaml:"schemes"`
	// Paths is the ordered list of API path prefixes to try. Empty →
	// [DefaultProbePaths].
	Paths []string `yaml:"paths"`
	// HealthPath is appended to each (scheme, prefix) candidate as the actual
	// endpoint hit (e.g. "/health"). Empty → the prefix itself is requested.
	HealthPath string `yaml:"healthPath"`
	// Timeout bounds EACH individual probe attempt. 0 → 3s.
	Timeout time.Duration `yaml:"timeout"`
	// Method is the HTTP method used to probe. Empty → GET.
	Method string `yaml:"method"`
}

// ProbeOption configures a probe call in the functional-options style (§3.5).
type ProbeOption func(*ProbeConfig)

// ProbeSchemes overrides the ordered list of schemes to try.
func ProbeSchemes(schemes ...string) ProbeOption {
	return func(c *ProbeConfig) { c.Schemes = schemes }
}

// ProbePaths overrides the ordered list of API path prefixes to autodetect.
func ProbePaths(paths ...string) ProbeOption {
	return func(c *ProbeConfig) { c.Paths = paths }
}

// ProbeHealthPath sets an endpoint appended to each candidate (e.g. "/health").
func ProbeHealthPath(p string) ProbeOption {
	return func(c *ProbeConfig) { c.HealthPath = p }
}

// ProbeTimeout sets the per-attempt timeout.
func ProbeTimeout(d time.Duration) ProbeOption {
	return func(c *ProbeConfig) { c.Timeout = d }
}

// ProbeMethod sets the HTTP method used for probing (default GET).
func ProbeMethod(m string) ProbeOption {
	return func(c *ProbeConfig) { c.Method = m }
}

// resolve fills a [ProbeConfig]'s defaults, returning a ready copy.
func (c ProbeConfig) resolve() ProbeConfig {
	if len(c.Schemes) == 0 {
		c.Schemes = []string{"https", "http"}
	}
	if len(c.Paths) == 0 {
		c.Paths = DefaultProbePaths
	}
	if c.Timeout == 0 {
		c.Timeout = 3 * time.Second
	}
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	return c
}

// hostOf extracts the bare host (and optional :port) from a target that may be
// a bare host, host:port, or a full URL — so the probe accepts whatever shape a
// config/flag supplies. A scheme on the input is dropped (the probe chooses the
// scheme); any path is dropped (the probe chooses the prefix).
func hostOf(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "://") {
		if u, err := url.Parse(t); err == nil && u.Host != "" {
			return u.Host
		}
	}
	// Strip any accidental path/query on a scheme-less input.
	if i := strings.IndexAny(t, "/?#"); i >= 0 {
		t = t[:i]
	}
	return t
}

// Probe autodetects how to reach target (a bare host, host:port, or URL),
// trying each configured scheme × path prefix in order and returning the first
// that answers acceptably as a typed [Reachability]. The Client's auth, retry,
// and transport tuning are applied to each probe attempt, so a probe through an
// authenticating client carries its credentials. The probe itself does NOT
// retry across candidates via the backoff loop — it walks candidates once,
// each bounded by the per-attempt timeout.
//
// Acceptance: any response with a status below 500 proves the host is reachable
// (a 401/404 still means "the service answered"); a 5xx or transport error
// moves on to the next candidate. The returned [Reachability.BaseURL] is ready
// to hand to [WithBaseURL].
func (c *Client) Probe(ctx context.Context, target string, opts ...ProbeOption) Reachability {
	cfg := ProbeConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return c.probe(ctx, target, cfg.resolve())
}

// ProbeWithConfig is the §3.5 config-consuming form: it probes using an
// already-loaded [ProbeConfig] sub-struct (it does NOT call shikumi.Load).
func (c *Client) ProbeWithConfig(ctx context.Context, target string, cfg ProbeConfig) Reachability {
	return c.probe(ctx, target, cfg.resolve())
}

// probe is the shared engine over a fully-resolved config.
func (c *Client) probe(ctx context.Context, target string, cfg ProbeConfig) Reachability {
	host := hostOf(target)
	r := Reachability{Host: host}
	if host == "" {
		r.Err = &HTTPError{Status: 0, Body: "todoku: empty probe target"}
		return r
	}

	for _, scheme := range cfg.Schemes {
		for _, prefix := range cfg.Paths {
			r.Attempts++
			base := scheme + "://" + host + prefix
			endpoint := base + cfg.HealthPath

			attemptCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			start := time.Now()
			resp, err := c.singleAttempt(attemptCtx, cfg.Method, endpoint)
			elapsed := time.Since(start)
			cancel()

			if err != nil {
				r.Err = err
				continue
			}
			status := resp.StatusCode
			drainBody(resp)
			if status >= 500 {
				// Host answered but is unhealthy; keep looking for a better
				// (scheme, prefix) before giving up.
				r.Err = &HTTPError{Status: status, Body: "probe: server error"}
				continue
			}
			// Reachable.
			return Reachability{
				Reachable:  true,
				Scheme:     scheme,
				Host:       host,
				PathPrefix: prefix,
				BaseURL:    base,
				StatusCode: status,
				Latency:    elapsed,
				Attempts:   r.Attempts,
			}
		}
	}
	return r
}

// singleAttempt performs ONE probe round-trip without engaging the retry loop
// (the probe owns its own candidate walk + per-attempt timeout). It applies the
// client's auth, User-Agent, and transport so a probe behaves like a real
// request. It returns the live response (caller drains it).
func (c *Client) singleAttempt(ctx context.Context, method, fullURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, &NonRetryableError{Err: err}
	}
	req.Header.Set("User-Agent", c.userAgent)
	c.auth.Apply(req)
	return c.httpClient.Do(req)
}

// Probe is the package-level convenience for a one-off reachability check
// without first constructing a [Client]: it builds a default client, probes,
// and returns the [Reachability]. For repeated probes or authenticated probes,
// construct a [Client] and call [Client.Probe].
func Probe(ctx context.Context, target string, opts ...ProbeOption) Reachability {
	c, _ := New()
	return c.Probe(ctx, target, opts...)
}
