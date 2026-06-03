package todoku

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry returns a config with negligible, deterministic backoff for tests.
func fastRetry(maxRetries int) RetryConfig {
	return RetryConfig{
		MaxRetries:     maxRetries,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Multiplier:     1.0,
		Jitter:         0,
	}
}

func TestBackoffFor(t *testing.T) {
	cfg := RetryConfig{
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2.0,
	}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 500 * time.Millisecond},
		{1, 1000 * time.Millisecond},
		{2, 2000 * time.Millisecond},
		{3, 4000 * time.Millisecond},
		{-1, 500 * time.Millisecond}, // negative treated as 0
	}
	for _, tt := range tests {
		if got := cfg.BackoffFor(tt.attempt); got != tt.want {
			t.Errorf("BackoffFor(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffClampedToMax(t *testing.T) {
	cfg := RetryConfig{
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     time.Second,
		Multiplier:     2.0,
	}
	if got := cfg.BackoffFor(10); got != time.Second {
		t.Errorf("BackoffFor(10) = %v, want 1s (clamped)", got)
	}
	if got := cfg.BackoffFor(100); got != time.Second {
		t.Errorf("BackoffFor(100) = %v, want 1s (no overflow)", got)
	}
}

func TestShouldRetryStatus(t *testing.T) {
	cfg := DefaultRetry()
	retryable := []int{429, 500, 502, 503, 504}
	for _, s := range retryable {
		if !cfg.ShouldRetryStatus(s) {
			t.Errorf("ShouldRetryStatus(%d) = false, want true", s)
		}
	}
	notRetryable := []int{200, 201, 301, 400, 401, 403, 404, 409, 422}
	for _, s := range notRetryable {
		if cfg.ShouldRetryStatus(s) {
			t.Errorf("ShouldRetryStatus(%d) = true, want false", s)
		}
	}
}

func TestRetryPresets(t *testing.T) {
	if got := NoRetry().MaxRetries; got != 0 {
		t.Errorf("NoRetry MaxRetries = %d, want 0", got)
	}
	if got := DefaultRetry().MaxRetries; got != 3 {
		t.Errorf("DefaultRetry MaxRetries = %d, want 3", got)
	}
	if got := AggressiveRetry().MaxRetries; got != 5 {
		t.Errorf("AggressiveRetry MaxRetries = %d, want 5", got)
	}
	if got := AggressiveRetry().InitialBackoff; got != 200*time.Millisecond {
		t.Errorf("AggressiveRetry InitialBackoff = %v, want 200ms", got)
	}
	// NoRetry keeps the default retry-status set.
	if !NoRetry().ShouldRetryStatus(503) {
		t.Error("NoRetry should retain default retry statuses")
	}
}

// RetryWithBackoff returns a typed value on first success.
func TestRetryWithBackoffSuccess(t *testing.T) {
	got, err := RetryWithBackoff(context.Background(), fastRetry(3), func(context.Context) (string, error) {
		return "hello", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

// RetryWithBackoff is generic: it returns whatever T the operation produces.
func TestRetryWithBackoffTypedValue(t *testing.T) {
	type widget struct {
		ID   int
		Name string
	}
	got, err := RetryWithBackoff(context.Background(), fastRetry(2), func(context.Context) (widget, error) {
		return widget{ID: 7, Name: "gear"}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 7 || got.Name != "gear" {
		t.Errorf("got %+v, want {7 gear}", got)
	}

	// And with a pointer element type.
	gotPtr, err := RetryWithBackoff(context.Background(), fastRetry(2), func(context.Context) (*widget, error) {
		return &widget{ID: 9}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPtr == nil || gotPtr.ID != 9 {
		t.Errorf("got %v, want &{9 }", gotPtr)
	}
}

// RetryWithBackoff retries transient failures then succeeds.
func TestRetryWithBackoffRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	got, err := RetryWithBackoff(context.Background(), fastRetry(5), func(context.Context) (int, error) {
		if calls.Add(1) < 3 {
			return 0, errors.New("transient")
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	if c := calls.Load(); c != 3 {
		t.Errorf("calls = %d, want 3", c)
	}
}

// When all attempts fail, RetryWithBackoff returns ExhaustedError with the count.
func TestRetryWithBackoffExhausted(t *testing.T) {
	var calls atomic.Int32
	sentinel := errors.New("always fails")
	_, err := RetryWithBackoff(context.Background(), fastRetry(2), func(context.Context) (int, error) {
		calls.Add(1)
		return 0, sentinel
	})
	var exh *ExhaustedError
	if !errors.As(err, &exh) {
		t.Fatalf("expected *ExhaustedError, got %T: %v", err, err)
	}
	if exh.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", exh.Attempts)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("ExhaustedError should wrap the final error")
	}
	if c := calls.Load(); c != 3 {
		t.Errorf("calls = %d, want 3 (MaxRetries+1)", c)
	}
}

// MaxRetries of 0 runs the operation exactly once.
func TestRetryWithBackoffZeroRetries(t *testing.T) {
	var calls atomic.Int32
	_, err := RetryWithBackoff(context.Background(), fastRetry(0), func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("nope")
	})
	if c := calls.Load(); c != 1 {
		t.Errorf("calls = %d, want 1", c)
	}
	var exh *ExhaustedError
	if !errors.As(err, &exh) || exh.Attempts != 1 {
		t.Errorf("expected ExhaustedError attempts=1, got %v", err)
	}
}

// A classifier returning false short-circuits with NonRetryableError.
func TestRetryWithBackoffClassNonRetryable(t *testing.T) {
	var calls atomic.Int32
	fatal := errors.New("fatal")
	_, err := RetryWithBackoffClass(context.Background(), fastRetry(5),
		func(error) bool { return false },
		func(context.Context) (int, error) {
			calls.Add(1)
			return 0, fatal
		})
	var nre *NonRetryableError
	if !errors.As(err, &nre) {
		t.Fatalf("expected *NonRetryableError, got %T: %v", err, err)
	}
	if !errors.Is(err, fatal) {
		t.Error("NonRetryableError should wrap the cause")
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("calls = %d, want 1 (bailed immediately)", c)
	}
}

// The classifier can inspect the error to distinguish transient from permanent.
func TestRetryWithBackoffClassInspects(t *testing.T) {
	var calls atomic.Int32
	transient := errors.New("transient")
	fatal := errors.New("fatal")
	_, err := RetryWithBackoffClass(context.Background(), fastRetry(5),
		func(e error) bool { return errors.Is(e, transient) },
		func(context.Context) (int, error) {
			if calls.Add(1) == 1 {
				return 0, transient
			}
			return 0, fatal
		})
	var nre *NonRetryableError
	if !errors.As(err, &nre) {
		t.Fatalf("expected *NonRetryableError, got %v", err)
	}
	if c := calls.Load(); c != 2 {
		t.Errorf("calls = %d, want 2 (retry transient, bail on fatal)", c)
	}
}

// Context cancellation aborts the retry loop during backoff.
func TestRetryWithBackoffContextCancel(t *testing.T) {
	cfg := RetryConfig{
		MaxRetries:     10,
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     time.Second,
		Multiplier:     2.0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	go func() {
		// Cancel shortly after the first attempt fails and we enter backoff.
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := RetryWithBackoff(ctx, cfg, func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("boom")
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Should abort well before the full 50ms+ backoff sequence completes,
	// and certainly before all 11 attempts run.
	if c := calls.Load(); c > 2 {
		t.Errorf("calls = %d, want <= 2 (aborted during backoff)", c)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected quick abort", elapsed)
	}
}

// A context already cancelled before the first attempt returns immediately.
func TestRetryWithBackoffPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	_, err := RetryWithBackoff(ctx, fastRetry(3), func(context.Context) (int, error) {
		calls.Add(1)
		return 0, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if c := calls.Load(); c != 0 {
		t.Errorf("calls = %d, want 0 (never ran)", c)
	}
}

// Jitter keeps the delay within the configured band and never goes negative.
func TestJitterBand(t *testing.T) {
	cfg := RetryConfig{Jitter: 0.2}
	base := 100 * time.Millisecond
	for i := 0; i < 1000; i++ {
		d := cfg.jitter(base)
		if d < 80*time.Millisecond || d > 120*time.Millisecond {
			t.Fatalf("jittered delay %v outside [80ms,120ms]", d)
			break
		}
	}
	// Zero jitter is deterministic.
	if got := (RetryConfig{Jitter: 0}).jitter(base); got != base {
		t.Errorf("zero jitter changed delay: %v", got)
	}
}
