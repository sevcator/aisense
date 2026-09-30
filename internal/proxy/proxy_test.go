package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/store"
)

func TestAPIKeyAppliedToUpstreamRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("Authorization = %q, want Bearer upstream-secret", got)
		}
		if got := r.Header.Get("User-Agent"); got != "OpenAI/Python 1.99.0" {
			t.Errorf("User-Agent = %q, want OpenAI/Python 1.99.0", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	manager, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "key", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "upstream", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"test-model"}, AuthMode: "swap", APIKeys: []string{"upstream-secret"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	gateway := New(manager, usage, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	body, _ := io.ReadAll(res.Result().Body)
	if res.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", res.Code, body)
	}
}

func TestUpstreamKeyPoolRoundRobin(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "swap", APIKeys: []string{"one", "two"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	gateway := New(manager, usage)
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	if got != "one,two,one,two" {
		t.Fatalf("rotation = %q", got)
	}
}

func TestUpstreamKeyPoolRetriesAndCoolsDownKeyFailures(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if key == "bad" {
			http.Error(w, "invalid", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "swap", APIKeys: []string{"bad", "good"}}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	// Failed native auth triggers bounded schema discovery with destination auth.
	if got != "bad,bad,,good,good" {
		t.Fatalf("retry/cooldown sequence = %q", got)
	}
}

func TestAttemptsFeedHitChancePerEndpointAndKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == "limited" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "swap", APIKeys: []string{"limited", "good"}}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	endpoint, keys := gateway.Health.HitRates(manager.Get().Upstreams[0])
	if endpoint.Attempts != 2 || endpoint.Chance != 0.5 {
		t.Fatalf("API base = %+v", endpoint)
	}
	if limited := keys[config.KeyFingerprint("limited")]; limited.Attempts != 1 || limited.Chance != 0 {
		t.Fatalf("limited key = %+v", limited)
	}
	if good := keys[config.KeyFingerprint("good")]; good.Attempts != 1 || good.Chance != 1 {
		t.Fatalf("good key = %+v", good)
	}
}

func TestUpstreamKeyPoolDoesNotRetryServerFailures(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "broken", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "swap", APIKeys: []string{"one", "two"}}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if calls.Load() != 1 {
		t.Fatalf("server failure tried %d keys, want 1", calls.Load())
	}
}

func TestUpstreamKeyPoolSelectionIsConcurrentSafe(t *testing.T) {
	p := &Proxy{upstreamKeys: map[string]*upstreamKeyPoolState{}}
	up := &config.Upstream{ID: "up", AuthMode: "swap", APIKeys: []string{"one", "two", "three"}}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := p.upstreamKeyOrder(up, "", false); len(got) != 3 {
				t.Errorf("key order length = %d", len(got))
			}
		}()
	}
	wg.Wait()
}

func TestUpstreamKeyPoolHonorsRetryAfter(t *testing.T) {
	p := hitchanceProxy(t, "http://synthetic.invalid", []string{"limited", "healthy"})
	up := p.Cfg.Get().Upstreams[0]
	p.markUpstreamKeyFailure(up, "limited", http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}})
	p.upstreamKeyMu.Lock()
	remaining := time.Until(p.upstreamKeys[up.ID].cooldown["limited"])
	p.upstreamKeyMu.Unlock()
	if remaining < 119*time.Second || remaining > 121*time.Second {
		t.Fatalf("Retry-After cooldown = %v", remaining)
	}
	if got := strings.Join(p.upstreamKeyOrder(up, "", false), ","); got != "healthy" {
		t.Fatalf("cooled key remained selectable: %q", got)
	}
}

func TestBuildUpstreamRequestUsesAnthropicPoolKey(t *testing.T) {
	p := &Proxy{}
	up := &config.Upstream{ID: "up", Type: "anthropic", AuthMode: "swap", APIKeys: []string{"anthropic-secret"}}
	in := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	in.Header.Set("Authorization", "Bearer client-key")
	req, err := p.buildUpstreamRequest(up, "anthropic", in, []byte(`{}`), "https://example.com/v1/messages", "anthropic-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-api-key"); got != "anthropic-secret" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("client authorization leaked: %q", got)
	}
}

func TestBuildUpstreamRequestNoneNeverForwardsClientCredentials(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			p := &Proxy{}
			up := &config.Upstream{Type: "auto", AuthMode: "none"}
			in := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			in.Header.Set("Authorization", "Bearer gateway-secret")
			in.Header.Set("X-Api-Key", "gateway-secret")
			req, err := p.buildUpstreamRequest(up, protocol, in, []byte(`{}`), "https://example.invalid/v1/messages", "unused-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("Authorization") != "" || req.Header.Get("X-Api-Key") != "" {
				t.Fatal("none mode leaked credentials")
			}
		})
	}
}

