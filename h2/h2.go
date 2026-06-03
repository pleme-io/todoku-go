// Package h2 tunes a todoku transport's HTTP/2 health-check pings to cure the
// well-known "h2 silent dead connection" failure mode: a long-lived HTTP/2
// connection (e.g. to an akeyless Gateway behind a load balancer) that the peer
// has silently dropped, leaving the client to hang on a connection that will
// never answer. Periodic pings (ReadIdleTimeout/PingTimeout) detect the dead
// connection and force a reconnect — the AWS Builders' Library treats this
// transport-level control as co-equal with backoff.
//
// It lives in a sub-package because it carries the golang.org/x/net/http2
// dependency (BOREALIS Law 6: dep-bearing features are clearly scoped; the core
// todoku package stays zero-dep). golang.org/x/net/http2 is the ONLY way to
// reach the h2 ping knobs (http2.Transport.ReadIdleTimeout/PingTimeout) — the
// net/http transport configures HTTP/2 implicitly and does not expose them, so
// the dependency is unavoidable for this feature and Go-team-owned.
//
// Usage composes via todoku.WithTransport:
//
//	tr := todoku.TunedTransport(todoku.TransportConfig{MaxIdleConnsPerHost: 32})
//	_ = h2.Configure(tr, h2.Default())
//	cli, _ := todoku.New(todoku.WithTransport(tr))
package h2

import (
	"net/http"
	"time"

	xhttp2 "golang.org/x/net/http2"
)

// Config is the typed HTTP/2 health-check configuration (yaml-tagged).
type Config struct {
	// ReadIdleTimeout is how long an h2 connection may sit idle (no frames
	// read) before a health-check PING is sent. 0 disables pings (the stdlib
	// default — and the bug). Set it to enable dead-connection detection.
	ReadIdleTimeout time.Duration `yaml:"readIdleTimeout"`
	// PingTimeout is how long to wait for the PING ACK before declaring the
	// connection dead and closing it. 0 → x/net/http2 default (15s).
	PingTimeout time.Duration `yaml:"pingTimeout"`
}

// Default returns the recommended health-check cadence: a 30s idle ping with a
// 15s ack deadline — fast enough to catch a dead Gateway connection well within
// a typical retry window, slow enough not to add meaningful traffic.
func Default() Config {
	return Config{ReadIdleTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
}

// Configure enables HTTP/2 on the given *http.Transport (if not already) and
// installs the ping cadence from cfg. It returns the *http2.Transport it
// configured (for further tuning) or an error if HTTP/2 could not be enabled.
//
// It is idempotent-friendly: call it once after building the transport with
// todoku.TunedTransport, before installing via todoku.WithTransport.
func Configure(t *http.Transport, cfg Config) (*xhttp2.Transport, error) {
	h2t, err := xhttp2.ConfigureTransports(t)
	if err != nil {
		return nil, err
	}
	d := Default()
	if cfg.ReadIdleTimeout != 0 {
		h2t.ReadIdleTimeout = cfg.ReadIdleTimeout
	} else {
		h2t.ReadIdleTimeout = d.ReadIdleTimeout
	}
	if cfg.PingTimeout != 0 {
		h2t.PingTimeout = cfg.PingTimeout
	}
	return h2t, nil
}

// FromConfig is the §3.5 config-consuming adapter: it applies an already-loaded
// [Config] sub-struct to t. It does not call shikumi.Load.
func FromConfig(t *http.Transport, cfg Config) (*xhttp2.Transport, error) {
	return Configure(t, cfg)
}
