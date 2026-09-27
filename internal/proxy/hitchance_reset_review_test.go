package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

func TestHitchanceR5LateLegacyFailureCannotEditNewIdentity(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer old.Close()
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "600"); w.WriteHeader(429) }))
	defer current.Close()
	p := hitchanceProxy(t, old.URL, []string{"key"})
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.Rules = []hitchance.Rule{{ID: "custom", Messages: []string{"special custom quota"}, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 60}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	request := func() error {
		_, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", httptest.NewRequest("POST", "/v1/chat/completions", nil), []byte(`{"model":"model","messages":[]}`), &bytes.Buffer{}, httptest.NewRecorder())
		return err
	}
	done := make(chan error, 1)
	go func() { done <- request() }()
	<-reached
	if err := p.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].BaseURL = current.URL; return nil }); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	if err := request(); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	expiry := func() time.Time {
		p.upstreamKeyMu.Lock()
		defer p.upstreamKeyMu.Unlock()
		return p.upstreamKeys["up"].cooldown["key"]
	}
	p.markUpstreamKeyFailure(p.Cfg.Get().Upstreams[0], "key", 429, http.Header{"Retry-After": []string{"600"}})
	before := expiry()
	if before.IsZero() {
		t.Fatal("runtime cooldown not seeded")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if after := expiry(); after != before {
		t.Fatalf("old endpoint changed current cooldown: %v -> %v", before, after)
	}
}