func TestModelsListAndProviderPrefixAreServerSideOnly(t *testing.T) {
	seenModel := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		seenModel <- req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "key", Key: "gateway-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "prefixed", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL,
			Models:        []string{"zai/glm-5.2", "anthropic/claude-opus", "provider/hidden-model"},
			BlockedModels: []string{"hidden-model"}, AuthMode: "none",
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	gateway := New(manager, usage)

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer gateway-key")
	modelsRes := httptest.NewRecorder()
	gateway.ServeOpenAI(modelsRes, modelsReq)
	if modelsRes.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", modelsRes.Code, modelsRes.Body.String())
	}
	if strings.Contains(modelsRes.Body.String(), "zai/glm-5.2") || strings.Contains(modelsRes.Body.String(), "anthropic/claude-opus") {
		t.Fatalf("models list leaked provider prefixes: %s", modelsRes.Body.String())
	}
	for _, want := range []string{"glm-5.2", "claude-opus"} {
		if !strings.Contains(modelsRes.Body.String(), `"id":"`+want+`"`) {
			t.Fatalf("models list missing %q: %s", want, modelsRes.Body.String())
		}
	}
	if strings.Contains(modelsRes.Body.String(), "hidden-model") {
		t.Fatalf("models list exposed blacklisted model: %s", modelsRes.Body.String())
	}
	anthropicModelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	anthropicModelsReq.Header.Set("Authorization", "Bearer gateway-key")
	anthropicModelsRes := httptest.NewRecorder()
	gateway.ServeAnthropic(anthropicModelsRes, anthropicModelsReq)
	if anthropicModelsRes.Code != http.StatusOK {
		t.Fatalf("anthropic models status=%d body=%s", anthropicModelsRes.Code, anthropicModelsRes.Body.String())
	}
	if !strings.Contains(anthropicModelsRes.Body.String(), `"id":"glm-5.2"`) {
		t.Fatalf("anthropic models endpoint returned no gateway models: %s", anthropicModelsRes.Body.String())
	}

	for _, path := range []string{"/models", "/model", "/v1/model"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, res.Code, res.Body.String())
		}
		if strings.Contains(res.Body.String(), "model field is required") || !strings.Contains(res.Body.String(), `"id":"glm-5.2"`) {
			t.Fatalf("%s did not return model list: %s", path, res.Body.String())
		}
	}

	retrieveReq := httptest.NewRequest(http.MethodGet, "/v1/models/ignored-provider/glm-5.2", nil)
	retrieveReq.Header.Set("Authorization", "Bearer gateway-key")
	retrieveRes := httptest.NewRecorder()
	gateway.ServeOpenAI(retrieveRes, retrieveReq)
	if retrieveRes.Code != http.StatusOK {
		t.Fatalf("model retrieve status=%d body=%s", retrieveRes.Code, retrieveRes.Body.String())
	}
	if strings.Contains(retrieveRes.Body.String(), "zai/glm-5.2") || !strings.Contains(retrieveRes.Body.String(), `"id":"glm-5.2"`) {
		t.Fatalf("model retrieve leaked prefix or missed clean id: %s", retrieveRes.Body.String())
	}

	postModelReq := httptest.NewRequest(http.MethodPost, "/model", strings.NewReader(`{"name":"glm-5.2"}`))
	postModelReq.Header.Set("Authorization", "Bearer gateway-key")
	postModelReq.Header.Set("Content-Type", "application/json")
	postModelRes := httptest.NewRecorder()
	gateway.ServeOpenAI(postModelRes, postModelReq)
	if postModelRes.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /model status=%d body=%s", postModelRes.Code, postModelRes.Body.String())
	}
	if strings.Contains(postModelRes.Body.String(), "model field is required") {
		t.Fatalf("POST /model returned confusing completion error: %s", postModelRes.Body.String())
	}

	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"ignored-provider/glm-5.2","messages":[]}`))
	chatReq.Header.Set("Authorization", "Bearer gateway-key")
	chatReq.Header.Set("Content-Type", "application/json")
	chatRes := httptest.NewRecorder()
	gateway.ServeOpenAI(chatRes, chatReq)
	if chatRes.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chatRes.Code, chatRes.Body.String())
	}
	select {
	case got := <-seenModel:
		if got != "zai/glm-5.2" {
			t.Fatalf("upstream model = %q, want zai/glm-5.2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive request")
	}
}

func TestResolveUpstreamModelHonorsBlacklist(t *testing.T) {
	up := &config.Upstream{
		Models:        []string{"*"},
		BlockedModels: []string{"provider/blocked-model", "other-model"},
	}
	for _, blocked := range []string{"blocked-model", "ignored-prefix/blocked-model", "other-model"} {
		if got, ok := resolveUpstreamModel(up, blocked, true); ok {
			t.Fatalf("blocked model %q resolved as %q", blocked, got)
		}
	}
	if got, ok := resolveUpstreamModel(up, "allowed-model", true); !ok || got != "allowed-model" {
		t.Fatalf("allowed model resolved as %q, %v", got, ok)
	}
}

func TestSemanticModelFamilyAppliesToPolicyBlacklistAndCache(t *testing.T) {
	key := &config.APIKey{AllowedModels: []string{"claude-opus-4.6"}}
	for _, alias := range []string{"claude-opus-4.6", "claude-4.6-opus", "opus4.6"} {
		if !allowedModel(key, alias) {
			t.Fatalf("semantic alias %q was rejected by allowlist", alias)
		}
		if got := upstreamCacheKey("openai", alias); got != upstreamCacheKey("openai", "claude-opus-4.6") {
			t.Fatalf("semantic alias %q has a different cache key: %q", alias, got)
		}
	}
	up := &config.Upstream{
		Models:        []string{"opus4.6"},
		BlockedModels: []string{"claude-opus-4.6"},
	}
	if got, ok := resolveUpstreamModel(up, "claude-4.6-opus", true); ok {
		t.Fatalf("blacklisted semantic family resolved as %q", got)
	}
}

func TestResolveUpstreamModelAliases(t *testing.T) {
	up := &config.Upstream{
		Models: []string{
			"anthropic/claude-opus-4.6-thinking",
			"claude-opus-4.6",
			"agnes/agnes-2.0-flash",
			"claude-opus-4-6", // hyphenated twin of claude-opus-4.6
		},
	}
	cases := []struct {
		requested      string
		preferThinking bool
		want           string
		wantOK         bool
	}{
		// prefix alias: user asks bare name, upstream has provider prefix
		{"agnes-2.0-flash", true, "agnes/agnes-2.0-flash", true},
		{"anthropic/agnes-2.0-flash", true, "agnes/agnes-2.0-flash", true},
		// thinking preferred for a plain request
		{"claude-opus-4.6", true, "claude-opus-4.6-thinking", true},
		// plain preferred when explicitly not thinking
		{"claude-opus-4.6", false, "claude-opus-4.6", true},
		// dots vs hyphens: claude-opus-4.6 matches claude-opus-4-6
		{"claude-opus-4.6", false, "claude-opus-4.6", true},
		// requested with -thinking falls back to plain when no thinking model
		{"claude-opus-4.6-thinking", true, "claude-opus-4.6-thinking", true},
		// unknown model: no match
		{"no-such-model", true, "", false},
	}
	for _, c := range cases {
		got, ok := resolveUpstreamModel(up, c.requested, c.preferThinking)
		if ok != c.wantOK || got != c.want {
			t.Errorf("resolve(%q, thinking=%v) = (%q,%v), want (%q,%v)",
				c.requested, c.preferThinking, got, ok, c.want, c.wantOK)
		}
	}

	// wildcard passthrough still works
	wild := &config.Upstream{Models: []string{"*"}}
	if got, ok := resolveUpstreamModel(wild, "anything/else", true); !ok || got != "else" {
		t.Errorf("wildcard resolved as %q, %v", got, ok)
	}

	// display names: prefix + thinking + hyphenated versions collapse
	for in, want := range map[string]string{
		"anthropic/claude-opus-4.6-thinking": "claude-opus-4.6",
		"claude-opus-4-6":                    "claude-opus-4.6",
		"claude-4.6-opus":                    "claude-opus-4.6",
		"aug/opus4.6":                        "claude-opus-4.6",
		"agnes/agnes-2.0-flash":              "agnes-2.0-flash",
		"llama-3-1-70b":                      "llama-3.1-70b",
	} {
		if got := displayModelName(in); got != want {
			t.Errorf("displayModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelAliasRetryAndSuccessfulRouteStickiness(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		seen = append(seen, request.Model)
		mu.Unlock()
		if request.Model == "glm-5.2" || request.Model == "glm-5-2" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model_not_found"}}`))
			return
		}
		if request.Model != "aug/glm-5.2" {
			t.Errorf("unexpected alias %q", request.Model)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL,
			Models: []string{"glm-5.2", "aug/glm-5.2"}, AuthMode: "none",
		}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm_5_2","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	if got != "glm-5.2,glm-5-2,aug/glm-5.2,aug/glm-5.2" {
		t.Fatalf("alias attempts = %q", got)
	}
}

