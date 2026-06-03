package todoku

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// idempotentMethods is the default set of HTTP methods considered idempotent
// per RFC 7231 §4.2.2: a retry of these cannot change server state beyond a
// single execution, so retrying them is always safe.
var idempotentMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodOptions: {},
	http.MethodTrace:   {},
	http.MethodPut:     {},
	http.MethodDelete:  {},
}

// IsIdempotentMethod reports whether method is idempotent by default
// (GET/HEAD/OPTIONS/TRACE/PUT/DELETE). POST and PATCH are not, unless guarded by
// an idempotency key (see [WithIdempotencyKeys]).
func IsIdempotentMethod(method string) bool {
	_, ok := idempotentMethods[method]
	return ok
}

// KeyFunc generates the value for the Idempotency-Key header given the request
// method and resolved URL. The same key is reused across every retry of one
// logical request, which is what makes retrying a non-idempotent verb safe
// (Stripe / IETF Idempotency-Key draft semantics). Return "" to skip the header
// for a given request.
type KeyFunc func(method, url string) string

// DefaultKeyFunc returns a [KeyFunc] that emits a fresh RFC-4122-shaped v4 UUID
// per logical request (computed once, reused across that request's retries). It
// uses crypto/rand — no external dependency.
func DefaultKeyFunc() KeyFunc {
	return func(method, url string) string { return newV4UUID() }
}

// newV4UUID returns a random version-4 UUID string using crypto/rand. On the
// (practically impossible) read failure it returns "" so the caller simply
// omits the header rather than panicking.
func newV4UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}
