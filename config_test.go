package todoku

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestFromConfigBasics(t *testing.T) {
	honor := false
	cfg := Config{
		BaseURL:   "https://api.example.com/v1",
		UserAgent: "test-agent",
		Timeout:   7 * time.Second,
		Retry: RetryConfigSpec{
			MaxRetries: 5,
			Jitter:     "equal",
		},
		HonorRetryAfter: &honor,
		Transport: TransportConfigSpec{
			MaxIdleConnsPerHost: 16,
		},
		Breaker: &BreakerConfigSpec{ConsecutiveFailures: 4, Timeout: time.Minute},
	}
	c, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != cfg.BaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL(), cfg.BaseURL)
	}
	if c.userAgent != "test-agent" {
		t.Errorf("userAgent = %q", c.userAgent)
	}
	if c.retry.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", c.retry.MaxRetries)
	}
	if c.retry.JitterMode != JitterEqual {
		t.Errorf("JitterMode = %v, want equal", c.retry.JitterMode)
	}
	if c.honorRetryAfter {
		t.Error("HonorRetryAfter should be false")
	}
	if c.breaker == nil {
		t.Error("breaker should be configured")
	}
}

// FromConfig defaults yield a working client; caller opts compose on top.
func TestFromConfigDefaultsAndOptCompose(t *testing.T) {
	c, err := FromConfig(Config{}, WithUserAgent("override"))
	if err != nil {
		t.Fatal(err)
	}
	if c.userAgent != "override" {
		t.Errorf("caller option should win: userAgent = %q", c.userAgent)
	}
	// Default retry preset is applied.
	if c.retry.MaxRetries != DefaultRetry().MaxRetries {
		t.Errorf("MaxRetries = %d, want default %d", c.retry.MaxRetries, DefaultRetry().MaxRetries)
	}
}

// A config-built client actually retries against a server.
func TestFromConfigClientWorks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := FromConfig(Config{
		BaseURL: srv.URL,
		Retry:   RetryConfigSpec{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Multiplier: 1.0, Jitter: "none"},
	})
	resp, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}
