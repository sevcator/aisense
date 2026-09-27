package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/routehealth"
	"aisense/internal/store"
)

func testServer(t *testing.T) (*Server, *config.Manager) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.Admin.Username = "admin"
		c.Server.Admin.Password = "correct-horse"
		c.Server.Admin.Token = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{Cfg: cfg, Store: st, UI: []byte("<html>panel</html>"), Login: []byte("<html>login</html>")}, cfg
}

func TestLoggingSettingsRuntimePersistenceAndWriteFailure(t *testing.T) {
	srv, cfg := testServer(t)
	dir := t.TempDir()
	trace := debuglog.NewDynamic(dir, "", func() bool { return cfg.Get().LoggingEnabled })
	defer trace.Close()
	srv.Debug = trace
	if err := cfg.SetLoggingValidator(trace.Validate); err != nil {
		t.Fatal(err)
	}
	save := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
		req.SetBasicAuth("admin", "correct-horse")
		res := httptest.NewRecorder()
		srv.Handle(res, req)
		return res
	}
	if cfg.Get().LoggingEnabled {
		t.Fatal("logging should default off")
	}
	if res := save(`{"logging_enabled":true}`); res.Code != 200 {
		t.Fatalf("enable: %d %s", res.Code, res.Body.String())
	}
	if !trace.Enabled() {
		t.Fatal("logger did not enable")
	}
	if res := save(`{"usage":{"enabled":false}}`); res.Code != 200 || !trace.Enabled() {
		t.Fatal("partial settings reset logging")
	}
	// The Settings UI reads the same value through the state endpoint.
	stateReq := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	stateReq.SetBasicAuth("admin", "correct-horse")
	state := httptest.NewRecorder()
	srv.Handle(state, stateReq)
	if !strings.Contains(state.Body.String(), `"logging_enabled":true`) {
		t.Fatal("state missing logging setting")
	}
	trace.Event("", "enabled-marker", nil)
	if res := save(`{"logging_enabled":false}`); res.Code != 200 || trace.Enabled() {
		t.Fatal("disable failed")
	}
	before, _ := os.ReadFile(trace.Path())
	trace.Event("", "disabled-marker", nil)
	after, _ := os.ReadFile(trace.Path())
	if string(before) != string(after) {
		t.Fatal("disable still writes")
	}
	bad := debuglog.NewDynamic(filepath.Join(dir, "missing", "parent"), "", func() bool { return cfg.Get().LoggingEnabled })
	defer bad.Close()
	srv.Debug = bad
	if err := cfg.SetLoggingValidator(bad.Validate); err != nil {
		t.Fatal(err)
	}
	if res := save(`{"logging_enabled":true}`); res.Code != 500 || cfg.Get().LoggingEnabled {
		t.Fatalf("fake success on bad path: %d %s", res.Code, res.Body.String())
	}
}

func TestLoginPageAndBasicAPICompatibility(t *testing.T) {
	srv, _ := testServer(t)

	for _, path := range []string{"/", "/admin/api/state"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			res := httptest.NewRecorder()
			srv.Handle(res, req)
			want := http.StatusUnauthorized
			if path == "/" {
				want = http.StatusOK
			}
			if res.Code != want {
				t.Fatalf("status = %d, want %d", res.Code, want)
			}
			if got := res.Header().Get("WWW-Authenticate"); got != "" {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "login") {
		t.Fatalf("authorized panel: status=%d body=%q", res.Code, res.Body.String())
	}
}

func TestLegacyTokenHeaderIsNotAccepted(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.Header.Set("X-Admin-Token", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
}

func TestStateRedactsPanelPassword(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	var state config.Config
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Server.Admin.Password != "" || state.Server.Admin.Token != "" {
		t.Fatalf("admin secrets leaked: password=%q token=%q", state.Server.Admin.Password, state.Server.Admin.Token)
	}
	if state.Server.Admin.Username != "admin" {
		t.Fatalf("username = %q", state.Server.Admin.Username)
	}
}

func TestBlankPasswordInSettingsPreservesCurrentPassword(t *testing.T) {
	srv, cfg := testServer(t)
	body := `{"server":{"openai":{"enabled":true,"port":8080,"path":"/v1"},"anthropic":{"enabled":true,"port":8081,"path":"/v1"},"admin":{"enabled":true,"port":8082,"username":"operator","password":""}}}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	got := cfg.Get().Server.Admin
	if got.Username != "operator" || got.Password != "correct-horse" {
		t.Fatalf("admin auth = %#v", got)
	}
}

func TestAdminUIElementIDsAreUnique(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\bid="([^"]+)"`)
	seen := map[string]bool{}
	for _, match := range re.FindAllStringSubmatch(string(raw), -1) {
		id := match[1]
		if seen[id] {
			t.Fatalf("duplicate element id %q", id)
		}
		seen[id] = true
	}
}

func TestAdminUIHasNoPricingControls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"price-auto-enabled", "price-model-url", "price-rates-url", "price-refresh-btn", "pricing/refresh", "currency-list", "Automatic pricing", "refreshPricingNow"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("pricing UI must be removed, found %q", forbidden)
		}
	}
}

