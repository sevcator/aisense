package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHitchanceSchedulerSettingsPartialAndRetiredPolicy(t *testing.T) {
	s, cfg := testServer(t)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		r.SetBasicAuth("admin", "correct-horse")
		w := httptest.NewRecorder()
		s.Handle(w, r)
		return w
	}
	if w := post(`{"hitchance":{"retry_cycles":4,"response_start_timeout_seconds":32,"upstream_cache_ttl_hours":48,"sticky_upstream_ttl_hours":12}}`); w.Code != 200 {
		t.Fatalf("save=%d %s", w.Code, w.Body.String())
	}
	if w := post(`{"hitchance":{"enabled":false}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	hc := cfg.Get().Hitchance
	if hc.Enabled || hc.RetryCycles != 0 || hc.ResponseStartTimeoutSeconds != 32 || hc.UpstreamCacheTTLHours != 48 || hc.StickyUpstreamTTLHours != 12 {
		t.Fatalf("partial discarded scheduler: %+v", hc)
	}
	before, _ := json.Marshal(cfg.Get())
	for _, old := range []string{`{}`, `null`, `{"mode":"none"}`, `{"retry_cycles":5}`} {
		w := post(`{"failover":` + old + `,"hitchance":{"retry_cycles":2}}`)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "failover") {
			t.Fatalf("retired policy not clearly rejected: %d %s", w.Code, w.Body.String())
		}
		after, _ := json.Marshal(cfg.Get())
		if string(before) != string(after) {
			t.Fatal("rejected settings mutated config")
		}
	}
	r := httptest.NewRequest("GET", "/admin/api/state", nil)
	r.SetBasicAuth("admin", "correct-horse")
	w := httptest.NewRecorder()
	s.Handle(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"failover"`) {
		t.Fatalf("canonical settings output: %d %s", w.Code, w.Body.String())
	}
}