func TestOpenAIOpus46ForcedAliasOrderAndStickiness(t *testing.T) {
	want := []string{
		"claude-opus-4.6-thinking",
		"claude-opus-4-6-thinking",
		"claude-opus-4.6",
		"claude-opus-4-6",
		"opus4.6",
		"claude-4.6-opus",
	}
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		seen = append(seen, request.Model)
		if request.Model != want[len(want)-1] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model_not_found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL,
			Models: []string{"opus4.6"}, AuthMode: "none",
		}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-4.6","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, res.Code, res.Body.String())
		}
	}
	if got, expected := strings.Join(seen[:len(want)], ","), strings.Join(want, ","); got != expected {
		t.Fatalf("forced order = %q, want %q", got, expected)
	}
	if seen[len(seen)-1] != "claude-4.6-opus" || len(seen) != len(want)+1 {
		t.Fatalf("sticky attempts = %#v", seen)
	}
}

// TestModelAliasFailureClearsStickiness verifies that when all aliases for an upstream
// are exhausted (aliasFailure), any previously-sticky route is cleared via forgetModelRoute().
// On the next request the alias list is re-evaluated from scratch, so a route that became
// healthy again is not buried behind the old sticky (broken) entry.
func TestModelAliasFailureClearsStickiness(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	var serveGood atomic.Bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		seen = append(seen, request.Model)
		mu.Unlock()
		if serveGood.Load() && request.Model == "glm-5.2" {
			// Second round: the plain alias now works.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		// First round (and any prefixed alias): always return model-not-found.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model_not_found"}}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gw", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL,
			// aug/glm-5.2 is listed first — it would become sticky if a prior success used it.
			// glm-5.2 is listed second but is the one that becomes healthy in round 2.
			Models: []string{"aug/glm-5.2", "glm-5.2"}, AuthMode: "none",
		}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gw := New(manager, usage)

	// Round 1: both aliases fail → full alias exhaustion → model-scoped failure.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req1.Header.Set("Authorization", "Bearer gw")
	res1 := httptest.NewRecorder()
	gw.ServeOpenAI(res1, req1)
	if res1.Code == http.StatusOK {
		t.Fatal("round 1 should have failed")
	}

	// Round 2: only plain "glm-5.2" now works; prefixed "aug/glm-5.2" still fails.
	// Cooldowns remain hard gates; recovery requires expiry or an explicit reset.
	if err := manager.Update(func(c *config.Config) error { c.Upstreams[0].HitchanceState = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	gw.Health.Reset(manager.Get().Upstreams[0])
	serveGood.Store(true)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer gw")
	res2 := httptest.NewRecorder()
	gw.ServeOpenAI(res2, req2)

	mu.Lock()
	allSeen := seen
	mu.Unlock()

	// Verify that in round 2 "glm-5.2" (the working alias) was indeed attempted —
	// i.e., stickiness did not lock us onto the broken "aug/glm-5.2" as the sole attempt.
	round2Seen := allSeen[2:] // skip round-1 calls
	foundPlain := false
	for _, m := range round2Seen {
		if m == "glm-5.2" {
			foundPlain = true
		}
	}
	if !foundPlain {
		t.Fatalf("round 2 never tried plain alias after stickiness clear; seen=%v", round2Seen)
	}
}

func TestAnthropicModelAliasRetryRewritesRawModelAndUsesAPIKey(t *testing.T) {
	want := []string{
		"claude-opus-4.6-thinking",
		"claude-opus-4-6-thinking",
		"claude-opus-4.6",
		"claude-opus-4-6",
		"opus4.6",
		"claude-4.6-opus",
	}
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "anthropic-secret" {
			t.Errorf("x-api-key = %q", got)
		}
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		seen = append(seen, request.Model)
		if request.Model != want[len(want)-1] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","content":[]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "up", Enabled: true, Priority: 1, Type: "anthropic", BaseURL: upstream.URL,
			Models:   []string{"claude-opus-4.6"},
			AuthMode: "swap", APIKeys: []string{"anthropic-secret"},
		}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude_opus_4_6","max_tokens":16,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeAnthropic(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if got := strings.Join(seen, ","); got != strings.Join(want, ",") {
		t.Fatalf("alias attempts = %q", got)
	}
}

