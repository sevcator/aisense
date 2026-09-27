package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

func TestHitchanceAliasRetryRefreshesCooledKeys(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		raw, _ := body["model"].(string)
		mu.Lock()
		calls = append(calls, key+"/"+raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if key == "a" {
			w.WriteHeader(402)
			w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"model unavailable"}}`))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a", "b"})
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Upstreams[0].ModelAliases = map[string][]string{"model": {"model", "provider/model"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := hitchanceRequest(p); w.Code != 503 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(calls, ","); got != "a/model,b/model,b/provider/model" {
		t.Fatalf("stale alias retry: %s", got)
	}
}

func TestHitchanceDisabledPreservesStateButIgnoresCooldowns(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer a" {
			t.Error("credential changed")
		}
		w.Write([]byte(openaiReply))
	}))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"a"})
	if err := p.Cfg.ObserveHitchance(p.Cfg.Get().Upstreams[0], "a", "model", hitchance.Decision{Action: "quarantine", Scope: "endpoint", Category: "test", RuleID: "q", Cooldown: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.markUpstreamKeyFailure(p.Cfg.Get().Upstreams[0], "a", 429, http.Header{})
	if w := hitchanceRequest(p); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("legacy fallback lost: HTTP %d calls=%d", w.Code, calls.Load())
	}
	if len(p.Cfg.Get().Upstreams[0].HitchanceState) != 1 {
		t.Fatal("disabled policy discarded retained state")
	}
}

func TestHitchanceQueuedKeyRevalidatedBeforeSend(t *testing.T) {
	for _, change := range []string{"delete-key", "cool-key", "quarantine-key", "cool-endpoint", "runtime-cool-key", "disable", "remove", "url", "auth", "proxy", "blocked-model", "disabled-policy-delete-key"} {
		t.Run(change, func(t *testing.T) {
			var oldCalls, editedCalls atomic.Int32
			edited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { editedCalls.Add(1); w.Write([]byte(openaiReply)) }))
			defer edited.Close()
			var p *Proxy
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if oldCalls.Add(1) == 1 {
					var err error
					up := p.Cfg.Get().Upstreams[0]
					switch change {
					case "runtime-cool-key":
						p.markUpstreamKeyFailure(up, "b", 429, http.Header{})
					case "disabled-policy-delete-key":
						err = p.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].APIKeys = []string{"a"}; return nil })
					case "delete-key", "cool-key", "quarantine-key", "cool-endpoint":
						action, scope := "demote", "key"
						if change == "delete-key" {
							action = "delete"
						}
						if change == "quarantine-key" {
							action = "quarantine"
						}
						if change == "cool-endpoint" {
							scope = "endpoint"
						}
						err = p.Cfg.ObserveHitchance(up, "b", "model", hitchance.Decision{Action: action, Scope: scope, Category: "test", RuleID: "concurrent", Cooldown: time.Hour})
					default:
						err = p.Cfg.Update(func(c *config.Config) error {
							switch change {
							case "disable":
								c.Upstreams[0].Enabled = false
							case "remove":
								c.Upstreams = nil
							case "url":
								c.Upstreams[0].BaseURL = edited.URL
							case "auth":
								c.Upstreams[0].AuthMode = "none"
							case "proxy":
								c.Upstreams[0].UseProxy = true
							case "blocked-model":
								c.Upstreams[0].KeyBlockedModels = map[string][]string{config.KeyFingerprint("b"): {"model"}}
							}
							return nil
						})
					}
					if err != nil {
						t.Error(err)
					}
					w.WriteHeader(429)
					w.Write([]byte(`{"error":{"message":"rate limit"}}`))
					return
				}
				w.Write([]byte(openaiReply))
			}))
			defer server.Close()
			p = hitchanceProxy(t, server.URL, []string{"a", "b"})
			if change == "disabled-policy-delete-key" {
				if err := p.Cfg.Update(func(c *config.Config) error { c.Hitchance.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			_, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", httptest.NewRequest("POST", "/v1/chat/completions", nil), []byte(`{"model":"model","messages":[]}`), &bytes.Buffer{}, httptest.NewRecorder())
			if err != nil {
				t.Fatal(err)
			}
			if oldCalls.Load() != 1 || editedCalls.Load() != 0 {
				t.Fatalf("queued key sent after %s: old=%d edited=%d", change, oldCalls.Load(), editedCalls.Load())
			}
		})
	}
}