func TestAdminUIUsesUpstreamKeyPools(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"u-keys", "upstream-key-input", "api_keys", "keyCount"} {
		if !strings.Contains(text, required) {
			t.Fatalf("key-pool UI missing %q", required)
		}
	}
	for _, removed := range []string{"Automatically detect HTTP / HTTPS", "Admin brute-force protection", "auth-max-attempts", "auth-window-seconds"} {
		if strings.Contains(text, removed) {
			t.Fatalf("removed setting remains %q", removed)
		}
	}
	for _, removed := range []string{`id="u-key"`, `id="me-key"`} {
		if strings.Contains(text, removed) {
			t.Fatalf("legacy single-key UI remains: %q", removed)
		}
	}
}

func TestAdminUIStreamsTestResultsOnlyInModal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, removed := range []string{"ups-test-results", "ups-test-summary", "ups-test-live-body", "overlay-up-import", "SCHEMA CONFIRMED", "UNCONFIRMED"} {
		if strings.Contains(text, removed) {
			t.Fatalf("obsolete UI remains: %s", removed)
		}
	}
	for _, required := range []string{"upstreams/test-all/stream", "getReader()", "insertAdjacentHTML('afterbegin'", "test-modal-body", "test-modal-summary", "overlay-up-test", "AbortController", "prefers-reduced-motion", ">Add</button>", "updateTestTasks()", "Not verified"} {
		if !strings.Contains(text, required) {
			t.Fatalf("live Test All UI missing %q", required)
		}
	}
}

func TestAdminUIExposesHealthAndRequestedHitchanceControls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{`id="hc-enabled"`, `id="hc-start-timeout"`, `id="hc-confirmations"`, `id="hc-rules"`, `id="overlay-hc-rule"`, "response_start_timeout_seconds", "upstreams/health", "UPSTREAM_HEALTH", "refreshUpstreamHealth", "endpoint_state", "health.keys", "health.hit_chance", "k.hit_chance", `id="hc-rule-match"`, "match_any"} {
		if !strings.Contains(text, required) {
			t.Fatalf("health-aware UI missing %q", required)
		}
	}
	for _, removed := range []string{"retry_cycles", "cooling_retry_seconds", "upstream_cache_ttl_hours", "sticky_upstream_ttl_hours"} {
		if strings.Contains(text, removed) {
			t.Fatalf("removed Hitchance setting remains in UI: %s", removed)
		}
	}
}

func TestAdminUIExposesEditableUpstreamModelAliases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{`id="u-model-aliases"`, "formatModelAliases", "parseModelAliases", "canonical = alias1, alias2"} {
		if !strings.Contains(text, required) {
			t.Fatalf("model alias editor missing %q", required)
		}
	}
}

func TestUpstreamModelAliasEditorPersistsDifferentRouteNames(t *testing.T) {
	srv, cfg := testServer(t)
	body := `{"id":"alias-up","type":"openai","base_url":"https://example.com/v1","models":["primary-model","different-route"],"model_aliases":{"primary-model":["primary-model","different-route"]},"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	up := cfg.Get().Upstreams[0]
	if len(up.Models) != 1 || up.Models[0] != "primary-model" {
		t.Fatalf("models = %#v", up.Models)
	}
	if got := strings.Join(up.ModelAliases["primary-model"], ","); got != "primary-model,different-route" {
		t.Fatalf("aliases = %#v", up.ModelAliases)
	}
}

func TestAdminUIModelsAvailableUsesCachedUpstreamsWithoutTelemetry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{"Models</span>", "setDashboardModelMode('available')", "setDashboardModelMode('all')", "collectCachedModels", "DASHBOARD_CACHED_UPS_CACHE", "No cached models yet", "STATE?.presentation_models", "DASHBOARD_CACHED_MODEL_NAMES = data.presentation_models || []", "sortModels([...DASHBOARD_CACHED_MODEL_NAMES])", "let DASHBOARD_MODEL_MODE = 'available'"} {
		if !strings.Contains(text, required) {
			t.Fatalf("models UI missing %q", required)
		}
	}
	for _, obsolete := range []string{"debug/event", "page.view", "ui.click", "ui.change", "ui.input", "ui.keydown", "ui.api.request", "ui.api.response", "function.enter", "function.exit", "installDebugFunctionTracing"} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("removed UI telemetry remains: %q", obsolete)
		}
	}
	if strings.Contains(text, "Available Models") {
		t.Fatal("legacy Available Models heading remains")
	}
}

func TestAdminDebugTraceExcludesPayloadsAndUIEvents(t *testing.T) {
	srv, _ := testServer(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	trace, err := debuglog.NewRaw(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	srv.Debug = trace

	req := httptest.NewRequest(http.MethodGet, "/?x=1", nil)
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("panel status=%d body=%q", res.Code, res.Body.String())
	}

	uiReq := httptest.NewRequest(http.MethodPost, "/admin/api/debug/event", strings.NewReader(`{"type":"page.view","view":"settings","target":{"id":"navsettings"}}`))
	uiReq.Header.Set("Content-Type", "application/json")
	uiReq.SetBasicAuth("admin", "correct-horse")
	uiRes := httptest.NewRecorder()
	srv.Handle(uiRes, uiReq)
	if uiRes.Code != http.StatusOK {
		t.Fatalf("debug event status=%d body=%q", uiRes.Code, uiRes.Body.String())
	}
	if err := trace.Close(); err != nil {
		t.Fatal(err)
	}

	logged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(logged)
	for _, want := range []string{`"event":"admin.request"`, `"event":"admin.response"`, `"handler":"admin.serveUI"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug log missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"correct-horse", "navsettings", "page.view", "/?x=1", "Authorization"} {
		if strings.Contains(out, secret) {
			t.Fatalf("debug log leaked %q", secret)
		}
	}
}

