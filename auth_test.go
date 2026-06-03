package todoku

import (
	"net/http"
	"testing"
)

func TestAuthApply(t *testing.T) {
	tests := []struct {
		name       string
		auth       Auth
		wantHeader string
		wantValue  string
		wantAbsent bool
	}{
		{
			name:       "NoAuth sets nothing",
			auth:       NoAuth(),
			wantAbsent: true,
		},
		{
			name:       "BearerAuth sets Authorization",
			auth:       BearerAuth("test-token-123"),
			wantHeader: "Authorization",
			wantValue:  "Bearer test-token-123",
		},
		{
			name:       "BearerAuth empty token",
			auth:       BearerAuth(""),
			wantHeader: "Authorization",
			wantValue:  "Bearer ",
		},
		{
			name:       "BasicAuth encodes user:pass",
			auth:       BasicAuth("user", "pass"),
			wantHeader: "Authorization",
			wantValue:  "Basic dXNlcjpwYXNz",
		},
		{
			name:       "BasicAuth empty password",
			auth:       BasicAuth("user", ""),
			wantHeader: "Authorization",
			wantValue:  "Basic dXNlcjo=",
		},
		{
			name:       "BasicAuth empty username",
			auth:       BasicAuth("", "pass"),
			wantHeader: "Authorization",
			wantValue:  "Basic OnBhc3M=",
		},
		{
			name:       "BasicAuth both empty",
			auth:       BasicAuth("", ""),
			wantHeader: "Authorization",
			wantValue:  "Basic Og==",
		},
		{
			name:       "HeaderAuth sets custom header",
			auth:       HeaderAuth("X-API-Key", "my-secret-key"),
			wantHeader: "X-API-Key",
			wantValue:  "my-secret-key",
		},
		{
			name:       "HeaderAuth empty value",
			auth:       HeaderAuth("X-API-Key", ""),
			wantHeader: "X-API-Key",
			wantValue:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.auth.Apply(req)

			if tt.wantAbsent {
				if got := req.Header.Get("Authorization"); got != "" {
					t.Errorf("expected no Authorization header, got %q", got)
				}
				return
			}
			if got := req.Header.Get(tt.wantHeader); got != tt.wantValue {
				t.Errorf("%s = %q, want %q", tt.wantHeader, got, tt.wantValue)
			}
		})
	}
}

// NoAuth leaves any pre-existing headers untouched.
func TestNoAuthPreservesHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("X-Existing", "keep-me")
	NoAuth().Apply(req)
	if got := req.Header.Get("X-Existing"); got != "keep-me" {
		t.Errorf("X-Existing = %q, want keep-me", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should be empty, got %q", got)
	}
}

// BearerAuth overwrites any existing Authorization header.
func TestBearerAuthOverwrites(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("Authorization", "Bearer old")
	BearerAuth("new").Apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer new" {
		t.Errorf("Authorization = %q, want Bearer new", got)
	}
}

// HeaderAuth does not disturb other headers.
func TestHeaderAuthIsolation(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("X-Other", "untouched")
	HeaderAuth("X-API-Key", "secret").Apply(req)
	if got := req.Header.Get("X-Other"); got != "untouched" {
		t.Errorf("X-Other = %q, want untouched", got)
	}
	if got := req.Header.Get("X-API-Key"); got != "secret" {
		t.Errorf("X-API-Key = %q, want secret", got)
	}
}

// Auth values satisfy the interface as values (no pointer required).
func TestAuthIsInterface(t *testing.T) {
	var _ Auth = NoAuth()
	var _ Auth = BearerAuth("x")
	var _ Auth = BasicAuth("u", "p")
	var _ Auth = HeaderAuth("k", "v")
}
