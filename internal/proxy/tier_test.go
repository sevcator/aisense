package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aisense/internal/config"
	"aisense/internal/store"
)

// tierUpstream records the model names it was asked to serve.
func tierUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	return up, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func tierGateway(t *testing.T, baseURL string, models []string) *Proxy {
	t.Helper()
	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{
			ID: "up", Enabled: true, Priority: 1, Type: "openai",
			BaseURL: baseURL, Models: models, AuthMode: "none",
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.Close() })
	return New(manager, usage)
}

func tierRequest(p *Proxy, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	return res
}

func TestBestAndShitTiersRouteByPriceHeuristic(t *testing.T) {
	up, received := tierUpstream(t)
	defer up.Close()
	models := []string{
		"gpt-5.6", "claude-opus-4.6", "glm-5.3-flash", "minimax-m2.7-highspeed",
		"minimax-m2.1-highspeed", "free", "text-embedding-v3",
	}
	p := tierGateway(t, up.URL, models)

	res := tierRequest(p, "best")
	if res.Code != http.StatusOK {
		t.Fatalf("best status = %d body=%s", res.Code, res.Body.String())
	}
	got := received()
	// The claude-opus-4.6 synonym family leads its route list with the forced
	// "-thinking" spelling, exactly as a direct request for the base name does.
	if len(got) != 1 || !strings.HasPrefix(got[0], "claude-opus-4.6") {
		t.Fatalf("best served %v, want the claude-opus-4.6 family", got)
	}

	res = tierRequest(p, "shit")
	if res.Code != http.StatusOK {
		t.Fatalf("shit status = %d body=%s", res.Code, res.Body.String())
	}
	got = received()
	if len(got) != 2 || got[1] != "minimax-m2.1-highspeed" {
		t.Fatalf("shit served %v, want minimax-m2.1-highspeed second", got)
	}
}

func TestTierFallsOverToNextModelWhenFirstUnavailable(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		// The most premium family answers 404 model-not-found under every
		// spelling (the forced "-thinking"/"opus4.6" synonyms lead), so the
		// walk must continue with the next one.
		if strings.Contains(body.Model, "opus") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	p := tierGateway(t, up.URL, []string{"claude-opus-4.6", "gpt-5.6", "glm-5.3-flash"})

	res := tierRequest(p, "best")
	if res.Code != http.StatusOK {
		t.Fatalf("best status = %d body=%s", res.Code, res.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[len(seen)-1] != "gpt-5.6" {
		t.Fatalf("tier walk served %v, want failover past the claude family to gpt-5.6", seen)
	}
	for _, served := range seen[:len(seen)-1] {
		if !strings.Contains(served, "opus") {
			t.Fatalf("unexpected pre-failover route %q in %v", served, seen)
		}
	}
}

func TestJunkModelRequestsAnswer404(t *testing.T) {
	up, received := tierUpstream(t)
	defer up.Close()
	p := tierGateway(t, up.URL, []string{"free", "free-model", "glm-5.3"})

	for _, model := range []string{"free", "FREE", "free-model", "step-router-v1"} {
		res := tierRequest(p, model)
		if res.Code != http.StatusNotFound {
			t.Fatalf("model %q status = %d, want 404", model, res.Code)
		}
	}
	if got := received(); len(got) != 0 {
		t.Fatalf("junk models reached the upstream: %v", got)
	}
	// A real model on the same upstream still routes.
	if res := tierRequest(p, "glm-5.3"); res.Code != http.StatusOK {
		t.Fatalf("glm-5.3 status = %d", res.Code)
	}
}

func TestModelListIncludesTiersAndExcludesJunk(t *testing.T) {
	up, _ := tierUpstream(t)
	defer up.Close()
	p := tierGateway(t, up.URL, []string{"claude-opus-4.6", "glm-5.3-flash", "free", "text-embedding-v3"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("models status = %d", res.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range body.Data {
		ids[item.ID] = true
	}
	if !ids["best"] || !ids["shit"] {
		t.Fatalf("tier names missing from model list: %v", ids)
	}
	for _, junk := range []string{"free", "text-embedding-v3"} {
		if ids[junk] {
			t.Fatalf("junk model %q listed", junk)
		}
	}
	if !ids["claude-opus-4.6"] || !ids["glm-5.3-flash"] {
		t.Fatalf("real models missing from list: %v", ids)
	}
}

func TestTierRetrieveEndpoint(t *testing.T) {
	up, _ := tierUpstream(t)
	defer up.Close()
	p := tierGateway(t, up.URL, []string{"claude-opus-4.6"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/best", nil)
	req.Header.Set("Authorization", "Bearer gateway")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"id":"best"`) {
		t.Fatalf("retrieve best = %d %s", res.Code, res.Body.String())
	}
}