func TestCRUDPreservesHiddenFieldsAndReportsMissingObjects(t *testing.T) {
	srv, cfg := testServer(t)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	lastUsed := created.Add(time.Hour)
	refreshed := created.Add(2 * time.Hour)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.Enabled = false
		c.Upstreams = []*config.Upstream{{
			ID: "up", Name: "Friendly", Type: "openai", BaseURL: "https://old.example/v1",
			APIKeys: []string{"secret"}, Models: []string{"model"}, Priority: 3, Enabled: true,
			FailCodes: []int{418}, AuthMode: "none", ModelsRefreshedAt: refreshed, ModelsRefreshError: "old warning",
		}}
		c.APIKeys = []*config.APIKey{{
			ID: "key", Name: "Old", Key: "sk-secret", Enabled: true,
			AllowedModels: []string{"*"}, CreatedAt: created, LastUsed: lastUsed,
		}}
		c.Proxies.List = []*config.ProxyEntry{{URL: "http://one.example:8080"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth("admin", "correct-horse")
		res := httptest.NewRecorder()
		srv.Handle(res, req)
		return res
	}

	res := request(http.MethodPost, "/admin/api/upstream?mode=edit", `{"id":"up","type":"openai","base_url":"https://new.example/v1","api_keys":["secret"],"models":["model-2"],"enabled":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit upstream status=%d body=%s", res.Code, res.Body.String())
	}
	up := cfg.Get().Upstreams[0]
	if up.Name != "Friendly" || up.Priority != 3 || up.AuthMode != "none" || len(up.FailCodes) != 1 || up.FailCodes[0] != 418 || !up.ModelsRefreshedAt.Equal(refreshed) || up.ModelsRefreshError != "old warning" {
		t.Fatalf("upstream hidden fields were not preserved: %#v", up)
	}
	if res := request(http.MethodPost, "/admin/api/upstream?mode=edit", `{"id":"missing","type":"openai","base_url":"https://new.example/v1","models":["model"],"enabled":true}`); res.Code != http.StatusNotFound {
		t.Fatalf("missing upstream edit status=%d body=%s", res.Code, res.Body.String())
	}
	if res := request(http.MethodDelete, "/admin/api/upstream?id=missing", ""); res.Code != http.StatusNotFound {
		t.Fatalf("missing upstream delete status=%d body=%s", res.Code, res.Body.String())
	}

	res = request(http.MethodPost, "/admin/api/key?mode=edit", `{"id":"key","name":"Edited","enabled":true,"allowed_models":["model"]}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit key status=%d body=%s", res.Code, res.Body.String())
	}
	key := cfg.Get().APIKeys[0]
	if key.Key != "sk-secret" || !key.CreatedAt.Equal(created) || !key.LastUsed.Equal(lastUsed) {
		t.Fatalf("key credential/timestamps were not preserved: %#v", key)
	}
	if res := request(http.MethodPost, "/admin/api/key?mode=edit", `{"id":"missing","name":"Edited","enabled":true}`); res.Code != http.StatusNotFound {
		t.Fatalf("missing key edit status=%d body=%s", res.Code, res.Body.String())
	}
	if res := request(http.MethodDelete, "/admin/api/key?id=missing", ""); res.Code != http.StatusNotFound {
		t.Fatalf("missing key delete status=%d body=%s", res.Code, res.Body.String())
	}

	if res := request(http.MethodPost, "/admin/api/proxies", `{"urls":["not-a-proxy"]}`); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy add status=%d body=%s", res.Code, res.Body.String())
	}
	if res := request(http.MethodPost, "/admin/api/proxies", `{"urls":["socks5://two.example:1080"]}`); res.Code != http.StatusOK {
		t.Fatalf("proxy add status=%d body=%s", res.Code, res.Body.String())
	}
	if res := request(http.MethodPost, "/admin/api/proxies/update", `{"old_url":"socks5://two.example:1080","new_url":"http://one.example:8080"}`); res.Code != http.StatusBadRequest {
		t.Fatalf("duplicate proxy edit status=%d body=%s", res.Code, res.Body.String())
	}
	if res := request(http.MethodDelete, "/admin/api/proxies?url="+url.QueryEscape("http://missing.example:8080"), ""); res.Code != http.StatusNotFound {
		t.Fatalf("missing proxy delete status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestUpstreamHealthEndpointAndSettingsValidation(t *testing.T) {
	srv, cfg := testServer(t)
	up := &config.Upstream{ID: "up", AuthMode: "none", Type: "openai", BaseURL: "https://example.com/v1", Models: []string{"model"}, Enabled: true}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{up}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv.healthManager().MarkFailure(cfg.Get().Upstreams[0], "model", routehealth.Model, time.Minute, "model unavailable")
	req := httptest.NewRequest(http.MethodGet, "/admin/api/upstreams/health", nil)
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"degraded"`) || !strings.Contains(res.Body.String(), `"model":"model"`) {
		t.Fatalf("health response status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"hitchance":{"retry_cycles":11,"response_start_timeout_seconds":20}}`))
	req.SetBasicAuth("admin", "correct-horse")
	res = httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "retry_cycles") {
		t.Fatalf("invalid settings status=%d body=%s", res.Code, res.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(`{"failover":{"mode":"stop_on_selected","default_fail_codes":[500]}}`))
	req.SetBasicAuth("admin", "correct-horse")
	res = httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "failover") {
		t.Fatalf("legacy settings not rejected: status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestRefreshUpstreamModelsUpdatesModelsWithoutDisablingUpstream(t *testing.T) {
	modelsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer provider-key" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"}]}`))
	}))
	defer modelsAPI.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.Enabled = true
		c.Upstreams = []*config.Upstream{{ID: "up", Name: "up", Type: "openai", BaseURL: modelsAPI.URL + "/v1", APIKeys: []string{"provider-key"}, Models: []string{"*"}, Enabled: true, AuthMode: "swap"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := srv.refreshUpstreamModels(context.Background(), []string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Updated != 1 {
		t.Fatalf("updated = %d", summary.Updated)
	}
	up := cfg.Get().Upstreams[0]
	if !up.Enabled {
		t.Fatalf("model refresh disabled upstream: %#v", up)
	}
	if strings.Join(up.Models, ",") != "model-a,model-b" {
		t.Fatalf("models = %#v", up.Models)
	}
}

func TestRefreshUpstreamModelsFailureDoesNotDisableOrClearModels(t *testing.T) {
	badAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer badAPI.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "opencode", Name: "opencode", Type: "openai", BaseURL: badAPI.URL + "/v1", APIKeys: []string{"bad"}, Models: []string{"kept-model"}, Enabled: true, AuthMode: "swap"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := srv.refreshUpstreamModels(context.Background(), []string{"opencode"})
	if err == nil {
		t.Fatalf("expected refresh error")
	}
	if summary.Updated != 0 {
		t.Fatalf("updated = %d", summary.Updated)
	}
	up := cfg.Get().Upstreams[0]
	if !up.Enabled || strings.Join(up.Models, ",") != "kept-model" {
		t.Fatalf("failed refresh mutated availability: %#v", up)
	}
}

func TestRefreshUpstreamModelsUnionsSuccessfulKeyPools(t *testing.T) {
	modelsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer key-one":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "Bearer key-two":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"}]}`))
		default:
			http.Error(w, "invalid key", http.StatusUnauthorized)
		}
	}))
	defer modelsAPI.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Type: "openai", BaseURL: modelsAPI.URL, APIKeys: []string{"bad", "key-one", "key-two"}, Models: []string{"*"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := srv.refreshUpstreamModels(context.Background(), []string{"up"})
	if err != nil {
		t.Fatalf("one bad key must not fail pooled discovery: %v", err)
	}
	if summary.Updated != 1 || strings.Join(cfg.Get().Upstreams[0].Models, ",") != "model-a,model-b" {
		t.Fatalf("summary=%#v upstream=%#v", summary, cfg.Get().Upstreams[0])
	}
	if len(cfg.Get().Upstreams[0].ModelAliases["model-a"]) == 0 || len(cfg.Get().Upstreams[0].ModelAliases["model-b"]) == 0 {
		t.Fatalf("discovery lost raw model aliases: %#v", cfg.Get().Upstreams[0].ModelAliases)
	}
}

