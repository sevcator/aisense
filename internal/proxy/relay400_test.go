package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aisense/internal/config"
)

// A relay provider answers 400 with its catch-all "openai_error" type when its
// own upstream broke. That must not terminate the walk: the model rests on the
// failing upstream and the next upstream serves the request.
func TestRelayCatchAll400FailsOverToNextUpstream(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	record := func(name string) {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("bad")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The server had an error while processing your request","type":"openai_error"}}`))
	}))
	defer failing.Close()
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		record("good:" + body.Model)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer serving.Close()

	p, _ := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "bad", Enabled: true, Type: "openai", BaseURL: failing.URL, Models: []string{"glm-5.3"}, AuthMode: "none"},
			{ID: "good", Enabled: true, Type: "openai", BaseURL: serving.URL, Models: []string{"glm-5.3"}, AuthMode: "none"},
		}
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	bad, good := 0, false
	for _, name := range asked {
		switch {
		case name == "bad":
			bad++
		case strings.HasPrefix(name, "good:"):
			good = true
		}
	}
	if bad == 0 || !good {
		t.Fatalf("expected the failing upstream to be asked and the next one to serve, got %v", asked)
	}
}
