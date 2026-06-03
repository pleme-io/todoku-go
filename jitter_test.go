package todoku

import (
	"testing"
	"time"
)

// fixedRand returns a deterministic RandSource yielding the given value.
type fixedRand float64

func (f fixedRand) Float64() float64 { return float64(f) }

func TestJitterFull(t *testing.T) {
	d := 100 * time.Millisecond
	// f=0 → 0; f=1 → full delay; f=0.5 → half.
	if got := applyJitter(JitterFull, d, fixedRand(0)); got != 0 {
		t.Errorf("full jitter at f=0 = %v, want 0", got)
	}
	if got := applyJitter(JitterFull, d, fixedRand(0.5)); got != 50*time.Millisecond {
		t.Errorf("full jitter at f=0.5 = %v, want 50ms", got)
	}
	// Full jitter never exceeds the computed delay across many draws.
	for i := 0; i < 1000; i++ {
		if got := applyJitter(JitterFull, d, nil); got < 0 || got > d {
			t.Fatalf("full jitter %v outside [0, %v]", got, d)
		}
	}
}

func TestJitterNone(t *testing.T) {
	d := 100 * time.Millisecond
	for i := 0; i < 100; i++ {
		if got := applyJitter(JitterNone, d, nil); got != d {
			t.Fatalf("none jitter changed delay: %v", got)
		}
	}
}

func TestJitterEqual(t *testing.T) {
	d := 100 * time.Millisecond
	if got := applyJitter(JitterEqual, d, fixedRand(0)); got != 50*time.Millisecond {
		t.Errorf("equal jitter at f=0 = %v, want 50ms (the fixed half)", got)
	}
	if got := applyJitter(JitterEqual, d, fixedRand(1)); got != 100*time.Millisecond {
		t.Errorf("equal jitter at f=1 = %v, want 100ms", got)
	}
	// Equal jitter is always in [d/2, d].
	for i := 0; i < 1000; i++ {
		if got := applyJitter(JitterEqual, d, nil); got < d/2 || got > d {
			t.Fatalf("equal jitter %v outside [%v, %v]", got, d/2, d)
		}
	}
}

func TestJitterString(t *testing.T) {
	cases := map[Jitter]string{JitterFull: "full", JitterNone: "none", JitterEqual: "equal"}
	for j, want := range cases {
		if got := j.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", j, got, want)
		}
	}
}

// The typed JitterMode supersedes the legacy band when set.
func TestRetryConfigTypedJitterWins(t *testing.T) {
	cfg := RetryConfig{Jitter: 0.2, JitterMode: JitterNone}
	if got := cfg.jitter(100 * time.Millisecond); got != 100*time.Millisecond {
		t.Errorf("JitterNone should override the legacy band, got %v", got)
	}
}

// A zero-value config (no typed mode, zero band) preserves deterministic backoff.
func TestRetryConfigLegacyZeroIsDeterministic(t *testing.T) {
	cfg := RetryConfig{}
	if got := cfg.jitter(100 * time.Millisecond); got != 100*time.Millisecond {
		t.Errorf("zero-value config should be deterministic, got %v", got)
	}
}
