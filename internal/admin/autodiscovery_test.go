package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"aisense/internal/config"
)

type fakeAutoSource struct{ calls int }

func (s *fakeAutoSource) Refresh(context.Context) ([]string, error) {
	s.calls++
	return []string{"auto/best-coding", "auto/best-reasoning"}, nil
}

func TestAutoDiscoveryEnabledRoutesOnly(t *testing.T) {
	srv, cfg := testServer(t)
	source := &fakeAutoSource{}
	srv.autoDiscovery = source
	response := `{"data":[{"id":"auto/best-coding"},{"id":"new-ordinary"}]}`
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(404)
			return
		}
		calls++
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, response)
	}))
	defer api.Close()
	if err := cfg.Update(func(c *config.Config) error {
		c.AutoModelsDiscovery = true
		c.Upstreams = []*config.Upstream{
			{ID: "on", Type: "openai", BaseURL: api.URL + "/v1", Enabled: true, Models: []string{"kept-ordinary"}},
			{ID: "off", Type: "openai", BaseURL: api.URL + "/disabled/v1", Enabled: false},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	up := cfg.Get().Upstreams[0]
	if calls != 1 || strings.Join(up.Models, ",") != "best-coding,kept-ordinary" || strings.Join(up.ModelAliases["best-coding"], ",") != "auto/best-coding" {
		t.Fatalf("calls=%d upstream=%+v", calls, up)
	}
	response = `{"data":[{"id":"new-ordinary"}]}`
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Get().Upstreams[0].Models, ","); got != "kept-ordinary" {
		t.Fatal(got)
	}
	response = `{"data":[{"id":"best-reasoning"}]}`
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Get().Upstreams[0].ModelAliases["best-reasoning"]; len(got) != 0 {
		t.Fatalf("identity alias persisted: %v", got)
	}
	response = `invalid JSON`
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err == nil {
		t.Fatal("expected upstream failure")
	}
	if !slices.Contains(cfg.Get().Upstreams[0].Models, "best-reasoning") {
		t.Fatal("failure lost cached route")
	}
	response = `{"data":[]}`
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Get().Upstreams[0].Models, ","); got != "kept-ordinary" {
		t.Fatal(got)
	}
	response = `{"data":[{"id":"new-ordinary"}]}`
	if err := cfg.Update(func(c *config.Config) error { c.AutoModelsDiscovery = false; return nil }); err != nil {
		t.Fatal(err)
	}
	previous := source.calls
	if _, err := srv.refreshUpstreamModels(context.Background(), []string{"on"}); err != nil {
		t.Fatal(err)
	}
	if source.calls != previous {
		t.Fatal("off setting fetched source")
	}
}

func TestAutoDiscoverySettingsPartialSave(t *testing.T) {
	srv, cfg := testServer(t)
	if cfg.Get().AutoModelsDiscovery {
		t.Fatal("must default off")
	}
	for _, body := range []string{`{"auto_models_discovery":true}`, `{"usage":{"enabled":false}}`, `{"model_discovery":{"enabled":false}}`} {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		req.SetBasicAuth("admin", "correct-horse")
		w := httptest.NewRecorder()
		srv.Handle(w, req)
		if w.Code != 200 || !cfg.Get().AutoModelsDiscovery {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
}
