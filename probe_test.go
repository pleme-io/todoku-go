package todoku

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostOf(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"bare host", "example.com", "example.com"},
		{"host port", "example.com:8443", "example.com:8443"},
		{"full https url", "https://example.com/api/v2", "example.com"},
		{"full http url with port", "http://example.com:8080/v2/foo", "example.com:8080"},
		{"scheme-less with path", "example.com/v2/x", "example.com"},
		{"scheme-less with query", "example.com?a=b", "example.com"},
		{"whitespace", "  example.com  ", "example.com"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostOf(tc.target); got != tc.want {
				t.Errorf("hostOf(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// The probe autodetects the path prefix: a server that only answers under
// /api/v2 is detected at that prefix, and the assembled BaseURL is correct.
func TestProbePathAutodetect(t *testing.T) {
	tests := []struct {
		name       string
		liveAt     string // the only prefix that returns 200; others 404
		wantPrefix string
	}{
		{"api/v2 prefix", "/api/v2", "/api/v2"},
		{"v2 prefix", "/v2", "/v2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.liveAt && tc.liveAt != "" {
					w.WriteHeader(http.StatusOK)
					return
				}
				// Non-live prefixes are "not this service" — answer 5xx so the
				// probe rejects them and keeps autodetecting (a 4xx would count
				// as reachable, since the host *did* answer).
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer srv.Close()

			c, _ := New()
			// Probe over http only (httptest is plaintext), default paths.
			// Restrict to the two API prefixes so the empty root candidate (which
			// the test server would 5xx) does not muddy the autodetect.
			r := c.Probe(context.Background(), srv.URL, ProbeSchemes("http"), ProbePaths("/api/v2", "/v2"))
			if !r.Reachable {
				t.Fatalf("not reachable: attempts=%d err=%v", r.Attempts, r.Err)
			}
			if r.PathPrefix != tc.wantPrefix {
				t.Errorf("PathPrefix = %q, want %q", r.PathPrefix, tc.wantPrefix)
			}
			if r.Scheme != "http" {
				t.Errorf("Scheme = %q, want http", r.Scheme)
			}
			if !strings.HasSuffix(r.BaseURL, tc.wantPrefix) {
				t.Errorf("BaseURL = %q, want suffix %q", r.BaseURL, tc.wantPrefix)
			}
			if r.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d, want 200", r.StatusCode)
			}
		})
	}
}

// A 401 (auth required) still proves reachability — the host answered.
func TestProbeReachableOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := New()
	r := c.Probe(context.Background(), srv.URL, ProbeSchemes("http"))
	if !r.Reachable {
		t.Fatalf("401 should still be reachable, err=%v", r.Err)
	}
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", r.StatusCode)
	}
	// First default prefix that answers wins: /api/v2.
	if r.PathPrefix != "/api/v2" {
		t.Errorf("PathPrefix = %q, want /api/v2 (first candidate)", r.PathPrefix)
	}
}

// A 5xx is NOT acceptable: the probe keeps walking and reports unreachable when
// every candidate is unhealthy.
func TestProbeServerErrorNotReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New()
	r := c.Probe(context.Background(), srv.URL, ProbeSchemes("http"))
	if r.Reachable {
		t.Fatal("a host returning only 5xx should not be reachable")
	}
	if r.Attempts != len(DefaultProbePaths) {
		t.Errorf("Attempts = %d, want %d (every path tried)", r.Attempts, len(DefaultProbePaths))
	}
	if r.Err == nil {
		t.Error("expected a non-nil Err on unreachable")
	}
}

func TestProbeEmptyTarget(t *testing.T) {
	c, _ := New()
	r := c.Probe(context.Background(), "   ")
	if r.Reachable || r.Err == nil {
		t.Errorf("empty target should be unreachable with an error, got %+v", r)
	}
}

// ProbeWithConfig consumes an already-loaded config (the §3.5 FromConfig-shaped
// form) and honours an explicit HealthPath + custom paths.
func TestProbeWithConfigHealthPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/svc/healthz" {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New()
	cfg := ProbeConfig{
		Schemes:    []string{"http"},
		Paths:      []string{"/svc"},
		HealthPath: "/healthz",
	}
	r := c.ProbeWithConfig(context.Background(), srv.URL, cfg)
	if !r.Reachable {
		t.Fatalf("not reachable, err=%v", r.Err)
	}
	if gotPath != "/svc/healthz" {
		t.Errorf("server saw path %q, want /svc/healthz", gotPath)
	}
	if r.PathPrefix != "/svc" {
		t.Errorf("PathPrefix = %q, want /svc", r.PathPrefix)
	}
}

// The package-level Probe convenience builds its own client.
func TestPackageProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	r := Probe(context.Background(), srv.URL, ProbeSchemes("http"))
	if !r.Reachable {
		t.Fatalf("package Probe should reach the test server, err=%v", r.Err)
	}
}
