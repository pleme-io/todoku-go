package todoku

import (
	"encoding/base64"
	"net/http"
)

// Auth is a pluggable authentication strategy for HTTP requests. It mirrors the
// Rust `todoku::Auth` trait. Implementations mutate the outgoing request in
// place — almost always by setting a header — immediately before it is sent, so
// short-lived tokens are applied per attempt rather than baked in at
// construction time.
//
// Implementations should be safe for concurrent use: a single [Client] is
// shared across goroutines and calls Apply on every request.
type Auth interface {
	// Apply attaches authentication to the request (typically a header).
	Apply(*http.Request)
}

// noAuth is the zero-credential strategy: it leaves the request untouched.
type noAuth struct{}

// NoAuth returns an [Auth] that applies no authentication. It is the default
// when [WithAuth] is not supplied.
func NoAuth() Auth { return noAuth{} }

// Apply is a no-op; it leaves every header untouched.
func (noAuth) Apply(*http.Request) {}

// bearerAuth applies an RFC 6750 "Authorization: Bearer <token>" header,
// covering OAuth2 access tokens and most API keys.
type bearerAuth struct{ token string }

// BearerAuth returns an [Auth] that sets "Authorization: Bearer <token>".
func BearerAuth(token string) Auth { return bearerAuth{token: token} }

// Apply sets the Authorization header to the bearer token.
func (b bearerAuth) Apply(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+b.token)
}

// basicAuth applies an RFC 7617 "Authorization: Basic <base64(user:pass)>"
// header.
type basicAuth struct{ encoded string }

// BasicAuth returns an [Auth] that sets HTTP Basic authentication from the
// given username and password. The credentials are base64-encoded once, at
// construction time.
func BasicAuth(username, password string) Auth {
	enc := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return basicAuth{encoded: enc}
}

// Apply sets the Authorization header to the encoded basic credentials.
func (b basicAuth) Apply(r *http.Request) {
	r.Header.Set("Authorization", "Basic "+b.encoded)
}

// headerAuth applies an arbitrary header (e.g. "X-API-Key: secret") — for
// vendor-specific auth schemes that do not use the Authorization header.
type headerAuth struct {
	name  string
	value string
}

// HeaderAuth returns an [Auth] that sets a single custom header to a fixed
// value, e.g. HeaderAuth("X-API-Key", "secret").
func HeaderAuth(name, value string) Auth {
	return headerAuth{name: name, value: value}
}

// Apply sets the configured custom header.
func (h headerAuth) Apply(r *http.Request) {
	r.Header.Set(h.name, h.value)
}
