package budget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	todoku "github.com/pleme-io/todoku-go"
)

// Compile-time proof the token bucket satisfies the core RetryBudget interface.
var _ todoku.RetryBudget = (*TokenBucket)(nil)

func TestTokenBucketAllows(t *testing.T) {
	b := NewTokenBucket(Config{RetriesPerSecond: 1, Burst: 2})
	if !b.AllowRetry() || !b.AllowRetry() {
		t.Fatal("first two retries should be allowed (burst=2)")
	}
	if b.AllowRetry() {
		t.Fatal("third immediate retry should be denied (bucket empty)")
	}
}

func TestTokenBucketDefaults(t *testing.T) {
	b := NewTokenBucket(Config{}) // zero → Default()
	if got := b.Tokens(); got < 1 {
		t.Errorf("default bucket should start with tokens, got %v", got)
	}
}

func TestFromConfig(t *testing.T) {
	b, err := FromConfig(Config{RetriesPerSecond: 3, Burst: 5})
	if err != nil || b == nil {
		t.Fatalf("FromConfig = (%v,%v)", b, err)
	}
}

// An exhausted budget stops the client's retry loop with ErrBudgetExhausted.
func TestBudgetStopsClientRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Burst of 1: the first retry is allowed, the second denied → loop stops.
	b := NewTokenBucket(Config{RetriesPerSecond: 0.0001, Burst: 1})
	c, _ := todoku.New(
		todoku.WithBaseURL(srv.URL),
		todoku.WithRetry(todoku.RetryConfig{
			MaxRetries:     5,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     2 * time.Millisecond,
			Multiplier:     1.0,
			RetryStatuses:  []int{503},
		}),
		todoku.WithRetryBudget(b),
	)
	_, err := c.Get(context.Background(), "/x")
	if !errors.Is(err, todoku.ErrBudgetExhausted) {
		t.Fatalf("expected ErrBudgetExhausted, got %v", err)
	}
	// attempt 0 + 1 budgeted retry = 2 server hits, then budget denied.
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (one retry then budget exhausted)", hits.Load())
	}
}