func TestStreamTestAllUpstreamsEmitsLiveNDJSONWithBoundedConcurrency(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"provider/glm-5-2"},{"id":"glm-5.2"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer api.Close()

	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		for i := 0; i < 12; i++ {
			c.Upstreams = append(c.Upstreams, &config.Upstream{
				ID: fmt.Sprintf("up-%d", i), Type: "openai", BaseURL: fmt.Sprintf("%s/up/%d", api.URL, i),
				Models: []string{"*"}, Enabled: true, AuthMode: "none",
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/test-all/stream", strings.NewReader(`{}`))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("status=%d content-type=%q body=%s", res.Code, res.Header().Get("Content-Type"), res.Body.String())
	}
	types := []string{}
	results := 0
	scanner := bufio.NewScanner(strings.NewReader(res.Body.String()))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", scanner.Text(), err)
		}
		typ, _ := event["type"].(string)
		types = append(types, typ)
		if typ == "result" {
			results++
			result, _ := event["result"].(map[string]any)
			if _, ok := result["capabilities"]; !ok {
				t.Fatalf("stream result missing capabilities: %#v", result)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	// Two capability progress events per upstream, plus start/results/complete.
	if len(types) != 38 || types[0] != "start" || types[len(types)-1] != "complete" || results != 12 {
		t.Fatalf("stream types=%#v results=%d", types, results)
	}
	if got := maxActive.Load(); got < 2 || got > 8 {
		t.Fatalf("max concurrency = %d, want 2..8", got)
	}
	// Schema checks do not claim model availability or update routing health.
}

func TestStreamTestAllUpstreamsStopsOnClientCancellation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer api.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		for i := 0; i < 16; i++ {
			c.Upstreams = append(c.Upstreams, &config.Upstream{
				ID: fmt.Sprintf("cancel-%d", i), Type: "openai", BaseURL: fmt.Sprintf("%s/%d", api.URL, i),
				Models: []string{"*"}, Enabled: true, AuthMode: "none",
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/test-all/stream", strings.NewReader(`{}`)).WithContext(ctx)
	res := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.streamTestAllUpstreams(res, req)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not stop after client cancellation")
	}
}

func TestUpstreamTestDoesNotGenerateOrRetryModelAliases(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.2"},{"id":"aug/glm-5.2"},{"id":"unrelated-model"}]}`))
			return
		}
		var request struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		seen = append(seen, request.Model)
		mu.Unlock()
		if request.Model != "" {
			t.Errorf("probe must not generate: %q", request.Model)
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"messages must be an array"}}`))
	}))
	defer api.Close()

	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Type: "openai", BaseURL: api.URL, Models: []string{"*"}, Enabled: true, AuthMode: "none"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := srv.testOneUpstream(context.Background(), cfg.Get().Upstreams[0], "glm_5_2")
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("test result = %#v", result)
	}
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	if got != "," {
		t.Fatalf("alias attempts = %q", got)
	}
}

func TestRefreshUpstreamModelsSwitchesHTTPSToHTTP(t *testing.T) {
	modelsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"plain-model"}]}`))
	}))
	defer modelsAPI.Close()
	srv, cfg := testServer(t)
	badHTTPS := "https://" + strings.TrimPrefix(modelsAPI.URL, "http://")
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.AutoFixProblems = true
		c.Upstreams = []*config.Upstream{{ID: "up", Type: "openai", BaseURL: badHTTPS, APIKeys: []string{"key"}, Models: []string{"*"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := srv.refreshUpstreamModels(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Updated != 1 || cfg.Get().Upstreams[0].BaseURL != modelsAPI.URL {
		t.Fatalf("summary=%#v base=%q", summary, cfg.Get().Upstreams[0].BaseURL)
	}
	if got := summary.Results[0]["protocol_change"]; got != "HTTPS → HTTP" {
		t.Fatalf("protocol change = %#v", got)
	}
}

func TestRefreshUpstreamModelsPrefersHTTPSAndMergesTwinKeys(t *testing.T) {
	modelsAPI := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := ""
		switch r.Header.Get("Authorization") {
		case "Bearer http-key":
			model = "http-key-model"
		case "Bearer https-key":
			model = "https-key-model"
		default:
			http.Error(w, "invalid key", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
	}))
	defer modelsAPI.Close()
	httpsBase := modelsAPI.URL
	httpBase := "http://" + strings.TrimPrefix(httpsBase, "https://")
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.AutoFixProblems = true
		c.ModelDiscovery.IgnoreCertErrors = true
		c.Upstreams = []*config.Upstream{
			{ID: "http", Type: "openai", BaseURL: httpBase, APIKeys: []string{"http-key"}, Models: []string{"*"}, Enabled: true},
			{ID: "https", Type: "openai", BaseURL: httpsBase, APIKeys: []string{"https-key"}, Models: []string{"*"}, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ups := cfg.Get().Upstreams
	if len(ups) != 1 || !strings.HasPrefix(ups[0].BaseURL, "https://") {
		t.Fatalf("protocol twins were not consolidated: %#v", ups)
	}
	if got := strings.Join(ups[0].APIKeys, ","); got != "https-key,http-key" {
		t.Fatalf("merged keys = %q", got)
	}
	if got := strings.Join(ups[0].Models, ","); got != "http-key-model,https-key-model" {
		t.Fatalf("merged models = %q", got)
	}
}

func TestRefreshUpstreamModelsKeepsTwinsWhenNeitherProtocolWorks(t *testing.T) {
	badAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "broken", http.StatusBadGateway)
	}))
	defer badAPI.Close()
	httpBase := badAPI.URL
	httpsBase := "https://" + strings.TrimPrefix(httpBase, "http://")
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.AutoFixProblems = true
		c.Upstreams = []*config.Upstream{
			{ID: "http", Type: "openai", BaseURL: httpBase, APIKeys: []string{"one"}, Models: []string{"kept"}, Enabled: true},
			{ID: "https", Type: "openai", BaseURL: httpsBase, APIKeys: []string{"two"}, Models: []string{"kept"}, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refreshUpstreamModels(context.Background(), nil); err == nil {
		t.Fatal("expected aggregate discovery failure")
	}
	if len(cfg.Get().Upstreams) != 2 {
		t.Fatalf("failed twins were destructively merged: %#v", cfg.Get().Upstreams)
	}
}

func TestParseUpstreamImportFindsURLAndKeyInLooseText(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "same line", text: "https://api.example.com/v1 sk-example-key"},
		{name: "blank separated", text: "https://api.example.com/v1\n\nsk-example-key"},
		{name: "labels", text: "тут что-то: https://api.example.com/v1\nтут что-то: sk-example-key"},
		{name: "key first", text: "key: sk-example-key\nurl: https://api.example.com/v1"},
		{name: "bearer header", text: "Endpoint = https://api.example.com/v1\nAuthorization: Bearer sk-example-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUpstreamImport(tc.text)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if got.BaseURL != "https://api.example.com/v1" {
				t.Fatalf("base URL = %q", got.BaseURL)
			}
			if strings.Join(got.APIKeys, ",") != "sk-example-key" {
				t.Fatalf("api keys = %q", got.APIKeys)
			}
			if got.Type != "auto" || got.AuthMode != "swap" || !got.Enabled {
				t.Fatalf("defaults = %#v", got)
			}
		})
	}
}

