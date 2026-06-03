package todoku

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testRetry is a near-instant retry config so server tests run fast.
func testRetry(maxRetries int, statuses ...int) RetryConfig {
	if len(statuses) == 0 {
		statuses = []int{429, 500, 502, 503, 504}
	}
	return RetryConfig{
		MaxRetries:     maxRetries,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		Multiplier:     1.0,
		Jitter:         0,
		RetryStatuses:  statuses,
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{"path with leading slash", "https://api.example.com/v1", "/items", "https://api.example.com/v1/items"},
		{"path without leading slash", "https://api.example.com/v1", "items", "https://api.example.com/v1/items"},
		{"base with trailing slash", "https://api.example.com/v1/", "/items", "https://api.example.com/v1/items"},
		{"base trailing path no leading", "https://api.example.com/v1/", "items", "https://api.example.com/v1/items"},
		{"multiple trailing slashes", "https://api.example.com///", "/items", "https://api.example.com/items"},
		{"multiple leading slashes", "https://api.example.com", "///items", "https://api.example.com/items"},
		{"empty path", "https://api.example.com", "", "https://api.example.com/"},
		{"nested path", "https://api.example.com", "/a/b/c", "https://api.example.com/a/b/c"},
		{"query params preserved", "https://api.example.com/v1", "/search?q=hello&page=1", "https://api.example.com/v1/search?q=hello&page=1"},
		{"no base returns path as-is", "", "https://other.com/api", "https://other.com/api"},
		{"empty base returns path as-is", "", "/relative", "/relative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(WithBaseURL(tt.baseURL))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.resolveURL(tt.path); got != tt.want {
				t.Errorf("resolveURL(%q) with base %q = %q, want %q", tt.path, tt.baseURL, got, tt.want)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.retry.MaxRetries != 3 {
		t.Errorf("default MaxRetries = %d, want 3", c.retry.MaxRetries)
	}
	if c.httpClient == nil {
		t.Error("httpClient should be non-nil")
	}
	if !strings.Contains(c.userAgent, "todoku-go") {
		t.Errorf("userAgent = %q, want it to mention todoku-go", c.userAgent)
	}
	// Default auth is NoAuth: applies no Authorization header.
	req, _ := http.NewRequest(http.MethodGet, "https://x", nil)
	c.auth.Apply(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("default auth set Authorization = %q, want empty", got)
	}
}

// The configured Auth header is applied to outgoing requests.
func TestClientAppliesAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c, err := New(WithBaseURL(srv.URL), WithAuth(BearerAuth("secret-tok")), WithRetry(testRetry(0)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(context.Background(), "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer secret-tok" {
		t.Errorf("server saw Authorization = %q, want Bearer secret-tok", gotAuth)
	}
}

// HeaderAuth and the default User-Agent both reach the server.
func TestClientAppliesHeaderAuthAndUserAgent(t *testing.T) {
	var gotKey, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotUA = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithAuth(HeaderAuth("X-API-Key", "abc123")), WithRetry(testRetry(0)))
	resp, err := c.Get(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotKey != "abc123" {
		t.Errorf("X-API-Key = %q, want abc123", gotKey)
	}
	if !strings.Contains(gotUA, "todoku-go") {
		t.Errorf("User-Agent = %q, want it to mention todoku-go", gotUA)
	}
}

// The base URL is joined correctly to the request path on the wire.
func TestClientBaseURLJoining(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	// Base ends without slash, path begins with slash → exactly one slash.
	c, _ := New(WithBaseURL(srv.URL+"/api"), WithRetry(testRetry(0)))
	resp, err := c.Get(context.Background(), "/v1/widgets")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/api/v1/widgets" {
		t.Errorf("server path = %q, want /api/v1/widgets", gotPath)
	}
}

// Retries on 503 a couple of times, then succeeds on 200.
func TestClientRetriesOn503ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "unavailable")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(5)))
	resp, err := c.Get(context.Background(), "/flaky")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if h := hits.Load(); h != 3 {
		t.Errorf("server hits = %d, want 3", h)
	}
}

// Stops immediately on a non-retryable 400 and surfaces an *HTTPError.
func TestClientStopsOn400(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad request body")
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(5)))
	_, err := c.Get(context.Background(), "/bad")
	if err == nil {
		t.Fatal("expected an error for 400")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if he.Status != 400 {
		t.Errorf("HTTPError.Status = %d, want 400", he.Status)
	}
	if he.Body != "bad request body" {
		t.Errorf("HTTPError.Body = %q, want bad request body", he.Body)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("server hits = %d, want 1 (no retry on 400)", h)
	}
}

