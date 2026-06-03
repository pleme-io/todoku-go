package todoku

import (
	"sync"
	"time"
)

// BreakerState is the circuit-breaker state, using sony/gobreaker's vocabulary
// (studied for the names, not vendored — re-expressed in-package per BOREALIS
// Law 6, well under 300 LOC).
type BreakerState int

const (
	// StateClosed is the healthy state: requests pass through and failures are
	// counted toward the trip threshold.
	StateClosed BreakerState = iota
	// StateOpen is the tripped state: requests fail fast with [ErrBreakerOpen]
	// until the open Timeout elapses.
	StateOpen
	// StateHalfOpen is the probing state after Timeout: up to MaxRequests trial
	// requests are admitted; a success closes the breaker, a failure reopens it.
	StateHalfOpen
)

// String returns the lowercase state name ("closed", "open", "half-open").
func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrBreakerOpen is returned by [CircuitBreaker.Allow] / the [Client] when the
// breaker is open (or half-open and over its trial budget) and the request is
// rejected without being attempted. It carries the Law-5 Retryable() carrier
// reporting false: an open breaker is (for this attempt) a permanent rejection.
var ErrBreakerOpen = &breakerOpenError{}

type breakerOpenError struct{}

func (*breakerOpenError) Error() string { return "todoku: circuit breaker open" }

// Retryable reports false — an open-breaker rejection is not itself retryable
// within the current call (it is the breaker's whole purpose to shed load).
func (*breakerOpenError) Retryable() bool { return false }

// Counts holds the rolling success/failure tallies the [BreakerSettings.ReadyToTrip]
// predicate inspects, mirroring gobreaker's Counts.
type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
}

func (c *Counts) onRequest() { c.Requests++ }
func (c *Counts) onSuccess() { c.TotalSuccesses++; c.ConsecutiveSuccesses++; c.ConsecutiveFailures = 0 }
func (c *Counts) onFailure() { c.TotalFailures++; c.ConsecutiveFailures++; c.ConsecutiveSuccesses = 0 }
func (c *Counts) clear()     { *c = Counts{} }

// BreakerSettings configures a [CircuitBreaker], mirroring gobreaker.Settings.
// The zero value is usable: a 5-consecutive-failure trip, a 60s open timeout,
// and one half-open trial request.
type BreakerSettings struct {
	// Name labels the breaker in [OnStateChange] callbacks.
	Name string
	// MaxRequests is the number of trial requests allowed through while
	// half-open. 0 is treated as 1.
	MaxRequests uint32
	// Interval is the cyclic period over which Closed-state counts are cleared.
	// 0 means counts are never cleared while Closed (only on a state change).
	Interval time.Duration
	// Timeout is how long the breaker stays Open before moving to HalfOpen.
	// 0 is treated as 60s.
	Timeout time.Duration
	// ReadyToTrip decides, from the current [Counts], whether a Closed breaker
	// should trip Open. A nil predicate trips after 5 consecutive failures.
	ReadyToTrip func(Counts) bool
	// OnStateChange, if non-nil, is invoked on every state transition.
	OnStateChange func(name string, from, to BreakerState)
	// Now is an injectable clock for tests; nil uses time.Now.
	Now func() time.Time
}

// CircuitBreaker is an opt-in failure detector (BOREALIS: breaker is opt-in,
// the retry budget is the default storm-control). It is safe for concurrent
// use. Wire it into a [Client] with [WithCircuitBreaker]; left unset, no
// breaker is applied.
type CircuitBreaker struct {
	mu          sync.Mutex
	name        string
	maxRequests uint32
	interval    time.Duration
	timeout     time.Duration
	readyToTrip func(Counts) bool
	onChange    func(name string, from, to BreakerState)
	now         func() time.Time

	state  BreakerState
	counts Counts
	expiry time.Time // when the current Open/Closed-interval window ends
}

