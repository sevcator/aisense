package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCombosAreCreatedRenamedAndDeleted(t *testing.T) {
	srv, cfg := testServer(t)
	call := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetBasicAuth("admin", "correct-horse")
		res := httptest.NewRecorder()
		srv.Handle(res, req)
		return res
	}
	if res := call(http.MethodPost, "/admin/api/combo", `{"name":"favourite","models":["kimi-k3","glm-5.3"]}`); res.Code != 200 {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	if models := cfg.Get().ComboModels("favourite"); strings.Join(models, ",") != "kimi-k3,glm-5.3" {
		t.Fatalf("created combo = %v", models)
	}
	// The state the panel loads carries the combos.
	res := call(http.MethodGet, "/admin/api/state", "")
	if !strings.Contains(res.Body.String(), `"combos"`) || !strings.Contains(res.Body.String(), "favourite") {
		t.Fatalf("state = %s", res.Body.String())
	}
	// Saving under a new name renames the combo instead of adding a second one.
	if res := call(http.MethodPost, "/admin/api/combo", `{"name":"daily","models":["glm-5.3"],"original_name":"favourite"}`); res.Code != 200 {
		t.Fatalf("rename: %d %s", res.Code, res.Body.String())
	}
	if combos := cfg.Get().Combos; len(combos) != 1 || combos[0].Name != "daily" {
		t.Fatalf("after rename = %+v", combos)
	}
	// Invalid combos are refused with the reason, and nothing is saved.
	res = call(http.MethodPost, "/admin/api/combo", `{"name":"empty","models":[]}`)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "at least one model") {
		t.Fatalf("invalid: %d %s", res.Code, res.Body.String())
	}
	if len(cfg.Get().Combos) != 1 {
		t.Fatalf("combos = %+v", cfg.Get().Combos)
	}
	if res := call(http.MethodDelete, "/admin/api/combo?name=daily", ""); res.Code != 200 {
		t.Fatalf("delete: %d %s", res.Code, res.Body.String())
	}
	if len(cfg.Get().Combos) != 0 {
		t.Fatalf("after delete = %+v", cfg.Get().Combos)
	}
	if res := call(http.MethodDelete, "/admin/api/combo?name=daily", ""); res.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d %s", res.Code, res.Body.String())
	}
}
