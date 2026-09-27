package admin

import (
	"aisense/internal/config"
	"aisense/internal/hitchance"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHitchancePreviewAndResetAreAuthenticatedAndScoped(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update(func(c *config.Config) error {
		c.Server.Admin.Username = "test"
		c.Server.Admin.Password = "test-password"
		c.Upstreams = []*config.Upstream{{ID: "up", BaseURL: "https://one.invalid", AuthMode: "swap", APIKeys: []string{"private-key"}, Enabled: true}}
		return nil
	})
	s := &Server{Cfg: cfg}
	request := func(path, body string, auth bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/admin/api/"+path, strings.NewReader(body))
		if auth {
			r.SetBasicAuth("test", "test-password")
		}
		s.Handle(w, r)
		return w
	}
	if w := request("hitchance/preview", `{"status":401,"body":"Your key is invalid"}`, false); w.Code != 401 {
		t.Fatal("preview bypassed auth")
	}
	if w := request("hitchance/preview", `{"status":401,"body":"Your key is invalid"}`, true); w.Code != 200 || !strings.Contains(w.Body.String(), `"action":"delete"`) {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if len(cfg.Get().Upstreams[0].APIKeys) != 1 {
		t.Fatal("preview mutated keys")
	}
	cfg.ObserveHitchance(cfg.Get().Upstreams[0], "private-key", "model", hitchance.Decision{RuleID: "quota", Category: "quota", Action: "demote", Scope: "key", Cooldown: time.Hour})
	if w := request("hitchance/reset", `{"upstream_id":"up"}`, true); w.Code != 200 {
		t.Fatalf("reset %d", w.Code)
	}
	if len(cfg.Get().Upstreams[0].HitchanceState) != 0 || len(cfg.Get().Upstreams[0].APIKeys) != 1 {
		t.Fatal("reset must clear health, not credentials")
	}
}
func TestHitchanceSettingsPartialAndValidation(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: cfg}
	w := httptest.NewRecorder()
	s.saveSettings(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"hitchance":{"enabled":false}}`)))
	if w.Code != 200 || cfg.Get().Hitchance.Enabled || len(cfg.Get().Hitchance.Rules) == 0 {
		t.Fatal("hitchance partial update not saved/preserved")
	}
	for _, body := range []string{`{"hitchance":{"invalid_confirmations":0}}`, `{"hitchance":{"typo":true}}`, `{"hitchance":{"rules":[{"id":"bad","action":"delete","scope":"key","category":"bad","cooldown_seconds":1,"status_codes":[403]}]}}`} {
		w = httptest.NewRecorder()
		s.saveSettings(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("unsafe config accepted: %d %s", w.Code, body)
		}
	}
}
