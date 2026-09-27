package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"aisense/internal/config"
	"aisense/internal/protocol"
)

func TestProtocolDiscoveryIgnoresHintAndFullEndpoint(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(404)
		case "/v1/messages":
			if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Anthropic-Version") == "" {
				t.Errorf("wrong auth %v", r.Header)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != nil || body["messages"] != nil || body["max_tokens"] != float64(0) {
				t.Errorf("generation probe: %v", body)
			}
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages must be an array"}}`)
		case "/v1/models":
			if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Anthropic-Version") == "" {
				t.Errorf("wrong models auth %v", r.Header)
			}
			io.WriteString(w, `{"data":[{"id":"m"}]}`)
		default:
			t.Errorf("incorrect endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	up := &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL + "/v1/messages", APIKeys: []string{"secret"}}
	res := discoverUpstreamModels(context.Background(), up, config.ModelDiscoveryCfg{}, nil)
	if res.err != nil || !res.capabilities[protocol.Messages].Supported || strings.Join(res.models, ",") != "m" {
		t.Fatalf("%+v", res)
	}
	if up.Type != "openai" || !strings.HasSuffix(up.BaseURL, "/messages") {
		t.Fatal("discovery mutated input")
	}
}

func TestCapabilityTestDoesNotMutateConfiguration(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"messages required"}}`)
	}))
	defer api.Close()
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.AutoFixProblems = true
		c.Upstreams = []*config.Upstream{{ID: "up", Type: "wrong", BaseURL: api.URL + "/v1/messages", Enabled: true, Models: []string{"*"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(cfg.Get())
	result := s.testOneUpstream(context.Background(), cfg.Get().Upstreams[0], "ignored-model")
	after, _ := json.Marshal(cfg.Get())
	if string(before) != string(after) {
		t.Fatal("test mutated config")
	}
	if result["ok"] != true {
		t.Fatal(result)
	}
}

func TestAutomaticImportUpsertAndReadOnlyHint(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "legacy", Type: "anthropic", BaseURL: "https://example.test/v1", Enabled: true, APIKeys: []string{"old"}, Models: []string{"*"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.importUpstream(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"text":"https://example.test/v1 new-api-key"}`)))
	if w.Code != 200 || len(cfg.Get().Upstreams) != 1 || cfg.Get().Upstreams[0].Type != "anthropic" || !reflect.DeepEqual(cfg.Get().Upstreams[0].APIKeys, []string{"old", "new-api-key"}) {
		t.Fatalf("%d %s %+v", w.Code, w.Body.String(), cfg.Get().Upstreams)
	}
	w = httptest.NewRecorder()
	s.upsertUpstream(w, httptest.NewRequest("POST", "/?mode=create", strings.NewReader(`{"base_url":"https://other.test/v1/messages","enabled":true,"api_keys":["key"]}`)))
	if w.Code != 200 || len(cfg.Get().Upstreams) != 2 || cfg.Get().Upstreams[1].Type != "auto" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.upsertUpstream(w, httptest.NewRequest("POST", "/?mode=edit", strings.NewReader(`{"id":"legacy","base_url":"https://example.test/v1","type":"openai","enabled":true}`)))
	if w.Code != 200 || cfg.Get().Upstreams[0].Type != "anthropic" {
		t.Fatal("edit changed identity hint", w.Body.String())
	}
	typ := "openai"
	if err := applyUpstreamMassEdit(cfg.Get().Upstreams[0], upstreamMassEditPatch{Type: &typ}); err == nil {
		t.Fatal("bulk type edit accepted")
	}
}

func TestUIProtocolIsReadOnly(t *testing.T) {
	b, err := os.ReadFile("../ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// The protocol is detected per operation, so upstreams show no type setting,
	// legacy type hint or provider brand icon.
	for _, forbidden := range []string{`id="u-type"`, `id="me-type"`, `id="me-apply-type"`, `$('u-type')`, `patch.type =`, `type: $('u-`,
		"u-protocol-hint", "Protocol (automatic)", "+' HINT'", "OAI_ICON", "ANTH_ICON", "brand-icon"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("UI still exposes upstream type: %s", forbidden)
		}
	}
	if !strings.Contains(s, "delete body.type") {
		t.Fatal("upstream save must not send the legacy type hint")
	}
}