func TestParseUpstreamImportsFindsMultipleUpstreamsAndNonSKKeys(t *testing.T) {
	res := parseUpstreamImports(`
https://api-ai.gitcode.com/v1 -F-HeL2hJ4B4sgxXxt7vm_Ls

url: https://one.example/v1
key: token_one_123456

тут что-то: https://two.example/v1
тут что-то: key-two-abcdef
`)
	if len(res.Skipped) != 0 {
		t.Fatalf("unexpected skips: %#v", res.Skipped)
	}
	got := res.Upstreams
	if len(got) != 3 {
		t.Fatalf("parsed %d upstreams: %#v", len(got), got)
	}
	want := []struct{ baseURL, apiKey string }{
		{"https://api-ai.gitcode.com/v1", "-F-HeL2hJ4B4sgxXxt7vm_Ls"},
		{"https://one.example/v1", "token_one_123456"},
		{"https://two.example/v1", "key-two-abcdef"},
	}
	for i := range want {
		if got[i].BaseURL != want[i].baseURL || strings.Join(got[i].APIKeys, ",") != want[i].apiKey {
			t.Fatalf("upstream %d = base_url %q keys %q, want %#v", i, got[i].BaseURL, got[i].APIKeys, want[i])
		}
	}
}

func TestImportUpstreamEndpointCreatesUniqueUpstream(t *testing.T) {
	srv, cfg := testServer(t)
	body := `{"text":"url: https://api.example.com/v1\nkey: sk-example-key"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	if got := cfg.Get().Upstreams; len(got) != 1 || got[0].ID != "import-api-example-com" || got[0].BaseURL != "https://api.example.com/v1" || strings.Join(got[0].APIKeys, ",") != "sk-example-key" {
		t.Fatalf("imported upstream = %#v", got)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(body))
	secondReq.SetBasicAuth("admin", "correct-horse")
	secondRes := httptest.NewRecorder()
	srv.Handle(secondRes, secondReq)
	if secondRes.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%q", secondRes.Code, secondRes.Body.String())
	}
	if got := cfg.Get().Upstreams; len(got) != 1 || got[0].ID != "import-api-example-com" {
		t.Fatalf("duplicate import should merge into one upstream: %#v", got)
	}
	if !strings.Contains(secondRes.Body.String(), `"merged":1`) {
		t.Fatalf("second import did not report merge: %s", secondRes.Body.String())
	}
}

func TestImportUpstreamEndpointCreatesMultipleUpstreams(t *testing.T) {
	srv, cfg := testServer(t)
	body := `{"text":"https://api-ai.gitcode.com/v1 -F-HeL2hJ4B4sgxXxt7vm_Ls\n\nurl: https://one.example/v1\nkey: token_one_123456"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	got := cfg.Get().Upstreams
	if len(got) != 2 {
		t.Fatalf("imported %d upstreams: %#v", len(got), got)
	}
	if got[0].BaseURL != "https://api-ai.gitcode.com/v1" || strings.Join(got[0].APIKeys, ",") != "-F-HeL2hJ4B4sgxXxt7vm_Ls" {
		t.Fatalf("first upstream = %#v", got[0])
	}
	if got[1].BaseURL != "https://one.example/v1" || strings.Join(got[1].APIKeys, ",") != "token_one_123456" {
		t.Fatalf("second upstream = %#v", got[1])
	}
	if !strings.Contains(res.Body.String(), `"imported":2`) {
		t.Fatalf("response missing imported count: %s", res.Body.String())
	}
}

// TestImportUpstreamAutoSkipsBadEntriesWithoutAbortingBatch is the core mass
// import contract: a paste with several URL blocks where one has no
// resolvable API key must still import every OTHER good entry, and report
// the bad one in "errors" instead of failing the whole request.
func TestImportUpstreamAutoSkipsBadEntriesWithoutAbortingBatch(t *testing.T) {
	srv, cfg := testServer(t)
	body := `{"text":"https://good-one.example/v1 sk-abcdef123456\n\nhttps://no-key-here.example/v1 contact admin@example.com for access\n\nhttps://good-two.example/v1 token_xyz_789012"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	got := cfg.Get().Upstreams
	if len(got) != 2 {
		t.Fatalf("expected 2 successfully imported upstreams (bad entry auto-skipped), got %d: %#v", len(got), got)
	}
	if got[0].BaseURL != "https://good-one.example/v1" || got[1].BaseURL != "https://good-two.example/v1" {
		t.Fatalf("unexpected upstreams imported: %#v", got)
	}
	if !strings.Contains(res.Body.String(), `"imported":2`) {
		t.Fatalf("response missing imported count: %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"skipped":1`) {
		t.Fatalf("response missing skipped count: %s", res.Body.String())
	}
}

