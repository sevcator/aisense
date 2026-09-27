package proxy

import (
	"aisense/internal/config"
	"aisense/internal/hitchance"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHitchanceOnlyRetryAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, mode     string
		enabled, rules bool
		status         int
		message        string
		wantCalls      int
	}{
		{"unmatched-auth-no-probes", "all", true, false, 401, "rejected", 1},
		{"unmatched-endpoint-no-probes", "all", true, false, 404, "rejected", 1},
		{"disabled-auth-no-probes", "all", false, true, 401, "rejected", 1},
		{"unmatched-429-no-key-rotation", "all", true, false, 429, "rejected", 1},
		{"unmatched-model-no-alias-switch", "all", true, false, 400, "model unavailable", 1},
		{"unmatched-500-no-upstream-switch", "all", true, false, 500, "rejected", 1},
		{"disabled-no-key-rotation", "all", false, true, 429, "rate limit", 1},
		{"disabled-no-alias-switch", "all", false, true, 400, "model unavailable", 1},
		{"disabled-no-upstream-switch", "all", false, true, 500, "rejected", 1},
		{"legacy-none-does-not-block", "none", true, true, 500, "rejected", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"message":%q}}`, tc.message)
			}))
			defer server.Close()
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte(openaiReply)) }))
			defer other.Close()
			p := hitchanceProxy(t, server.URL, []string{"a", "b"})
			if err := p.Cfg.Update(func(c *config.Config) error {
				if err := json.Unmarshal([]byte(fmt.Sprintf(`{"failover":{"mode":%q}}`, tc.mode)), c); err != nil {
					return err
				}
				c.Hitchance.Enabled = tc.enabled
				if !tc.rules {
					c.Hitchance.Rules = nil
				}
				c.Upstreams[0].ModelAliases = map[string][]string{"model": {"model", "provider/model"}}
				c.Upstreams[0].Priority = -100
				c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "other", BaseURL: other.URL, Enabled: true, Models: []string{"model"}, AuthMode: "none", Priority: 1})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w := hitchanceRequest(p)
			if int(calls.Load()) != tc.wantCalls {
				t.Fatalf("calls=%d want=%d status=%d", calls.Load(), tc.wantCalls, w.Code)
			}
			if tc.wantCalls == 1 && (w.Code != tc.status || len(p.Cfg.Get().Upstreams[0].HitchanceState) != 0) {
				t.Fatalf("terminal response altered: %d states=%v", w.Code, p.Cfg.Get().Upstreams[0].HitchanceState)
			}
		})
	}
}

func TestHitchanceTransportRetryRequiresDecision(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, rules := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/rules=%v", enabled, rules), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h := w.(http.Hijacker); c, _, _ := h.Hijack(); c.Close() }))
				defer server.Close()
				var calls atomic.Int32
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte(openaiReply)) }))
				defer other.Close()
				p := hitchanceProxy(t, server.URL, []string{"a"})
				if err := p.Cfg.Update(func(c *config.Config) error {
					c.Hitchance.Enabled = enabled
					if !rules {
						c.Hitchance.Rules = []hitchance.Rule{}
					}
					c.Upstreams[0].Priority = -100
					c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "other", BaseURL: other.URL, Enabled: true, Models: []string{"model"}, AuthMode: "none"})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				w := hitchanceRequest(p)
				want := int32(0)
				if enabled && rules {
					want = 1
				}
				if calls.Load() != want {
					t.Fatalf("calls=%d want=%d status=%d", calls.Load(), want, w.Code)
				}
			})
		}
	}
}
