package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aisense/internal/config"
)

func TestPresentationPreservesExplicitRawRoutes(t *testing.T) {
	names := []string{"0g-deepseek-v4-flash", "deepseek-v4-flash", "deepseek-v3-flash", "standalone-low", "longmodelname-20b", "longmodelname-21b", "abcdefghijklmnopqrst", "abcdefghijklmnopqrsx"}
	seen := make(chan string, len(names))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body.Model
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	gateway, manager := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: upstream.URL, Models: names, AuthMode: "none"}}
	})
	before, err := json.Marshal(manager.Get().Upstreams)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"abcdefghijklmnopqrst", "deepseek-v3-flash", "deepseek-v4-flash", "longmodelname-20b", "longmodelname-21b", "standalone-low"}
	if got := modelsListIDs(t, gateway); !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	after, err := json.Marshal(manager.Get().Upstreams)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("listing changed routing catalog")
	}
	for _, name := range names {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+name+`","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		gateway.ServeOpenAI(res, req)
		if res.Code != 200 {
			t.Fatalf("%s: %d %s", name, res.Code, res.Body.String())
		}
		select {
		case got := <-seen:
			if got != name {
				t.Fatalf("%s routed as %s", name, got)
			}
		default:
			t.Fatalf("%s was not routed", name)
		}
	}
}
