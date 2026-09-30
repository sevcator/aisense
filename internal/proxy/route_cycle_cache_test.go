package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/store"
)

func TestRetryCyclePlacesRecent429Last(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"temporarily rejected"}}`))
		}))
	}
	rate := record("rate", 429)
	defer rate.Close()
	down := record("down", 503)
	defer down.Close()
	p := hitchanceProxy(t, rate.URL, []string{"rate-key"})
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 2
		c.Hitchance.CoolingRetrySeconds = 3
		c.Hitchance.Rules = []hitchance.Rule{
			{ID: "down", StatusCodes: []int{503}, Category: "outage", Action: "demote", Scope: "endpoint", CooldownSeconds: 1},
			{ID: "rate", StatusCodes: []int{429}, Category: "rate_limit", Action: "demote", Scope: "endpoint", CooldownSeconds: 1},
		}
		c.Upstreams[0].ID = "rate"
		c.Upstreams[0].Priority = 0
		c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "down", BaseURL: down.URL, Enabled: true, AuthMode: "swap", Models: []string{"model"}, APIKeys: []string{"down-key"}, Priority: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := hitchanceRequest(p).Code; got != 503 {
		t.Fatalf("status=%d", got)
	}
	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if want := []string{"rate", "down", "down", "rate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attempt order=%v, want %v", got, want)
	}
}

func TestTemporary429KeepsLastSuccessfulCachedUpstream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Write([]byte(openaiReply))
			return
		}
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"too many requests"}}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"key"})
	if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.CoolingRetrySeconds = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := hitchanceRequest(p).Code; got != 200 {
		t.Fatalf("first status=%d", got)
	}
	if got := hitchanceRequest(p).Code; got != 503 {
		t.Fatalf("second status=%d", got)
	}
	entries := p.CachedUpstreams()
	if len(entries) != 1 || entries[0].UpstreamID != "up" || entries[0].Model != "model" {
		t.Fatalf("cached upstream after 429: %+v", entries)
	}
}

func TestCachedUpstreamsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "working", BaseURL: "https://api.example.com", Enabled: true, AuthMode: "swap", Models: []string{"model"}, APIKeys: []string{"key"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(cfg, st)
	p.setCachedUpstream("openai", "model", cfg.Get().Upstreams[0], "")
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := New(reloaded, st).CachedUpstreams()
	if len(entries) != 1 || entries[0].UpstreamID != "working" {
		t.Fatalf("cached routes after restart: %+v", entries)
	}
}