func TestImportUpstreamRefreshesModelsWhenDiscoveryEnabled(t *testing.T) {
	modelsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer dynamic-key" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"dynamic-model"}]}`))
	}))
	defer modelsAPI.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.Enabled = true
		c.ModelDiscovery.RefreshIntervalMinutes = 60
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"text":"%s/v1 dynamic-key"}`, modelsAPI.URL)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartModelDiscovery(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for cfg.Get().Upstreams[0].ModelsRefreshedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	up := cfg.Get().Upstreams[0]
	if strings.Join(up.Models, ",") != "dynamic-model" || !up.Enabled {
		t.Fatalf("import did not refresh models without disabling: %#v", up)
	}
	if !strings.Contains(res.Body.String(), `"models_discovery_queued":true`) {
		t.Fatalf("response missing queued discovery: %s", res.Body.String())
	}
}

func TestMassEditUpstreamsUpdatesSelectedThenAll(t *testing.T) {
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{
			{ID: "one", Name: "one", Type: "openai", BaseURL: "https://one.example/v1", Models: []string{"gpt-4"}, Priority: 1, Enabled: true, AuthMode: "swap"},
			{ID: "two", Name: "two", Type: "anthropic", BaseURL: "https://two.example/v1", Models: []string{"claude"}, Priority: 2, Enabled: true, AuthMode: "swap"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"ids":["one"],"patch":{"models":{"mode":"add","values":["gpt-5","gpt-4"]},"blocked_models":{"mode":"replace","values":["gpt-4"]},"priority":42,"enabled":false,"use_proxy":true,"auth_mode":"none","fail_codes":[418,429]}}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/mass-edit", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
	one, two := cfg.Get().Upstreams[0], cfg.Get().Upstreams[1]
	if strings.Join(one.Models, ",") != "gpt-4,gpt-5" || strings.Join(one.BlockedModels, ",") != "gpt-4" {
		t.Fatalf("selected models = %#v blocked = %#v", one.Models, one.BlockedModels)
	}
	if one.Priority != 42 || one.Enabled || !one.UseProxy || one.AuthMode != "none" || len(one.FailCodes) != 2 || one.FailCodes[0] != 418 || one.FailCodes[1] != 429 {
		t.Fatalf("selected settings = %#v", one)
	}
	if two.Priority != 2 || !two.Enabled || two.UseProxy || two.AuthMode != "swap" || strings.Join(two.Models, ",") != "claude" || len(two.BlockedModels) != 0 {
		t.Fatalf("unselected upstream changed = %#v", two)
	}

	allReq := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams/mass-edit", strings.NewReader(`{"all":true,"patch":{"enabled":true}}`))
	allReq.SetBasicAuth("admin", "correct-horse")
	allRes := httptest.NewRecorder()
	srv.Handle(allRes, allReq)
	if allRes.Code != http.StatusOK {
		t.Fatalf("all status=%d body=%q", allRes.Code, allRes.Body.String())
	}
	for _, up := range cfg.Get().Upstreams {
		if !up.Enabled {
			t.Fatalf("all edit did not enable %#v", up)
		}
	}
}

func TestUpstreamTestHonorsAuthModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		up         *config.Upstream
		wantHeader string
		wantValue  string
	}{
		{name: "none sends no auth", up: &config.Upstream{ID: "up", Name: "up", Type: "openai", Models: []string{"test-model"}, Enabled: true, AuthMode: "none"}, wantHeader: "Authorization", wantValue: ""},
		{name: "swap sends api key", up: &config.Upstream{ID: "up", Name: "up", Type: "openai", APIKeys: []string{"provider-key"}, Models: []string{"test-model"}, Enabled: true, AuthMode: "swap"}, wantHeader: "Authorization", wantValue: "Bearer provider-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantHeader, wantValue := tc.wantHeader, tc.wantValue
				if strings.HasSuffix(r.URL.Path, "/messages") {
					wantHeader = "X-Api-Key"
					wantValue = strings.TrimPrefix(wantValue, "Bearer ")
				}
				if got := r.Header.Get(wantHeader); got != wantValue {
					t.Fatalf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
				}
				if got := r.Header.Get("User-Agent"); got != "OpenAI/Python 1.99.0" {
					t.Fatalf("User-Agent = %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"messages must be an array"}}`))
			}))
			defer api.Close()
			srv, cfg := testServer(t)
			if err := cfg.Update(func(c *config.Config) error {
				up := *tc.up
				up.BaseURL = api.URL + "/v1"
				c.Upstreams = []*config.Upstream{&up}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/test", strings.NewReader(`{"id":"up"}`))
			req.SetBasicAuth("admin", "correct-horse")
			res := httptest.NewRecorder()
			srv.Handle(res, req)
			if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"ok":true`) {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestSettingsCanRotateBrowserCredentials(t *testing.T) {
	srv, _ := testServer(t)
	body := `{"server":{"openai":{"enabled":true,"port":8080,"path":"/v1"},"anthropic":{"enabled":true,"port":8081,"path":"/v1"},"admin":{"enabled":true,"port":8082,"username":"operator","password":"new-browser-password"}}}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("rotate credentials: status=%d body=%q", res.Code, res.Body.String())
	}

	oldReq := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	oldReq.SetBasicAuth("admin", "correct-horse")
	oldRes := httptest.NewRecorder()
	srv.Handle(oldRes, oldReq)
	if oldRes.Code != http.StatusUnauthorized {
		t.Fatalf("old credentials status=%d, want 401", oldRes.Code)
	}

	newReq := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	newReq.SetBasicAuth("operator", "new-browser-password")
	newRes := httptest.NewRecorder()
	srv.Handle(newRes, newReq)
	if newRes.Code != http.StatusOK {
		t.Fatalf("new credentials status=%d body=%q", newRes.Code, newRes.Body.String())
	}
}

