package todoku

import "fmt"

// HTTPError is returned when a request completes but the server responds with a
// non-2xx status code that is not (or no longer) retryable. It is the Go analog
// of the Rust `TodokuError::Http` variant.
type HTTPError struct {
	// Status is the HTTP status code (e.g. 404, 500).
	Status int
	// Body is the response body, captured for diagnostics. It may be truncated
	// or empty if the body could not be read.
	Body string
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// statusError is the lightweight sentinel returned by the internal request loop
// for a retryable status code, carrying the code so [Client.Do]'s classifier can
// inspect it. It never escapes the package: a non-retryable status surfaces as
// an [*HTTPError] instead.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d (retryable): %s", e.status, e.body)
}
