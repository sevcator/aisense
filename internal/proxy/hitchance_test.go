package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/routehealth"
	"aisense/internal/store"
	"time"
)

func hitchanceProxy(t *testing.T, url string, keys []string) *Proxy {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway-test-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", BaseURL: url, Enabled: true, AuthMode: "swap", Models: []string{"model"}, APIKeys: keys}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return New(cfg, st)
}
func hitchanceRequest(p *Proxy) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	r.Header.Set("Authorization", "Bearer gateway-test-key")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	return w
}
func TestHitchanceInvalidKeyRemovedWithoutProtocolProbes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"Your key is invalid"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"bad", "good"})
	if w := hitchanceRequest(p); w.Code != 200 {
		t.Fatalf("HTTP %d", w.Code)
	}
	if len(p.Cfg.Get().Upstreams[0].APIKeys) != 1 {
		t.Fatal("invalid key retained")
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected protocol probes: %d", calls.Load())
	}
}
func TestHitchanceCoolingNeverBypassedAndExactRouteRecovery(t *testing.T) {
	p := hitchanceProxy(t, "https://one.invalid", []string{"key"})
	up := p.Cfg.Get().Upstreams[0]
	d := hitchance.Decision{RuleID: "model", Category: "model", Action: "demote", Scope: "model", Cooldown: time.Minute}
	if err := p.Cfg.ObserveHitchance(up, "key", "model", d); err != nil {
		t.Fatal(err)
	}
	up = p.Cfg.Get().Upstreams[0]
	if len(p.upstreamKeyOrder(up, "model", false)) != 0 {
		t.Fatal("cooled key/model remains eligible")
	}
	if len(p.upstreamKeyOrder(up, "other", false)) != 1 {
		t.Fatal("model failure spilled onto other model")
	}
	p.Cfg.Update(func(c *config.Config) error {
		s := c.Upstreams[0].HitchanceState[config.HitchanceTarget("model", "key", "model")]
		s.Until = time.Now().Add(-time.Second)
		c.Upstreams[0].HitchanceState[config.HitchanceTarget("model", "key", "model")] = s
		return nil
	})
	if len(p.upstreamKeyOrder(p.Cfg.Get().Upstreams[0], "model", false)) != 1 {
		t.Fatal("cooldown never recovers")
	}
}
func TestHitchanceStickyCannotOverrideHealth(t *testing.T) {
	p := hitchanceProxy(t, "https://one.invalid", []string{"key"})
	p.Cfg.Update(func(c *config.Config) error {
		c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "other", BaseURL: "https://two.invalid", Models: []string{"model"}, Enabled: true, AuthMode: "none"})
		return nil
	})
	up := p.Cfg.Get().Upstreams[0]
	p.setCachedUpstream("openai", "model", up)
	p.Health.MarkFailure(up, "model", routehealth.Global, time.Minute, "unavailable")
	got := p.candidates("openai", "model", true, nil)
	if len(got) != 2 || got[0].ID != "other" {
		t.Fatal("sticky moved unhealthy endpoint first")
	}
}
func TestHitchanceAllQuotaKeysSkipRepeatedRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(402)
		w.Write([]byte("not enough credits"))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a", "b"})
	hitchanceRequest(p)
	first := calls.Load()
	hitchanceRequest(p)
	if first != 2 || calls.Load() != first {
		t.Fatalf("quota retry storm: %d then %d", first, calls.Load())
	}
	if !p.Cfg.Get().Upstreams[0].Enabled || len(p.Cfg.Get().Upstreams[0].APIKeys) != 2 {
		t.Fatal("quota keys/endpoint removed")
	}
}
func TestHitchanceQuotaFailsOverWithoutLegacy402Filter(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		w.Write([]byte("not enough credits"))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer good.Close()
	p := hitchanceProxy(t, bad.URL, []string{"empty"})
	p.Cfg.Update(func(c *config.Config) error {
		c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "z", BaseURL: good.URL, Models: []string{"model"}, Enabled: true, AuthMode: "none", Priority: 10})
		return nil
	})
	if w := hitchanceRequest(p); w.Code != 200 {
		t.Fatalf("billing did not fail over: %d", w.Code)
	}
}
func TestHitchanceQuotaRotatesRetainsAndDemotes(t *testing.T) {
	var depleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer depleted" {
			depleted.Add(1)
			w.WriteHeader(402)
			w.Write([]byte(`{"error":{"message":"not enough credits"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"depleted", "good"})
	for i := 0; i < 2; i++ {
		if w := hitchanceRequest(p); w.Code != 200 {
			t.Fatalf("quota key blocked good key: HTTP %d", w.Code)
		}
	}
	up := p.Cfg.Get().Upstreams[0]
	if len(up.APIKeys) != 2 || up.APIKeys[1] != "depleted" {
		t.Fatal("quota key must be retained at bottom of pool")
	}
	if depleted.Load() != 1 {
		t.Fatalf("cooling key retried %d times", depleted.Load())
	}
}
