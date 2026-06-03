package h2

import (
	"net/http"
	"testing"
	"time"

	todoku "github.com/pleme-io/todoku-go"
)

func TestConfigureSetsPingKnobs(t *testing.T) {
	tr := todoku.TunedTransport(todoku.TransportConfig{MaxIdleConnsPerHost: 8})
	h2t, err := Configure(tr, Config{ReadIdleTimeout: 45 * time.Second, PingTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if h2t.ReadIdleTimeout != 45*time.Second {
		t.Errorf("ReadIdleTimeout = %v, want 45s", h2t.ReadIdleTimeout)
	}
	if h2t.PingTimeout != 10*time.Second {
		t.Errorf("PingTimeout = %v, want 10s", h2t.PingTimeout)
	}
}

func TestConfigureDefaults(t *testing.T) {
	tr := &http.Transport{}
	h2t, err := Configure(tr, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if h2t.ReadIdleTimeout != Default().ReadIdleTimeout {
		t.Errorf("ReadIdleTimeout = %v, want default %v", h2t.ReadIdleTimeout, Default().ReadIdleTimeout)
	}
}

// The h2-tuned transport composes into a todoku client via WithTransport.
func TestComposesIntoClient(t *testing.T) {
	tr := todoku.TunedTransport(todoku.TransportConfig{})
	if _, err := Configure(tr, Default()); err != nil {
		t.Fatal(err)
	}
	c, err := todoku.New(todoku.WithTransport(tr))
	if err != nil || c == nil {
		t.Fatalf("New = (%v,%v)", c, err)
	}
}

func TestFromConfig(t *testing.T) {
	tr := &http.Transport{}
	if _, err := FromConfig(tr, Default()); err != nil {
		t.Fatal(err)
	}
}
