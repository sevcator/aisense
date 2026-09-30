package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

// The Targets panel shows only the credential that actually served a model,
// so a successful request must record that key's fingerprint with the route.
func TestCachedRouteRecordsUsedKey(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer api.Close()

	p, _ := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{{ID: "keyed", Enabled: true, Type: "openai", BaseURL: api.URL, Models: []string{"glm-x"}, APIKeys: []string{"sk-upstream-one", "sk-upstream-two"}}}
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-x","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	if res.Code != 200 {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}

	routes := p.CachedUpstreams()
	if len(routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(routes))
	}
	if routes[0].KeyFingerprint != config.KeyFingerprint("sk-upstream-one") && routes[0].KeyFingerprint != config.KeyFingerprint("sk-upstream-two") {
		t.Fatalf("route fingerprint %q matches neither upstream key", routes[0].KeyFingerprint)
	}
}