func TestModelAliasDoesNotRetryGenericServerFailure(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"glm-5.2", "aug/glm-5.2"}, AuthMode: "none"}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if calls.Load() != 1 {
		t.Fatalf("generic server failure tried %d aliases", calls.Load())
	}
}

func TestRetryCyclesNeverBypassCoolingUpstreams(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	failed := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
	}
	first, second := failed(&firstCalls), failed(&secondCalls)
	defer first.Close()
	defer second.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 2
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "first", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"model"}, AuthMode: "none"},
			{ID: "second", Enabled: true, Priority: 1, Type: "openai", BaseURL: second.URL, Models: []string{"model"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusServiceUnavailable || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("status=%d calls=%d,%d body=%s", res.Code, firstCalls.Load(), secondCalls.Load(), res.Body.String())
	}
}

func TestRetryCycleLimitAndHitchanceDisabled(t *testing.T) {
	t.Run("ten cycles cannot bypass active cooldown", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer upstream.Close()
		dir := t.TempDir()
		manager, _ := config.Load(filepath.Join(dir, "config.json"))
		_ = manager.Update(func(c *config.Config) error {
			c.Hitchance.RetryCycles = 10
			c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
			c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "none"}}
			return nil
		})
		usage, _ := store.New(filepath.Join(dir, "usage.json"))
		defer usage.Close()
		gateway := New(manager, usage)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway")
		gateway.ServeOpenAI(httptest.NewRecorder(), req)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1 under active cooldown", calls.Load())
		}
	})

	t.Run("disabled policy disables cycling and retries", func(t *testing.T) {
		var firstCalls, secondCalls atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			firstCalls.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secondCalls.Add(1)
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		defer second.Close()
		dir := t.TempDir()
		manager, _ := config.Load(filepath.Join(dir, "config.json"))
		_ = manager.Update(func(c *config.Config) error {
			c.Hitchance.Enabled = false
			c.Hitchance.RetryCycles = 10
			c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
			c.Upstreams = []*config.Upstream{
				{ID: "first", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"model"}, AuthMode: "none"},
				{ID: "second", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"model"}, AuthMode: "none"},
			}
			return nil
		})
		usage, _ := store.New(filepath.Join(dir, "usage.json"))
		defer usage.Close()
		gateway := New(manager, usage)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != http.StatusServiceUnavailable || firstCalls.Load() != 1 || secondCalls.Load() != 0 {
			t.Fatalf("status=%d calls=%d,%d", res.Code, firstCalls.Load(), secondCalls.Load())
		}
	})
}

