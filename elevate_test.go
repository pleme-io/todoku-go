package todoku

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- Retry-After honoring ---

func TestRetryAfterSeconds(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"2"}}}
	d, ok := RetryAfter(resp, nil)
	if !ok || d != 2*time.Second {
		t.Fatalf("RetryAfter seconds = (%v,%v), want (2s,true)", d, ok)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	resp := &http.Response{Header: http.Header{"Retry-After": []string{future}}}
	d, ok := RetryAfter(resp, func() time.Time { return now })
	if !ok || d != 30*time.Second {
		t.Fatalf("RetryAfter date = (%v,%v), want (30s,true)", d, ok)
	}
	// A past date clamps to zero.
	past := now.Add(-time.Hour).Format(http.TimeFormat)
	resp2 := &http.Response{Header: http.Header{"Retry-After": []string{past}}}
	if d, ok := RetryAfter(resp2, func() time.Time { return now }); !ok || d != 0 {
		t.Fatalf("past RetryAfter = (%v,%v), want (0,true)", d, ok)
	}
}

func TestRetryAfterAbsent(t *testing.T) {
	if _, ok := RetryAfter(&http.Response{Header: http.Header{}}, nil); ok {
		t.Error("absent Retry-After should report ok=false")
	}
	if _, ok := RetryAfter(nil, nil); ok {
		t.Error("nil response should report ok=false")
	}
}

// The client honors Retry-After as the backoff for the next attempt.
func TestClientHonorsRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0") // retry immediately
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(2)))
	resp, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

// --- CheckRetry policy hook ---

func TestWithCheckRetryHardStop(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	stop := errors.New("policy stop")
	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(5)),
		WithCheckRetry(func(ctx context.Context, resp *http.Response, err error) (bool, error) {
			return false, stop // immediate hard stop
		}))
	_, err := c.Get(context.Background(), "/x")
	if !errors.Is(err, stop) {
		t.Fatalf("expected policy stop error, got %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (hard stop after first)", hits.Load())
	}
}

func TestDefaultCheckRetry(t *testing.T) {
	cr := DefaultCheckRetry([]int{503})
	ctx := context.Background()
	if retry, _ := cr(ctx, &http.Response{StatusCode: 503}, nil); !retry {
		t.Error("503 should be retryable")
	}
	if retry, _ := cr(ctx, &http.Response{StatusCode: 400}, nil); retry {
		t.Error("400 should not be retryable")
	}
	if retry, _ := cr(ctx, nil, errors.New("dial fail")); !retry {
		t.Error("transport error should be retryable")
	}
	if retry, _ := cr(ctx, nil, context.Canceled); retry {
		t.Error("context error should not be retryable")
	}
}

// --- Retryable() carrier (Law 5) ---

func TestRetryableCarrierStops(t *testing.T) {
	var calls atomic.Int32
	_, err := RetryWithBackoff(context.Background(), fastRetry(5), func(context.Context) (int, error) {
		calls.Add(1)
		return 0, &RetryableError{Err: errors.New("permanent"), Retry: false}
	})
	var nre *NonRetryableError
	if !errors.As(err, &nre) {
		t.Fatalf("carrier Retryable()=false should stop, got %T", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRetryableCarrierContinues(t *testing.T) {
	var calls atomic.Int32
	got, err := RetryWithBackoff(context.Background(), fastRetry(5), func(context.Context) (int, error) {
		if calls.Add(1) < 3 {
			return 0, &RetryableError{Err: errors.New("transient"), Retry: true}
		}
		return 7, nil
	})
	if err != nil || got != 7 {
		t.Fatalf("got (%d,%v), want (7,nil)", got, err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// A type implementing Temporary()==true is promoted to retry even with a nil
// classifier; Temporary()==false never demotes (deprecated-convention safety).
type tempErr struct{ temp bool }

func (e tempErr) Error() string   { return "temp" }
func (e tempErr) Temporary() bool { return e.temp }

func TestTemporaryCarrierPositiveOnly(t *testing.T) {
	var calls atomic.Int32
	// Temporary()==false must NOT stop the loop (connection-refused trap).
	_, err := RetryWithBackoff(context.Background(), fastRetry(2), func(context.Context) (int, error) {
		calls.Add(1)
		return 0, tempErr{temp: false}
	})
	var exh *ExhaustedError
	if !errors.As(err, &exh) {
		t.Fatalf("Temporary()=false should not demote; want exhaustion, got %T", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (still retried)", calls.Load())
	}
}

// --- Idempotency ---

func TestIsIdempotentMethod(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"} {
		if !IsIdempotentMethod(m) {
			t.Errorf("%s should be idempotent", m)
		}
	}
	for _, m := range []string{"POST", "PATCH"} {
		if IsIdempotentMethod(m) {
			t.Errorf("%s should not be idempotent", m)
		}
	}
}

// Strict idempotency: POST without a key runs exactly once.
func TestStrictIdempotencyGatesPost(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(3)), WithStrictIdempotency(true))
	_, err := c.Post(context.Background(), "/x", strings.NewReader("body"))
	if err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 1 {
		t.Errorf("strict POST hits = %d, want 1 (no retry)", hits.Load())
	}
}

// An idempotency key makes a strict-mode POST retryable and reuses the key.
func TestIdempotencyKeyEnablesPostRetry(t *testing.T) {
	var hits atomic.Int32
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(3)),
		WithStrictIdempotency(true), WithIdempotencyKeys(DefaultKeyFunc()))
	resp, err := c.Post(context.Background(), "/x", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("idempotency keys not reused across retries: %v", keys)
	}
}

func TestDefaultKeyFuncShape(t *testing.T) {
	k := DefaultKeyFunc()("POST", "https://x")
	if len(k) != 36 || strings.Count(k, "-") != 4 {
		t.Errorf("key %q is not a v4-UUID shape", k)
	}
	// Distinct logical requests get distinct keys.
	if k == DefaultKeyFunc()("POST", "https://x") {
		t.Error("expected distinct keys per call")
	}
}

// --- Transport adapters (Law 2 composition) ---

func TestStandardClientRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(WithRetry(testRetry(3)))
	std := c.StandardClient()
	resp, err := std.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2 (RoundTripper retried)", hits.Load())
	}
}

func TestRoundTripperReturnsPermanentResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := New(WithRetry(testRetry(3)))
	resp, err := c.StandardClient().Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("a non-retryable status should surface as a response, not an error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTunedTransportApplies(t *testing.T) {
	tr := TunedTransport(TransportConfig{
		MaxIdleConnsPerHost:   32,
		ResponseHeaderTimeout: 5 * time.Second,
	})
	if tr.MaxIdleConnsPerHost != 32 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 5*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 5s", tr.ResponseHeaderTimeout)
	}
}