// When retries are exhausted on a persistent 503, the final error is an
// *HTTPError carrying the last status.
func TestClientExhaustedReturnsHTTPError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "still down")
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(2)))
	_, err := c.Get(context.Background(), "/down")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *HTTPError after exhaustion, got %T: %v", err, err)
	}
	if he.Status != 503 {
		t.Errorf("HTTPError.Status = %d, want 503", he.Status)
	}
	if h := hits.Load(); h != 3 {
		t.Errorf("server hits = %d, want 3 (MaxRetries+1)", h)
	}
}

// GetJSON decodes a successful response into a typed value.
func TestGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":7,"name":"gear"}`)
	}))
	defer srv.Close()

	type widget struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(0)))
	var w widget
	if err := GetJSON(context.Background(), c, "/widgets/7", &w); err != nil {
		t.Fatal(err)
	}
	if w.ID != 7 || w.Name != "gear" {
		t.Errorf("got %+v, want {7 gear}", w)
	}
}

// PostJSON marshals the body, sets Content-Type, and decodes the response.
func TestPostJSON(t *testing.T) {
	var gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"created":true,"id":99}`)
	}))
	defer srv.Close()

	type req struct {
		Name string `json:"name"`
	}
	type resp struct {
		Created bool `json:"created"`
		ID      int  `json:"id"`
	}
	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(0)))
	var out resp
	if err := PostJSON(context.Background(), c, "/widgets", req{Name: "bolt"}, &out); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody["name"] != "bolt" {
		t.Errorf("server got body name = %v, want bolt", gotBody["name"])
	}
	if !out.Created || out.ID != 99 {
		t.Errorf("response = %+v, want {true 99}", out)
	}
}

// Post sends the raw body and reaches the server.
func TestPostRawBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(0)))
	resp, err := c.Post(context.Background(), "/raw", strings.NewReader("payload-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotBody != "payload-bytes" {
		t.Errorf("server body = %q, want payload-bytes", gotBody)
	}
}

// The request body is replayed on each retry attempt.
func TestClientReplaysBodyOnRetry(t *testing.T) {
	var bodies []string
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if hits.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(3)))
	resp, err := c.Post(context.Background(), "/echo", strings.NewReader("same-body"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(bodies))
	}
	for i, b := range bodies {
		if b != "same-body" {
			t.Errorf("attempt %d body = %q, want same-body", i, b)
		}
	}
}

// Context cancellation aborts an in-flight client request.
func TestClientContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // block until the test cancels
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	c, _ := New(WithBaseURL(srv.URL), WithRetry(testRetry(5)))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.Get(ctx, "/slow")
	if err == nil {
		t.Fatal("expected an error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// A network error (connection refused) is retried, then surfaces after exhaustion.
func TestClientNetworkErrorRetried(t *testing.T) {
	// Start then immediately close a server to obtain a dead address.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	c, _ := New(WithBaseURL(deadURL), WithRetry(testRetry(2)))
	_, err := c.Get(context.Background(), "/x")
	if err == nil {
		t.Fatal("expected a network error")
	}
	// Should be an ExhaustedError wrapping the transport failure (not an HTTPError).
	var he *HTTPError
	if errors.As(err, &he) {
		t.Fatalf("network failure should not produce *HTTPError, got %v", err)
	}
	var exh *ExhaustedError
	if !errors.As(err, &exh) {
		t.Fatalf("expected *ExhaustedError for repeated network failure, got %T: %v", err, err)
	}
}

// WithHTTPClient is honoured (and takes precedence over WithTimeout ordering).
func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 7 * time.Second}
	c, err := New(WithTimeout(time.Second), WithHTTPClient(custom))
	if err != nil {
		t.Fatal(err)
	}
	if c.httpClient != custom {
		t.Error("WithHTTPClient should install the supplied client")
	}
	if c.httpClient.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s (from custom client)", c.httpClient.Timeout)
	}
}

// WithTimeout configures the auto-created client when no custom client is given.
func TestWithTimeout(t *testing.T) {
	c, err := New(WithTimeout(15 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.httpClient.Timeout != 15*time.Second {
		t.Errorf("timeout = %v, want 15s", c.httpClient.Timeout)
	}
}

// Options apply in order; later WithBaseURL wins.
func TestOptionOrdering(t *testing.T) {
	c, _ := New(WithBaseURL("https://first"), WithBaseURL("https://second"))
	if c.BaseURL() != "https://second" {
		t.Errorf("BaseURL = %q, want https://second", c.BaseURL())
	}
}

// HTTPError implements error with a useful message.
func TestHTTPErrorMessage(t *testing.T) {
	e := &HTTPError{Status: 404, Body: "Not Found"}
	if got := e.Error(); got != "HTTP 404: Not Found" {
		t.Errorf("Error() = %q, want HTTP 404: Not Found", got)
	}
}