func TestExhaustedAuthAndRateLimitKeepScopedHealth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		minimum    time.Duration
	}{{"unauthorized", 401, "", 4 * time.Minute}, {"forbidden", 403, "", 4 * time.Minute}, {"rate limited", 429, "2", time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"request rejected"}}`))
			}))
			defer upstream.Close()
			dir := t.TempDir()
			manager, _ := config.Load(filepath.Join(dir, "config.json"))
			_ = manager.Update(func(c *config.Config) error {
				c.Hitchance.RetryCycles = 1
				c.Hitchance.CoolingRetrySeconds = 0 // this test is about the recorded rest, not the wait
				c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
				c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: []string{"model"}, AuthMode: "swap", APIKeys: []string{"synthetic-key"}}}
				return nil
			})
			usage, _ := store.New(filepath.Join(dir, "usage.json"))
			defer usage.Close()
			gateway := New(manager, usage)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
			req.Header.Set("Authorization", "Bearer gateway")
			gateway.ServeOpenAI(httptest.NewRecorder(), req)
			record := gateway.Health.Snapshot(manager.Get().Upstreams)[0]
			up := manager.Get().Upstreams[0]
			state := up.HitchanceState[config.HitchanceTarget("key", "synthetic-key", "model")]
			if time.Until(state.Until) < tc.minimum || record.CooldownUntil != nil {
				t.Fatalf("scoped failure widened or lost: state=%+v runtime=%+v", state, record)
			}
			if _, ok := up.HitchanceState["endpoint"]; ok {
				t.Fatal("key failure widened to endpoint")
			}
		})
	}
}

func TestResponseStartTimeoutFailsOverButDoesNotStopActiveStream(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer fast.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.Hitchance.ResponseStartTimeoutSeconds = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "slow", Enabled: true, Priority: 1, Type: "openai", BaseURL: slow.URL, Models: []string{"model"}, AuthMode: "none"},
			{ID: "fast", Enabled: true, Priority: 2, Type: "openai", BaseURL: fast.URL, Models: []string{"model"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	started := time.Now()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout failover status=%d duration=%v body=%s", res.Code, time.Since(started), res.Body.String())
	}
	health := gateway.Health.Snapshot(manager.Get().Upstreams)
	if health[0].Status != "unavailable" {
		t.Fatalf("slow health = %#v", health[0])
	}

	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = w.Write([]byte("data: second\n\n"))
	}))
	defer stream.Close()
	_ = manager.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "stream", Enabled: true, Priority: 1, Type: "openai", BaseURL: stream.URL, Models: []string{"model"}, AuthMode: "none"}}
		return nil
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res = httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "first") || !strings.Contains(res.Body.String(), "second") {
		t.Fatalf("active stream was interrupted: status=%d body=%q", res.Code, res.Body.String())
	}
}

func TestLogicalErrorEnvelopeFailsOverBeforeResponseCommit(t *testing.T) {
	cases := []struct {
		name      string
		request   string
		firstType string
		firstBody string
	}{
		{
			name:      "stream request with JSON error body",
			request:   `{"model":"testmodel","stream":true,"messages":[]}`,
			firstType: "application/json",
			firstBody: `{"error":{"type":"invalid_request_error","message":"opaque upstream failure"}}`,
		},
		{
			name:      "stream request with SSE error event",
			request:   `{"model":"testmodel","stream":true,"messages":[]}`,
			firstType: "text/event-stream",
			firstBody: "data: {\"error\":{\"message\":\"model not found\"}}\n\n",
		},
		{
			name:      "non-stream request with JSON error body",
			request:   `{"model":"testmodel","messages":[]}`,
			firstType: "application/json",
			firstBody: `{"error":{"type":"service_unavailable","message":"all upstream accounts are inactive"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				w.Header().Set("Content-Type", tc.firstType)
				_, _ = w.Write([]byte(tc.firstBody))
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls.Add(1)
				if strings.Contains(tc.request, `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer second.Close()

			dir := t.TempDir()
			manager, _ := config.Load(filepath.Join(dir, "config.json"))
			_ = manager.Update(func(c *config.Config) error {
				c.Hitchance.RetryCycles = 1
				c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
				c.Upstreams = []*config.Upstream{
					{ID: "false-200", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"testmodel"}, AuthMode: "none"},
					{ID: "working", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"testmodel"}, AuthMode: "none"},
				}
				return nil
			})
			usage, _ := store.New(filepath.Join(dir, "usage.json"))
			defer usage.Close()
			gateway := New(manager, usage)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.request))
			req.Header.Set("Authorization", "Bearer gateway")
			res := httptest.NewRecorder()
			gateway.ServeOpenAI(res, req)
			if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "ok") || strings.Contains(res.Body.String(), "upstream failure") {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
			if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
				t.Fatalf("calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
			}
			cached := gateway.CachedUpstreams()
			if len(cached) != 1 || cached[0].UpstreamID != "working" {
				t.Fatalf("cached routes = %#v", cached)
			}
		})
	}
}

func TestStreamingRequestAcceptsValidJSONCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: []string{"testmodel"}, AuthMode: "none"}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"testmodel","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestClientCancellationStopsCyclesWithoutPenalizingHealth(t *testing.T) {
	started := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, "late", http.StatusServiceUnavailable)
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer second.Close()
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 10
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "first", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"model"}, AuthMode: "none"},
			{ID: "second", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"model"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[]}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer gateway")
	done := make(chan struct{})
	go func() {
		gateway.ServeOpenAI(httptest.NewRecorder(), req)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cancelled request did not stop routing")
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("client cancellation reached %d later upstreams", secondCalls.Load())
	}
	if record := gateway.Health.Snapshot(manager.Get().Upstreams)[0]; record.LastFailureAt != nil || record.CooldownUntil != nil {
		t.Fatalf("client cancellation penalized upstream: %#v", record)
	}
}

func TestModelAliasExhaustionFailsOverToNextUpstream(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"unknown model"}}`))
	}))
	defer first.Close()
	var secondModel string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		secondModel = request.Model
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer second.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "first", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"glm-5.2", "aug/glm-5.2"}, AuthMode: "none"},
			{ID: "second", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"forge/glm-5-2"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm_5_2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if firstCalls.Load() != 3 || secondModel != "forge/glm-5-2" {
		t.Fatalf("first calls=%d second model=%q", firstCalls.Load(), secondModel)
	}
	health := gateway.Health.Snapshot(manager.Get().Upstreams)
	if health[0].CooldownUntil != nil || len(manager.Get().Upstreams[0].HitchanceState) == 0 || health[1].Status != "available" {
		t.Fatalf("model-scoped health = %#v", health)
	}
}

func TestModelAliasAttemptsAreBoundedAcrossThinkingPasses(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer upstream.Close()
	rawModels := make([]string, 0, 40)
	for i := 0; i < 20; i++ {
		rawModels = append(rawModels, fmt.Sprintf("thinking-%02d/glm-5.2-thinking", i))
		rawModels = append(rawModels, fmt.Sprintf("plain-%02d/glm-5.2", i))
	}
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: rawModels, AuthMode: "none"}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if calls.Load() != modelalias.MaxCandidates {
		t.Fatalf("alias attempts = %d, want %d", calls.Load(), modelalias.MaxCandidates)
	}
}

