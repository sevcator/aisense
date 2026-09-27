package proxy

import (
	"aisense/internal/config"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHitchanceDisabledIgnoresAllRuntimeCooldowns(t *testing.T) {
	p := hitchanceProxy(t, "http://synthetic.invalid", []string{"a", "b"})
	up := p.Cfg.Get().Upstreams[0]
	p.markUpstreamKeyFailure(up, "a", 429, http.Header{})
	if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	got := p.upstreamKeyOrder(p.Cfg.Get().Upstreams[0], "model", false)
	if len(got) != 2 || got[0] != "a" {
		t.Fatalf("disabled policy still reorders cooling keys: %v", got)
	}
}
func TestHitchanceHTTPSRepairRequiresDecision(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := hitchanceProxy(t, "http://synthetic.invalid", []string{"a"})
		if err := p.Cfg.Update(func(c *config.Config) error {
			c.Hitchance.Enabled = enabled
			c.Hitchance.Rules = nil
			c.ModelDiscovery.AutoFixProblems = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		up := p.Cfg.Get().Upstreams[0]
		original := &forwardResult{status: 400, body: []byte("The plain HTTP request was sent to HTTPS port")}
		calls := 0
		p.autofixHTTPS(up, "/v1", httptest.NewRequest("POST", "/v1/chat/completions", nil), []byte(`{"model":"model"}`), original, func(string) (*http.Response, error) { calls++; return nil, errors.New("synthetic failure") })
		if calls != 0 || p.Cfg.Get().Upstreams[0].BaseURL != up.BaseURL {
			t.Fatalf("unclassified failure repaired/retried: enabled=%v calls=%d", enabled, calls)
		}
	}
}
func TestHitchanceDelegatedAuthReportsEffectiveScope(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer s.Close()
	p := hitchanceProxy(t, s.URL, nil)
	if err := p.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].AuthMode = "none"; return nil }); err != nil {
		t.Fatal(err)
	}
	fr, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", httptest.NewRequest("POST", "/v1/chat/completions", nil), []byte(`{"model":"model"}`), &bytes.Buffer{}, httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	if fr.hitchance.Scope != "endpoint" {
		t.Fatalf("runtime decision disagrees with persisted scope: %+v", fr.hitchance)
	}
}