// NewCircuitBreaker constructs a breaker from settings, applying the documented
// zero-value defaults.
func NewCircuitBreaker(s BreakerSettings) *CircuitBreaker {
	cb := &CircuitBreaker{
		name:        s.Name,
		maxRequests: s.MaxRequests,
		interval:    s.Interval,
		timeout:     s.Timeout,
		readyToTrip: s.ReadyToTrip,
		onChange:    s.OnStateChange,
		now:         s.Now,
	}
	if cb.maxRequests == 0 {
		cb.maxRequests = 1
	}
	if cb.timeout <= 0 {
		cb.timeout = 60 * time.Second
	}
	if cb.now == nil {
		cb.now = time.Now
	}
	if cb.readyToTrip == nil {
		cb.readyToTrip = func(c Counts) bool { return c.ConsecutiveFailures >= 5 }
	}
	cb.toClosed()
	return cb
}

// State returns the current breaker state (advancing Open→HalfOpen if the
// timeout has elapsed).
func (cb *CircuitBreaker) State() BreakerState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.evaluate(cb.now())
	return cb.state
}

// Allow reserves a slot for one request. It returns a done callback that the
// caller MUST invoke with the request outcome (success=true on a non-failure),
// or ErrBreakerOpen if the request is rejected and must not be attempted.
func (cb *CircuitBreaker) Allow() (done func(success bool), err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := cb.now()
	cb.evaluate(now)

	switch cb.state {
	case StateOpen:
		return nil, ErrBreakerOpen
	case StateHalfOpen:
		if cb.counts.Requests >= cb.maxRequests {
			return nil, ErrBreakerOpen
		}
	}
	cb.counts.onRequest()
	return func(success bool) { cb.report(success) }, nil
}

// report records an outcome and advances the state machine.
func (cb *CircuitBreaker) report(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := cb.now()
	cb.evaluate(now)
	if success {
		cb.onSuccess(now)
	} else {
		cb.onFailure(now)
	}
}

func (cb *CircuitBreaker) onSuccess(now time.Time) {
	cb.counts.onSuccess()
	if cb.state == StateHalfOpen && cb.counts.ConsecutiveSuccesses >= cb.maxRequests {
		cb.toClosed()
	}
}

func (cb *CircuitBreaker) onFailure(now time.Time) {
	cb.counts.onFailure()
	switch cb.state {
	case StateClosed:
		if cb.readyToTrip(cb.counts) {
			cb.toOpen(now)
		}
	case StateHalfOpen:
		cb.toOpen(now)
	}
}

// evaluate performs time-based transitions: Open→HalfOpen after Timeout, and
// the cyclic Closed counts clear after Interval.
func (cb *CircuitBreaker) evaluate(now time.Time) {
	switch cb.state {
	case StateOpen:
		if !cb.expiry.IsZero() && now.After(cb.expiry) {
			cb.setState(StateHalfOpen, now)
		}
	case StateClosed:
		if cb.interval > 0 && !cb.expiry.IsZero() && now.After(cb.expiry) {
			cb.counts.clear()
			cb.expiry = now.Add(cb.interval)
		}
	}
}

func (cb *CircuitBreaker) toClosed()            { cb.setState(StateClosed, cb.now()) }
func (cb *CircuitBreaker) toOpen(now time.Time) { cb.setState(StateOpen, now) }

func (cb *CircuitBreaker) setState(to BreakerState, now time.Time) {
	if cb.state == to && to != StateClosed {
		return
	}
	from := cb.state
	cb.state = to
	cb.counts.clear()
	switch to {
	case StateClosed:
		if cb.interval > 0 {
			cb.expiry = now.Add(cb.interval)
		} else {
			cb.expiry = time.Time{}
		}
	case StateOpen:
		cb.expiry = now.Add(cb.timeout)
	case StateHalfOpen:
		cb.expiry = time.Time{}
	}
	if from != to && cb.onChange != nil {
		cb.onChange(cb.name, from, to)
	}
}