func TestDebugTraceCapturesLifecycleAndRedactsSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream-cookie=secret")
		_, _ = w.Write([]byte(`{"answer":"visible-response","access_token":"response-secret"}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "key-id", Key: "gateway-secret", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "upstream-id", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"debug-model"}, AuthMode: "swap", APIKeys: []string{"upstream-cookie=secret"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	logPath := filepath.Join(dir, "trace.jsonl")
	trace, err := debuglog.New(logPath, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	gateway := New(manager, usage, trace)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?code=query-secret", strings.NewReader(`{"model":"debug-model","messages":[{"content":"visible-request"}],"api_key":"body-secret"}`))
	req.Header.Set("Authorization", "Bearer gateway-secret")
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, event := range []string{"request.received", "route.candidates", "attempt.start", "upstream.request", "upstream.response", "client.response_chunk", "request.complete"} {
		if !strings.Contains(text, `"event":"`+event+`"`) {
			t.Fatalf("missing event %q in trace: %s", event, text)
		}
	}
	for _, visible := range []string{"debug-model", "visible-request", "visible-response", "upstream-id", "key-id"} {
		if !strings.Contains(text, visible) {
			t.Fatalf("trace missing %q: %s", visible, text)
		}
	}
	for _, secret := range []string{"gateway-secret", "provider-cookie=secret", "query-secret", "body-secret", "response-secret", "upstream-cookie=secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("trace leaked %q: %s", secret, text)
		}
	}
}

func TestModelAliasFailsOverOn410GoneEOL(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Gone","status":410,"detail":"The model 'deepseek-ai/deepseek-v4-pro' has reached its end of life on 2026-08-07T09:00:00Z and is no longer available."}`))
	}))
	defer first.Close()

	var secondModel string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		secondModel = request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok from fallback"}}]}`))
	}))
	defer second.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.Rules = append([]hitchance.Rule{{ID: "model-eol", StatusCodes: []int{410}, Messages: []string{"end of life"}, Category: "model", Action: "demote", Scope: "model", CooldownSeconds: 300}}, c.Hitchance.Rules...)
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "nvidia-nim", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"deepseek-ai/deepseek-v4-pro"}, AuthMode: "none"},
			{ID: "fallback-provider", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"deepseek-v4-pro"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed with status 200, got %d body=%s", res.Code, res.Body.String())
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("first upstream called %d times, want 1", firstCalls.Load())
	}
	if secondModel != "deepseek-v4-pro" {
		t.Fatalf("second upstream model = %q, want deepseek-v4-pro", secondModel)
	}
	health := gateway.Health.Snapshot(manager.Get().Upstreams)
	if health[0].CooldownUntil != nil || len(manager.Get().Upstreams[0].HitchanceState) == 0 || health[1].Status != "available" {
		t.Fatalf("expected first upstream degraded for EOL model, got health = %#v", health)
	}
}

func TestHitchanceOnCustomBodyMessage(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		// Returns 400 with a custom quota error message
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"custom low balance diagnostic"}}`))
	}))
	defer first.Close()

	var secondCalled atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback success"}}]}`))
	}))
	defer second.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Hitchance.Rules = []hitchance.Rule{{ID: "custom-balance", Messages: []string{"custom low balance"}, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 60}}
		c.Upstreams = []*config.Upstream{
			{ID: "up-1", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"custom-model"}, AuthMode: "none"},
			{ID: "up-2", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"custom-model"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"custom-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected status 200 via failover, got %d body=%s", res.Code, res.Body.String())
	}
	if !secondCalled.Load() {
		t.Fatal("expected second upstream to be called after body message failover")
	}
}

func TestUsedModelsTrackedAfterSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gw-key", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "up", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"gpt-5.6-sol", "claude-opus-4.6"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	if len(gateway.UsedModels()) != 0 {
		t.Fatalf("expected 0 used models initially, got %v", gateway.UsedModels())
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gw-key")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.Code)
	}

	used := gateway.UsedModels()
	if len(used) != 1 || used[0] != "gpt-5.6-sol" {
		t.Fatalf("expected [gpt-5.6-sol], got %v", used)
	}

	// Make another request with a different model
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-4.6","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer gw-key")
	res2 := httptest.NewRecorder()
	gateway.ServeOpenAI(res2, req2)
	if res2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", res2.Code)
	}

	used = gateway.UsedModels()
	if len(used) != 2 || used[0] != "claude-opus-4.6" || used[1] != "gpt-5.6-sol" {
		t.Fatalf("expected [claude-opus-4.6, gpt-5.6-sol], got %v", used)
	}
}

func TestStickyUpstreamExpiresAfterTTL(t *testing.T) {
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.StickyUpstreamTTLHours = 1 // 1 hour TTL
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	up := &config.Upstream{
		ID:     "up-1",
		Models: []string{"glm-5.2", "aug/glm-5.2"},
	}

	// Manually inject a fresh sticky route preferring aug/glm-5.2
	gateway.rememberModelRoute(up, "glm-5.2", false, nil, "aug/glm-5.2")

	// Fresh: modelRouteCandidates should prioritize "aug/glm-5.2"
	cands := gateway.modelRouteCandidates(up, "glm-5.2", false, nil)
	if len(cands) < 2 || cands[0] != "aug/glm-5.2" {
		t.Fatalf("expected sticky candidate aug/glm-5.2 first, got %v", cands)
	}

	// Manually age the sticky entry past TTL (2 hours ago)
	stateKey := routeStateKey(up.ID, "glm-5.2", false, nil)
	gateway.modelAliasMu.Lock()
	gateway.modelAliases[stateKey] = routeEntry{
		route:    "aug/glm-5.2",
		cachedAt: time.Now().Add(-2 * time.Hour),
	}
	gateway.modelAliasMu.Unlock()

	// Expired: modelRouteCandidates should drop sticky preference and revert to default ("glm-5.2" first)
	candsExpired := gateway.modelRouteCandidates(up, "glm-5.2", false, nil)
	if len(candsExpired) < 2 || candsExpired[0] != "glm-5.2" {
		t.Fatalf("expected expired sticky to revert to default glm-5.2, got %v", candsExpired)
	}
}

