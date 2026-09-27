package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

func TestVariantRoutingMatrixAndPolicy(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			seen := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				seen = body.Model
				_, _ = w.Write([]byte(`{"choices":[]}`))
			}))
			defer upstream.Close()
			reasoning := mask&1 != 0
			o := modelalias.VariantOptions{Reasoning: reasoning, Other: mask&2 != 0, Fast: mask&4 != 0}
			base := "deepseek-v4-flash"
			want := base
			if reasoning {
				want += "-low"
			}
			if o.Other {
				want += "-free"
			}
			if o.Fast {
				want += "-fast"
			}
			names := []string{base, "0g-" + base, base + "-low", base + "-free", base + "-fast", want}
			gateway, manager := newTestGateway(t, func(c *config.Config) {
				c.Models = config.ModelsCfg{ReasoningVariants: &reasoning, PreferOtherVariants: o.Other, FastMode: o.Fast}
				c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: names, AuthMode: "none"}}
			})
			if got := modelsListIDs(t, gateway); !slices.Equal(got, modelalias.PresentationNames(names, o)) {
				t.Fatal(got)
			}
			chat := func() {
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+base+`","reasoning_effort":"low","messages":[]}`))
				req.Header.Set("Authorization", "Bearer gateway-key")
				res := httptest.NewRecorder()
				gateway.ServeOpenAI(res, req)
				if res.Code != 200 {
					t.Fatalf("%d %s", res.Code, res.Body.String())
				}
			}
			up := manager.Get().Upstreams[0]
			gateway.rememberModelRoute(up, base, false, o.Suffixes("low"), base)
			chat()
			if seen != want {
				t.Fatalf("got %s want %s", seen, want)
			}
			if err := manager.Update(func(c *config.Config) error { c.APIKeys[0].AllowedModels = []string{base}; return nil }); err != nil {
				t.Fatal(err)
			}
			chat()
			if seen != base {
				t.Fatalf("key restriction bypassed: %s", seen)
			}
			if err := manager.Update(func(c *config.Config) error {
				c.APIKeys[0].AllowedModels = nil
				c.Upstreams[0].BlockedModels = []string{base + "-low", base + "-free", base + "-fast"}
				if want != base {
					c.Upstreams[0].BlockedModels = append(c.Upstreams[0].BlockedModels, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			chat()
			if seen != base {
				t.Fatalf("blocked variant used: %s", seen)
			}
		})
	}
}

func TestVariantsPrecedeForcedAndCachedRoutes(t *testing.T) {
	base := "claude-opus-4.6"
	gateway, manager := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "base", Enabled: true, Type: "openai", Models: []string{base}},
			{ID: "variant", Enabled: true, Type: "openai", Models: []string{base, base + "-low", base + "-high"}},
		}
	})
	for _, effort := range []string{"low", "high"} {
		boost := manager.Get().Models.VariantOptions().Suffixes(effort)
		up := manager.Get().Upstreams[1]
		gateway.rememberModelRoute(up, base, true, boost, base+"-thinking")
		gateway.setCachedUpstream("openai", base, manager.Get().Upstreams[0])
		if got := gateway.candidates("openai", base, true, boost); len(got) != 2 || got[0].ID != "variant" {
			t.Fatalf("cached base overrode preference: %v", got)
		}
		if got := gateway.combinedModelRoutes(up, base, boost); len(got) == 0 || got[0] != base+"-"+effort {
			t.Fatalf("forced alias overrode preference: %v", got)
		}
	}
}
