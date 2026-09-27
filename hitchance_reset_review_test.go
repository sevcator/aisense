package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aisense/internal/admin"
	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/proxy"
	"aisense/internal/store"
)

func reviewResetProxy(t *testing.T, urls ...string) (*proxy.Proxy, *admin.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.Server.Admin.Username = "review"
		c.Server.Admin.Password = "synthetic-password"
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "synthetic-client", Enabled: true}}
		c.Hitchance.Rules = []hitchance.Rule{{ID: "custom-only", StatusCodes: []int{429}, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 60}}
		c.Upstreams = nil
		for i, url := range urls {
			id := []string{"up", "other"}[i]
			c.Upstreams = append(c.Upstreams, &config.Upstream{ID: id, BaseURL: url, Enabled: true, AuthMode: "swap", APIKeys: []string{"synthetic-provider"}, Models: []string{id}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	p := proxy.New(cfg, st)
	return p, &admin.Server{Cfg: cfg, Store: st, Health: p.Health, Proxy: proxyInfoAdapter{p}}
}
func reviewForward(p *proxy.Proxy, model string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	r.Header.Set("Authorization", "Bearer synthetic-client")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	return w
}
func reviewReset(s *admin.Server, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/admin/api/hitchance/reset", strings.NewReader(`{"upstream_id":"up"}`))
	if auth {
		r.SetBasicAuth("review", "synthetic-password")
	}
	w := httptest.NewRecorder()
	s.Handle(w, r)
	return w
}
func reviewProvider(t *testing.T, calls *atomic.Int32, limited *atomic.Bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if limited.Load() {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"message":"too many requests"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestHitchanceR5AuthenticatedResetClearsClassifiedCooldown(t *testing.T) {
	var a, b atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	first := reviewProvider(t, &a, &limited)
	second := reviewProvider(t, &b, &limited)
	p, s := reviewResetProxy(t, first.URL, second.URL)
	reviewForward(p, "up")
	reviewForward(p, "other")
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("not forwarded: %d %d", a.Load(), b.Load())
	}
	if len(p.Cfg.Get().Upstreams[0].HitchanceState) != 1 {
		t.Fatal("test must exercise an explicit Hitchance cooldown")
	}
	if w := reviewReset(s, false); w.Code != 401 {
		t.Fatal("reset not authenticated")
	}
	reviewForward(p, "up")
	if a.Load() != 1 {
		t.Fatal("unauthorized reset cleared cooldown")
	}
	if w := reviewReset(s, true); w.Code != 200 {
		t.Fatalf("reset=%d %s", w.Code, w.Body.String())
	}
	limited.Store(false)
	w := reviewForward(p, "up")
	if w.Code != 200 || a.Load() != 2 {
		t.Errorf("reset not effective on next request: HTTP %d calls=%d", w.Code, a.Load())
	}
	reviewForward(p, "other")
	if b.Load() != 1 {
		t.Error("reset cleared unrelated endpoint cooldown")
	}
}

type reviewResetBarrier struct {
	proxyInfoAdapter
	reached chan *config.Upstream
	release chan struct{}
}

func (b reviewResetBarrier) ResetHitchanceKeys(up *config.Upstream) {
	b.reached <- up
	<-b.release
	if reset, ok := any(b.proxyInfoAdapter).(interface{ ResetHitchanceKeys(*config.Upstream) }); ok {
		reset.ResetHitchanceKeys(up)
	}
}
func TestHitchanceR5ResetCannotClearEditedIdentity(t *testing.T) {
	var oldCalls, newCalls atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	old := reviewProvider(t, &oldCalls, &limited)
	current := reviewProvider(t, &newCalls, &limited)
	p, s := reviewResetProxy(t, old.URL)
	reviewForward(p, "up")
	barrier := reviewResetBarrier{proxyInfoAdapter: proxyInfoAdapter{p}, reached: make(chan *config.Upstream), release: make(chan struct{})}
	s.Proxy = barrier
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- reviewReset(s, true) }()
	select {
	case snapshot := <-barrier.reached:
		if snapshot.BaseURL != old.URL {
			t.Error("reset did not capture target identity")
		}
	case <-done:
		t.Fatal("reset bypassed proxy cooldown reset callback")
	}
	// Publish a different endpoint while the old identity's reset is paused.
	err := p.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].BaseURL = current.URL; return nil })
	if err != nil {
		close(barrier.release)
		<-done
		t.Fatal(err)
	}
	reviewForward(p, "up")
	if newCalls.Load() != 1 {
		t.Error("old identity's pool blocked edited endpoint")
	}
	close(barrier.release)
	if w := <-done; w.Code != 200 {
		t.Errorf("reset=%d", w.Code)
	}
	reviewForward(p, "up")
	if newCalls.Load() != 1 {
		t.Error("stale reset cleared edited endpoint's cooldown")
	}
}