func TestCachedUpstreamRoutesTTLPurge(t *testing.T) {
	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.Hitchance.UpstreamCacheTTLHours = 2 // 2 hours TTL
		c.Upstreams = []*config.Upstream{{ID: "up-1", Enabled: true, Models: []string{"fresh-model"}}}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	// Inject a fresh route and a stale route (> 2 hours)
	gateway.cachedUpstreamMu.Lock()
	gateway.cachedUpstreams["openai:fresh-model"] = &CachedUpstreamEntry{
		Type:       "openai",
		UpstreamID: "up-1",
		Model:      "fresh-model",
		CachedAt:   time.Now().Add(-30 * time.Minute),
	}
	gateway.cachedUpstreams["openai:stale-model"] = &CachedUpstreamEntry{
		Type:       "openai",
		UpstreamID: "up-2",
		Model:      "stale-model",
		CachedAt:   time.Now().Add(-3 * time.Hour),
	}
	gateway.cachedUpstreamMu.Unlock()

	routes := gateway.CachedUpstreams()
	if len(routes) != 1 {
		t.Fatalf("expected exactly 1 fresh route, got %d (%v)", len(routes), routes)
	}
	if routes[0].Model != "fresh-model" || routes[0].UpstreamID != "up-1" {
		t.Fatalf("expected fresh-model from up-1, got %v", routes[0])
	}

	// Verify internal map purged the stale entry
	gateway.cachedUpstreamMu.Lock()
	_, hasStale := gateway.cachedUpstreams["openai:stale-model"]
	gateway.cachedUpstreamMu.Unlock()
	if hasStale {
		t.Fatal("expected stale entry to be removed from cachedUpstreams")
	}
}

func TestModelUpstreamCacheHitAndPrioritization(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model_not_found"}}`))
	}))
	defer first.Close()

	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"response from second"}}]}`))
	}))
	defer second.Close()

	dir := t.TempDir()
	manager, _ := config.Load(filepath.Join(dir, "config.json"))
	_ = manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gw", Enabled: true}}
		c.Upstreams = []*config.Upstream{
			{ID: "up-first", Enabled: true, Priority: 1, Type: "openai", BaseURL: first.URL, Models: []string{"gpt-5.6-sol"}, AuthMode: "none"},
			{ID: "up-second", Enabled: true, Priority: 2, Type: "openai", BaseURL: second.URL, Models: []string{"gpt-5.6-sol"}, AuthMode: "none"},
		}
		return nil
	})
	usage, _ := store.New(filepath.Join(dir, "usage.json"))
	defer usage.Close()
	gateway := New(manager, usage)

	// Request 1: First upstream (Priority 1) fails with 404, fails over to second (Priority 2) which succeeds.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[]}`))
	req1.Header.Set("Authorization", "Bearer gw")
	res1 := httptest.NewRecorder()
	gateway.ServeOpenAI(res1, req1)
	if res1.Code != http.StatusOK {
		t.Fatalf("request 1 failed: status=%d body=%s", res1.Code, res1.Body.String())
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 1 {
		t.Fatalf("expected synthesized punctuation retry then failover on req1, got first=%d, second=%d", firstCalls.Load(), secondCalls.Load())
	}

	// Verify cachedUpstreams pinned up-second for gpt-5.6-sol
	cached := gateway.CachedUpstreams()
	if len(cached) != 1 || cached[0].UpstreamID != "up-second" {
		t.Fatalf("expected up-second cached, got %v", cached)
	}

	// Request 2: Because up-second is pinned/cached, it MUST be tried directly at Index 0 without hitting up-first!
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer gw")
	res2 := httptest.NewRecorder()
	gateway.ServeOpenAI(res2, req2)
	if res2.Code != http.StatusOK {
		t.Fatalf("request 2 failed: status=%d body=%s", res2.Code, res2.Body.String())
	}
	// firstCalls should remain at the two aliases attempted by request 1.
	if firstCalls.Load() != 2 {
		t.Fatalf("expected first upstream not to be called on req2, calls=%d", firstCalls.Load())
	}
	if secondCalls.Load() != 2 {
		t.Fatalf("expected second upstream to be called directly on req2, calls=%d", secondCalls.Load())
	}
}

func newTestGateway(t *testing.T, mutate func(c *config.Config)) (*Proxy, *config.Manager) {
	t.Helper()
	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "key", Key: "gateway-key", Enabled: true}}
		mutate(c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.Close() })
	return New(manager, usage), manager
}

