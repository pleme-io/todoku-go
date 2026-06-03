package todoku

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerTripsAndRejects(t *testing.T) {
	cb := NewCircuitBreaker(BreakerSettings{
		ReadyToTrip: func(c Counts) bool { return c.ConsecutiveFailures >= 3 },
		Timeout:     time.Hour, // stay open for the test
	})
	for i := 0; i < 3; i++ {
		done, err := cb.Allow()
		if err != nil {
			t.Fatalf("attempt %d unexpectedly rejected: %v", i, err)
		}
		done(false) // failure
	}
	if cb.State() != StateOpen {
		t.Fatalf("breaker state = %v, want open", cb.State())
	}
	if _, err := cb.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("open breaker should reject with ErrBreakerOpen, got %v", err)
	}
}

func TestBreakerHalfOpenRecovers(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cb := NewCircuitBreaker(BreakerSettings{
		ReadyToTrip: func(c Counts) bool { return c.ConsecutiveFailures >= 1 },
		Timeout:     10 * time.Second,
		MaxRequests: 1,
		Now:         clock,
	})
	done, _ := cb.Allow()
	done(false) // trip
	if cb.State() != StateOpen {
		t.Fatal("breaker should be open")
	}
	// Advance past the timeout → half-open.
	now = now.Add(11 * time.Second)
	if cb.State() != StateHalfOpen {
		t.Fatalf("breaker state = %v, want half-open", cb.State())
	}
	d, err := cb.Allow()
	if err != nil {
		t.Fatalf("half-open should admit one trial: %v", err)
	}
	d(true) // success closes
	if cb.State() != StateClosed {
		t.Fatalf("breaker state = %v, want closed", cb.State())
	}
}

func TestBreakerOnStateChange(t *testing.T) {
	var transitions []string
	cb := NewCircuitBreaker(BreakerSettings{
		ReadyToTrip: func(c Counts) bool { return c.ConsecutiveFailures >= 1 },
		Timeout:     time.Hour,
		OnStateChange: func(name string, from, to BreakerState) {
			transitions = append(transitions, from.String()+"->"+to.String())
		},
	})
	done, _ := cb.Allow()
	done(false)
	if len(transitions) != 1 || transitions[0] != "closed->open" {
		t.Errorf("transitions = %v, want [closed->open]", transitions)
	}
}

// An open breaker on a Client fails fast without dialing.
func TestClientCircuitBreakerOpens(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cb := NewCircuitBreaker(BreakerSettings{
		ReadyToTrip: func(c Counts) bool { return c.TotalFailures >= 2 },
		Timeout:     time.Hour,
	})
	// No retries so each Do = exactly one attempt = one breaker sample.
	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(0)), WithCircuitBreaker(cb))

	for i := 0; i < 2; i++ {
		_, _ = c.Get(context.Background(), "/x")
	}
	hitsBefore := hits.Load()
	// Breaker should now be open; next call fails fast.
	_, err := c.Get(context.Background(), "/x")
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("expected ErrBreakerOpen, got %v", err)
	}
	if hits.Load() != hitsBefore {
		t.Errorf("breaker-open call still dialed the server (hits %d -> %d)", hitsBefore, hits.Load())
	}
}

func TestBreakerStateString(t *testing.T) {
	cases := map[BreakerState]string{StateClosed: "closed", StateOpen: "open", StateHalfOpen: "half-open"}
	for s, want := range cases {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}
