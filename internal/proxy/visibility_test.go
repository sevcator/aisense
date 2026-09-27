package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aisense/internal/config"
)

func TestDisabledHiddenAndKeyBlockedPresentationIncludingCache(t *testing.T) {
	p, cfg := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "visible", Enabled: true, Models: []string{"gpt-visible"}},
			{ID: "disabled", Enabled: false, Models: []string{"gpt-disabled"}},
			{ID: "hidden", Enabled: true, HiddenInvalid: true, Models: []string{"gpt-hidden"}},
			{ID: "blocked", Enabled: true, Models: []string{"provider/gpt-blocked"}, APIKeys: []string{"secret"}, KeyBlockedModels: map[string][]string{config.KeyFingerprint("secret"): {"provider/gpt-blocked"}}},
		}
	})
	for _, up := range cfg.Get().Upstreams {
		p.setCachedUpstream("openai", up.Models[0], up)
		p.recordUsedModel(up.Models[0])
	}
	if got := modelsListIDs(t, p); !reflect.DeepEqual(got, []string{"gpt-visible"}) {
		t.Fatal(got)
	}
	if got := p.UsedModels(); !reflect.DeepEqual(got, []string{"gpt-visible"}) {
		t.Fatal(got)
	}
	if got := p.CachedUpstreams(); len(got) != 1 || got[0].UpstreamID != "visible" {
		t.Fatal(got)
	}
	for _, model := range []string{"gpt-disabled", "gpt-hidden", "gpt-blocked"} {
		if p.writeModelObject(httptest.NewRecorder(), &config.APIKey{}, model) {
			t.Fatalf("hidden retrieve: %s", model)
		}
	}
	// Hiding is presentation-only: cached routing state and original catalog survive.
	if p.getCachedUpstream("openai", "gpt-hidden") == nil || len(cfg.Get().Upstreams[2].Models) != 1 {
		t.Fatal("hiding removed routing state")
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams[0].Enabled = false
		c.Upstreams[2].HiddenInvalid = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := modelsListIDs(t, p); !reflect.DeepEqual(got, []string{"gpt-hidden"}) {
		t.Fatal(got)
	}
	if got := p.CachedUpstreams(); len(got) != 1 || got[0].UpstreamID != "hidden" {
		t.Fatal(got)
	}
}

func TestPerKeyBlockSkipsOnlyExactRawRouteAndHiddenStillRoutes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			t.Error("blocked credential used")
		}
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[]}`)
	}))
	defer api.Close()
	p, _ := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, HiddenInvalid: true, BaseURL: api.URL, Models: []string{"gpt-test"}, APIKeys: []string{"bad", "good"}, KeyBlockedModels: map[string][]string{config.KeyFingerprint("bad"): {"gpt-test"}}}}
	})
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
		r.Header.Set("Authorization", "Bearer gateway-key")
		w := httptest.NewRecorder()
		p.ServeOpenAI(w, r)
		if w.Code != 200 {
			t.Fatalf("hidden route failed: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestTestTransportUsesProxyTLSAuthAndNoRedirect(t *testing.T) {
	proxyCalls := 0
	rotation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls++
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Host != "upstream.invalid" {
			t.Error("wrong proxy credentials or destination")
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer rotation.Close()
	p, cfg := newTestGateway(t, func(c *config.Config) {
		c.Proxies.Enabled = true
		c.Proxies.List = []*config.ProxyEntry{{URL: rotation.URL, Working: true}}
	})
	defer p.CloseTestConnections()
	resp, err := p.TestRequest(context.Background(), &config.Upstream{BaseURL: "http://upstream.invalid", UseProxy: true}, "secret", "models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxyCalls != 1 {
		t.Fatal("rotation transport bypassed")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("none auth sent credentials")
		}
		w.Header().Set("Location", "https://never-follow.invalid")
		w.WriteHeader(302)
	}))
	defer tls.Close()
	if err := cfg.Update(func(c *config.Config) error { c.ModelDiscovery.IgnoreCertErrors = true; return nil }); err != nil {
		t.Fatal(err)
	}
	resp, err = p.TestRequest(context.Background(), &config.Upstream{BaseURL: tls.URL, AuthMode: "none"}, "secret", "models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatal("redirect followed")
	}
}

func TestAllKeysBlockedRawRouteDoesNotSuppressWorkingAlias(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"provider/gpt-test"`) {
			t.Errorf("wrong raw route: %s", body)
		}
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[]}`)
	}))
	defer api.Close()
	p, cfg := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, Models: []string{"gpt-test"}, ModelAliases: map[string][]string{"gpt-test": {"gpt-test", "provider/gpt-test"}}, APIKeys: []string{"secret"}, KeyBlockedModels: map[string][]string{config.KeyFingerprint("secret"): {"gpt-test"}}}}
	})
	if !cfg.Get().Upstreams[0].ModelVisible("gpt-test") {
		t.Fatal("working alias disappeared")
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	r.Header.Set("Authorization", "Bearer gateway-key")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	if w.Code != 200 {
		t.Fatalf("alias suppressed: %d %s", w.Code, w.Body.String())
	}
}