type mockProxyInfo struct {
	used   []string
	routes []CachedRouteEntry
}

func (m *mockProxyInfo) UsedModels() []string                { return m.used }
func (m *mockProxyInfo) CachedUpstreams() []CachedRouteEntry { return m.routes }

func TestCachedRoutesEndpointAndStatsUsedModels(t *testing.T) {
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up-1", Enabled: true, Models: []string{"gpt-5.6-sol", "claude-opus-4.6"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv.Proxy = &mockProxyInfo{
		used: []string{"gpt-5.6-sol", "claude-opus-4.6"},
		routes: []CachedRouteEntry{
			{UpstreamID: "up-1", Model: "gpt-5.6-sol", CachedAt: time.Now()},
		},
	}

	// Test GET /admin/api/cached-routes
	req := httptest.NewRequest(http.MethodGet, "/admin/api/cached-routes", nil)
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("cached-routes status = %d body = %s", res.Code, res.Body.String())
	}
	var cRes struct {
		Routes   []CachedRouteEntry `json:"routes"`
		TTLHours int                `json:"ttl_hours"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &cRes); err != nil {
		t.Fatal(err)
	}
	if len(cRes.Routes) != 1 || cRes.Routes[0].Model != "gpt-5.6-sol" {
		t.Fatalf("expected 1 route gpt-5.6-sol, got %#v", cRes.Routes)
	}
	if cRes.TTLHours != 24 {
		t.Fatalf("expected TTLHours = 24, got %d", cRes.TTLHours)
	}

	// Test GET /admin/api/stats contains used_models
	sReq := httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil)
	sReq.SetBasicAuth("admin", "correct-horse")
	sRes := httptest.NewRecorder()
	srv.Handle(sRes, sReq)
	if sRes.Code != http.StatusOK {
		t.Fatalf("stats status = %d body = %s", sRes.Code, sRes.Body.String())
	}
	var statsRes struct {
		UsedModels []string `json:"used_models"`
	}
	if err := json.Unmarshal(sRes.Body.Bytes(), &statsRes); err != nil {
		t.Fatal(err)
	}
	if len(statsRes.UsedModels) != 2 || statsRes.UsedModels[0] != "gpt-5.6-sol" {
		t.Fatalf("expected 2 used models, got %#v", statsRes.UsedModels)
	}
}

func TestSettingsHitchanceTTLPersistence(t *testing.T) {
	srv, cfg := testServer(t)

	// Valid TTL configuration
	body := `{"hitchance":{"retry_cycles":2,"response_start_timeout_seconds":20,"upstream_cache_ttl_hours":48,"sticky_upstream_ttl_hours":12}}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("settings save status = %d body = %s", res.Code, res.Body.String())
	}

	fo := cfg.Get().Hitchance
	if fo.UpstreamCacheTTLHours != 48 || fo.StickyUpstreamTTLHours != 12 {
		t.Fatalf("expected TTLs (48, 12), got (%d, %d)", fo.UpstreamCacheTTLHours, fo.StickyUpstreamTTLHours)
	}

	// Invalid TTL range (> 720 hours)
	badBody := `{"hitchance":{"retry_cycles":2,"response_start_timeout_seconds":20,"upstream_cache_ttl_hours":1000}}`
	badReq := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(badBody))
	badReq.SetBasicAuth("admin", "correct-horse")
	badRes := httptest.NewRecorder()
	srv.Handle(badRes, badReq)
	if badRes.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range TTL, got %d", badRes.Code)
	}
}

