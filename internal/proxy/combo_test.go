package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

// A combo is one model name standing for several models: the request walks them
// in order, so the next model answers as soon as the current one cannot.
func TestComboFallsThroughToItsNextModel(t *testing.T) {
	var down, live []string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down = append(down, modelOf(r))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream is down"}}`))
	}))
	defer downstream.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := modelOf(r)
		live = append(live, model)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"model":"` + model + `"}`))
	}))
	defer upstream.Close()

	gateway, manager := newTestGateway(t, func(c *config.Config) {
		c.Hitchance.RetryCycles = 1
		c.Combos = []*config.ModelCombo{{Name: "favourite", Models: []string{"missing-model", "down-model", "live-model"}}}
		c.Upstreams = []*config.Upstream{
			{ID: "down", Enabled: true, Priority: 1, Type: "openai", BaseURL: downstream.URL, Models: []string{"down-model"}, AuthMode: "swap", APIKeys: []string{"k1"}},
			{ID: "live", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"live-model"}, AuthMode: "swap", APIKeys: []string{"k2"}},
		}
	})
	res := comboRequest(t, gateway, "favourite")
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"model":"live-model"`) {
		t.Fatalf("combo answered with %s", res.Body.String())
	}
	// "missing-model" has no upstream at all and is skipped without a request.
	if len(down) != 1 || down[0] != "down-model" || len(live) != 1 || live[0] != "live-model" {
		t.Fatalf("models tried: down=%v live=%v", down, live)
	}
	// Every model of the combo failing is still one "unavailable" answer,
	// reported for the name the client asked for.
	if err := manager.Update(func(c *config.Config) error {
		c.Combos[0].Models = []string{"down-model"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res = comboRequest(t, gateway, "favourite")
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), "favourite") {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func modelOf(r *http.Request) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Model
}

// The rate limit belongs to the client's request, not to each model tried.
func TestComboChargesTheRateLimitOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	gateway, _ := newTestGateway(t, func(c *config.Config) {
		c.Hitchance.RetryCycles = 1
		c.APIKeys = []*config.APIKey{{ID: "key", Key: "gateway-key", Enabled: true, RPM: 2}}
		c.Combos = []*config.ModelCombo{{Name: "favourite", Models: []string{"missing-model", "live-model"}}}
		c.Upstreams = []*config.Upstream{{ID: "live", Enabled: true, Priority: 1, Type: "openai", BaseURL: upstream.URL, Models: []string{"live-model"}, AuthMode: "swap", APIKeys: []string{"k"}}}
	})
	for i := 0; i < 2; i++ {
		if res := comboRequest(t, gateway, "favourite"); res.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i+1, res.Code, res.Body.String())
		}
	}
	if res := comboRequest(t, gateway, "favourite"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: status=%d body=%s", res.Code, res.Body.String())
	}
}

// Clients pick a combo from the model list like any other model.
func TestComboNamesAreListedForKeysThatMayUseThem(t *testing.T) {
	gateway, manager := newTestGateway(t, func(c *config.Config) {
		c.Combos = []*config.ModelCombo{
			{Name: "favourite", Models: []string{"live-model"}},
			{Name: "unreachable", Models: []string{"other-model"}},
		}
		c.Upstreams = []*config.Upstream{{ID: "live", Enabled: true, Priority: 1, Type: "openai", BaseURL: "http://127.0.0.1:1", Models: []string{"live-model", "other-model"}, AuthMode: "swap", APIKeys: []string{"k"}}}
	})
	ids := modelsListIDs(t, gateway)
	for _, want := range []string{"favourite", "unreachable", "live-model"} {
		if !contains(ids, want) {
			t.Fatalf("model list %v is missing %s", ids, want)
		}
	}
	// A restricted key only sees the combos whose models it may use.
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys[0].AllowedModels = []string{"live-model"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ids = modelsListIDs(t, gateway)
	if !contains(ids, "favourite") || contains(ids, "unreachable") {
		t.Fatalf("restricted key sees %v", ids)
	}
}

func comboRequest(t *testing.T, gateway *Proxy, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	gateway.ServeOpenAI(res, req)
	return res
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