func modelsListIDs(t *testing.T, gateway *Proxy) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", res.Code, res.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestModelsListMergesHidesMetaAndVariants(t *testing.T) {
	gateway, manager := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "a", Enabled: true, Type: "openai", BaseURL: "http://a.invalid", Models: []string{"gpt5.6-sol"}, AuthMode: "none"},
			{ID: "b", Enabled: true, Type: "openai", BaseURL: "http://b.invalid", Models: []string{"gpt-5.6-sol", "gpt-5.6-sol-low", "gpt-5.6-sol-medium", "auto", "auto-beta", "nova.2-max", "glm-5.2", "glm-5.2-fast"}, AuthMode: "none"},
		}
	})

	ids := modelsListIDs(t, gateway)
	has := func(want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	if !has("gpt-5.6-sol") {
		t.Fatalf("merged canonical gpt-5.6-sol missing: %v", ids)
	}
	for _, unwanted := range []string{"gpt5.6-sol", "auto", "auto-beta", "*", "gpt-5.6-sol-low", "gpt-5.6-sol-medium"} {
		if has(unwanted) {
			t.Fatalf("model list should not contain %q: %v", unwanted, ids)
		}
	}
	if !has("nova.2-max") || has("nova.2") {
		t.Fatalf("standalone variant must remain visible without fabricating a base: %v", ids)
	}
	if !has("glm-5.2-fast") {
		t.Fatalf("-fast must stay visible while fast mode is off: %v", ids)
	}

	if err := manager.Update(func(c *config.Config) error {
		c.Models.FastMode = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ids = modelsListIDs(t, gateway)
	for _, id := range ids {
		if id == "glm-5.2-fast" {
			t.Fatalf("glm-5.2-fast must be hidden in fast mode: %v", ids)
		}
	}
	found := false
	for _, id := range ids {
		if id == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("glm-5.2 missing after enabling fast mode: %v", ids)
	}
}

func TestReasoningEffortRoutesToVariantModel(t *testing.T) {
	seenBase := make(chan string, 1)
	upstreamBase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		seenBase <- req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstreamBase.Close()
	seenVariant := make(chan string, 1)
	upstreamVariant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		seenVariant <- req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstreamVariant.Close()

	newGateway := func() *Proxy {
		gateway, _ := newTestGateway(t, func(c *config.Config) {
			c.Upstreams = []*config.Upstream{
				{ID: "base-only", Enabled: true, Type: "openai", BaseURL: upstreamBase.URL, Models: []string{"gpt-5.6-sol"}, AuthMode: "none"},
				{ID: "with-variant", Enabled: true, Type: "openai", BaseURL: upstreamVariant.URL, Models: []string{"gpt-5.6-sol", "gpt-5.6-sol-low"}, AuthMode: "none"},
			}
		})
		return gateway
	}

	chat := func(gateway *Proxy, payload string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer gateway-key")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		return res.Code
	}

	// effort=low: the variant-serving upstream must win and receive the -low model.
	gateway := newGateway()
	if code := chat(gateway, `{"model":"gpt-5.6-sol","reasoning_effort":"low","messages":[]}`); code != http.StatusOK {
		t.Fatalf("effort=low status=%d", code)
	}
	select {
	case got := <-seenVariant:
		if got != "gpt-5.6-sol-low" {
			t.Fatalf("variant upstream model = %q, want gpt-5.6-sol-low", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("variant upstream did not receive the effort=low request")
	}
	select {
	case got := <-seenBase:
		t.Fatalf("base upstream should not have been tried first, got %q", got)
	default:
	}

	// effort=high without a -high variant anywhere: base model is used.
	gateway = newGateway()
	if code := chat(gateway, `{"model":"gpt-5.6-sol","reasoning_effort":"high","messages":[]}`); code != http.StatusOK {
		t.Fatalf("effort=high status=%d", code)
	}
	select {
	case got := <-seenVariant:
		if got != "gpt-5.6-sol" {
			t.Fatalf("variant upstream model = %q, want gpt-5.6-sol", got)
		}
	case got := <-seenBase:
		if got != "gpt-5.6-sol" {
			t.Fatalf("base upstream model = %q, want gpt-5.6-sol", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no upstream received the effort=high request")
	}

	// no effort: regular routing, base model.
	gateway = newGateway()
	if code := chat(gateway, `{"model":"gpt-5.6-sol","messages":[]}`); code != http.StatusOK {
		t.Fatalf("plain status=%d", code)
	}
	select {
	case got := <-seenVariant:
		if got != "gpt-5.6-sol" {
			t.Fatalf("variant upstream model = %q, want gpt-5.6-sol", got)
		}
	case got := <-seenBase:
		if got != "gpt-5.6-sol" {
			t.Fatalf("base upstream model = %q, want gpt-5.6-sol", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no upstream received the plain request")
	}

	// explicit variant request keeps working even though the variant is hidden from the list.
	gateway = newGateway()
	if code := chat(gateway, `{"model":"gpt-5.6-sol-low","messages":[]}`); code != http.StatusOK {
		t.Fatalf("explicit variant status=%d", code)
	}
	select {
	case got := <-seenVariant:
		if got != "gpt-5.6-sol-low" {
			t.Fatalf("explicit variant model = %q, want gpt-5.6-sol-low", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("explicit variant request did not reach the variant upstream")
	}
}

func TestFastModePrefersFastVariant(t *testing.T) {
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		seen <- req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	chat := func(gateway *Proxy) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		return res.Code
	}

	gateway, _ := newTestGateway(t, func(c *config.Config) {
		c.Models.FastMode = true
		c.Upstreams = []*config.Upstream{
			{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: []string{"glm-5.2", "glm-5.2-fast"}, AuthMode: "none"},
		}
	})
	if code := chat(gateway); code != http.StatusOK {
		t.Fatalf("fast mode status=%d", code)
	}
	select {
	case got := <-seen:
		if got != "glm-5.2-fast" {
			t.Fatalf("fast mode model = %q, want glm-5.2-fast", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive fast mode request")
	}

	gateway, _ = newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: []string{"glm-5.2", "glm-5.2-fast"}, AuthMode: "none"},
		}
	})
	if code := chat(gateway); code != http.StatusOK {
		t.Fatalf("normal mode status=%d", code)
	}
	select {
	case got := <-seen:
		if got != "glm-5.2" {
			t.Fatalf("normal mode model = %q, want glm-5.2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive normal request")
	}
}