func TestQueueUndiscoveredUpstreams(t *testing.T) {
	srv, cfg := testServer(t)
	now := time.Now()
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{
			{ID: "fresh", Enabled: true, Models: []string{"*"}},
			{ID: "discovered", Enabled: true, Models: []string{"gpt-x"}, ModelsRefreshedAt: now},
			{ID: "disabled", Enabled: false, Models: []string{"*"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv.queueUndiscoveredUpstreams()
	srv.discoveryMu.Lock()
	defer srv.discoveryMu.Unlock()
	if len(srv.discoveryPending) != 1 || !srv.discoveryPending["fresh"] {
		t.Fatalf("pending = %#v, want only fresh", srv.discoveryPending)
	}
}

func TestDiscoveryLeavesInferenceHealthUnverified(t *testing.T) {
	modelsAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer modelsAPI.Close()
	srv, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Name: "up", Type: "openai", BaseURL: modelsAPI.URL + "/v1", APIKeys: []string{"k"}, Models: []string{"*"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.refreshUpstreamModels(context.Background(), []string{"up"}); err != nil {
		t.Fatal(err)
	}
	status := ""
	for _, rec := range srv.healthManager().Snapshot(cfg.Get().Upstreams) {
		if rec.ID == "up" {
			status = rec.Status
		}
	}
	if status != "ready" {
		t.Fatalf("health status after discovery = %q, want ready", status)
	}

	// A failed discovery must not mark the upstream available.
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer failed.Close()
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = append(c.Upstreams, &config.Upstream{ID: "dead", Name: "dead", Type: "openai", BaseURL: failed.URL + "/v1", APIKeys: []string{"k"}, Models: []string{"*"}, Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = srv.refreshUpstreamModels(context.Background(), []string{"dead"})
	for _, rec := range srv.healthManager().Snapshot(cfg.Get().Upstreams) {
		if rec.ID == "dead" && rec.Status == "available" {
			t.Fatal("failed discovery must not mark upstream available")
		}
	}
}
