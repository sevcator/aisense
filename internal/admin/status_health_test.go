package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/routehealth"
)

func statusHealthServer(t testing.TB, up *config.Upstream) *Server {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.Admin.Username = "status-admin"
		c.Server.Admin.Password = "synthetic-test-password"
		c.ModelDiscovery.Enabled = false
		c.AutoModelsDiscovery = false
		c.Upstreams = []*config.Upstream{up}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &Server{Cfg: cfg}
}

func statusHealthUp() *config.Upstream {
	return &config.Upstream{ID: "up", Type: "openai", BaseURL: "https://synthetic.invalid/v1", AuthMode: "swap", APIKeys: []string{"synthetic-key"}, Models: []string{"model"}, Enabled: true}
}

func getStatusHealth(t *testing.T, s *Server) map[string]json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/admin/api/upstreams/health", nil)
	r.SetBasicAuth("status-admin", "synthetic-test-password")
	w := httptest.NewRecorder()
	s.Handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("health HTTP %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Upstreams []map[string]json.RawMessage `json:"upstreams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Upstreams) != 1 {
		t.Fatalf("want one health record, got %s", w.Body.String())
	}
	return response.Upstreams[0]
}

func assertStatusHealth(t *testing.T, s *Server, status, reason string) map[string]json.RawMessage {
	t.Helper()
	rec := getStatusHealth(t, s)
	var gotStatus, gotReason string
	if err := json.Unmarshal(rec["status"], &gotStatus); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(rec["status_reason"], &gotReason)
	if gotStatus != status || gotReason != reason {
		t.Fatalf("want %s (%s), got %s (%s): %s", status, reason, gotStatus, gotReason, rec)
	}
	return rec
}

func TestStatusHealthConfigurationEligibility(t *testing.T) {
	for _, tc := range []struct {
		name           string
		edit           func(*config.Upstream)
		status, reason string
	}{
		{"missing swap keys", func(up *config.Upstream) { up.APIKeys = nil }, "unavailable", "No usable API keys are configured for swap authentication."},
		{"blank swap keys", func(up *config.Upstream) { up.APIKeys = []string{"", "  "} }, "unavailable", "No usable API keys are configured for swap authentication."},
		{"no models", func(up *config.Upstream) { up.Models = nil }, "unavailable", "No usable model routes are configured."},
		{"all routes blocked", func(up *config.Upstream) { up.BlockedModels = []string{"*"} }, "unavailable", "No usable model routes are configured."},
		{"partial configured block", func(up *config.Upstream) {
			up.Models = []string{"model", "other"}
			up.BlockedModels = []string{"model"}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"wildcard with configured block", func(up *config.Upstream) { up.Models = []string{"*"}; up.BlockedModels = []string{"model"} }, "degraded", "Some credentials or model routes are restricted."},
		{"none", func(up *config.Upstream) { up.AuthMode = "none"; up.APIKeys = nil }, "ready", "Eligible; inference availability has not been verified."},
		{"oauth", func(up *config.Upstream) { up.AuthMode = "oauth"; up.APIKeys = nil }, "ready", "Eligible; inference availability has not been verified."},
		{"passthrough", func(up *config.Upstream) { up.AuthMode = "passthrough"; up.APIKeys = nil }, "ready", "Eligible; inference availability has not been verified."},
		{"hidden is still routable", func(up *config.Upstream) { up.HiddenInvalid = true }, "ready", "Eligible; inference availability has not been verified."},
		{"disabled", func(up *config.Upstream) { up.Enabled = false; up.APIKeys = nil }, "disabled", "Disabled by configuration."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := statusHealthUp()
			tc.edit(up)
			s := statusHealthServer(t, up)
			rec := assertStatusHealth(t, s, tc.status, tc.reason)
			if _, ok := rec["last_success_at"]; ok {
				t.Fatal("configuration fabricated successful inference")
			}
		})
	}
}

func TestStatusHealthPersistedRestrictionEligibility(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name           string
		setup          func(*config.Upstream)
		status, reason string
	}{
		{"one key cooling", func(up *config.Upstream) {
			up.APIKeys = []string{"a", "b"}
			up.HitchanceState[config.HitchanceTarget("key", "a", "")] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "quota"}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"all keys cooling", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("key", "synthetic-key", "")] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "quota"}
		}, "unavailable", "All effective credentials are restricted."},
		{"all raw models cooling", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "model")] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "entitlement"}
		}, "unavailable", "All configured model routes are restricted."},
		{"one alias restricted", func(up *config.Upstream) {
			up.ModelAliases = map[string][]string{"model": {"provider/model", "other/model"}}
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "provider/model")] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "entitlement"}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"permanent model block", func(up *config.Upstream) {
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("synthetic-key"): {"model"}}
		}, "unavailable", "All configured model routes are restricted."},
		{"permanent block one key", func(up *config.Upstream) {
			up.APIKeys = []string{"synthetic-key", "other-key"}
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("synthetic-key"): {"model"}}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"mixed key and model restrictions", func(up *config.Upstream) {
			up.APIKeys = []string{"a", "b"}
			up.HitchanceState[config.HitchanceTarget("key", "a", "")] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now}
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("b"): {"model"}}
		}, "unavailable", "All configured model routes are restricted."},
		{"endpoint cooldown", func(up *config.Upstream) {
			up.HitchanceState["endpoint"] = hitchance.State{Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "server_error"}
		}, "unavailable", "Endpoint failure cooldown is active."},
		{"endpoint quarantine expired timestamp", func(up *config.Upstream) {
			up.HitchanceState["endpoint"] = hitchance.State{Action: "quarantine", Until: now.Add(-time.Hour), UpdatedAt: now, Category: "manual"}
		}, "unavailable", "Endpoint is quarantined until health reset."},
		{"wildcard is not exhausted by one raw block", func(up *config.Upstream) {
			up.Models = []string{"*"}
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("synthetic-key"): {"model"}}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"expired", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "model")] = hitchance.State{Action: "demote", Until: now.Add(-time.Hour), UpdatedAt: now}
		}, "ready", "Eligible; inference availability has not been verified."},
		{"stale identity", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "model")] = hitchance.State{Identity: "old-endpoint-identity", Action: "quarantine", UpdatedAt: now}
		}, "ready", "Eligible; inference availability has not been verified."},
		{"removed key", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("model", "removed-key", "model")] = hitchance.State{Action: "quarantine", UpdatedAt: now}
		}, "ready", "Eligible; inference availability has not been verified."},
		{"removed raw route", func(up *config.Upstream) {
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "provider/model")] = hitchance.State{Action: "quarantine", UpdatedAt: now}
		}, "ready", "Eligible; inference availability has not been verified."},
		{"raw colon and duplicate union", func(up *config.Upstream) {
			up.ModelAliases = map[string][]string{"model": {"provider/model:free", "other/model"}}
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("synthetic-key"): {"provider/model:free", "provider/model:free"}}
			up.HitchanceState[config.HitchanceTarget("model", "synthetic-key", "provider/model:free")] = hitchance.State{Action: "quarantine", UpdatedAt: now}
		}, "degraded", "Some credentials or model routes are restricted."},
		{"none effective credential", func(up *config.Upstream) {
			up.AuthMode = "none"
			up.APIKeys = nil
			up.HitchanceState[config.HitchanceTarget("model", "", "model")] = hitchance.State{Action: "quarantine", UpdatedAt: now}
		}, "unavailable", "All configured model routes are restricted."},
		{"none ignores stored keys", func(up *config.Upstream) {
			up.AuthMode = "none"
			up.HitchanceState[config.HitchanceTarget("key", "synthetic-key", "")] = hitchance.State{Action: "quarantine", UpdatedAt: now}
		}, "ready", "Eligible; inference availability has not been verified."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := statusHealthUp()
			up.HitchanceState = map[string]hitchance.State{}
			tc.setup(up)
			s := statusHealthServer(t, up)
			rec := assertStatusHealth(t, s, tc.status, tc.reason)
			if _, ok := rec["last_success_at"]; ok {
				t.Fatal("restriction fabricated successful inference")
			}
			if tc.status == "ready" {
				for _, field := range []string{"cooldown_until", "last_error", "hitchance_state"} {
					if _, ok := rec[field]; ok {
						t.Fatalf("inactive state leaked into %s: %s", field, rec)
					}
				}
			}
			if tc.name != "endpoint cooldown" {
				if _, ok := rec["cooldown_until"]; ok {
					t.Fatalf("non-endpoint/indefinite restriction claimed an endpoint cooldown: %s", rec)
				}
			}
		})
	}
}

func TestStatusHealthSeparatesEndpointAndKeyState(t *testing.T) {
	now := time.Now()
	up := statusHealthUp()
	up.APIKeys = []string{"first-key", "second-key", "third-key"}
	up.Models = []string{"model", "other"}
	up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("third-key"): {"other"}}
	up.HitchanceState = map[string]hitchance.State{
		"endpoint": {Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "transient", RuleID: "server-outage"},
		config.HitchanceTarget("key", "second-key", ""):       {Action: "quarantine", UpdatedAt: now, Category: "invalid_key", RuleID: "invalid-credential", Failures: 2},
		config.HitchanceTarget("model", "third-key", "model"): {Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "model", RuleID: "model-route"},
		config.HitchanceTarget("key", "removed-key", ""):      {Action: "quarantine", UpdatedAt: now, Category: "invalid_key"},
	}
	rec := getStatusHealth(t, statusHealthServer(t, up))
	for _, secret := range []string{"first-key", "second-key", "third-key", config.KeyFingerprint("second-key"), config.KeyFingerprint("third-key")} {
		for _, field := range []string{"endpoint_state", "keys"} {
			if strings.Contains(string(rec[field]), secret) {
				t.Fatalf("%s leaked a credential or fingerprint: %s", field, rec[field])
			}
		}
	}
	var endpoint hitchance.State
	if err := json.Unmarshal(rec["endpoint_state"], &endpoint); err != nil || endpoint.RuleID != "server-outage" {
		t.Fatalf("endpoint state = %s (%v)", rec["endpoint_state"], err)
	}
	var keys []hitchanceKeyHealth
	if err := json.Unmarshal(rec["keys"], &keys); err != nil || len(keys) != 3 {
		t.Fatalf("keys = %s (%v)", rec["keys"], err)
	}
	for i, key := range keys {
		if key.Index != i {
			t.Fatalf("key %d reported index %d", i, key.Index)
		}
	}
	if keys[0].State != nil || len(keys[0].Models) != 0 || keys[0].BlockedModels != 0 {
		t.Fatalf("healthy key inherited another scope: %+v", keys[0])
	}
	if keys[1].State == nil || keys[1].State.Action != "quarantine" || keys[1].State.Failures != 2 {
		t.Fatalf("key state not attributed to its key: %+v", keys[1])
	}
	if keys[2].State != nil || keys[2].Models["model"].RuleID != "model-route" || keys[2].BlockedModels != 1 {
		t.Fatalf("model restriction not attributed to its key: %+v", keys[2])
	}
	if _, ok := rec["endpoint_models"]; ok {
		t.Fatal("keyed upstream reported keyless model state")
	}

	keyless := statusHealthUp()
	keyless.AuthMode, keyless.APIKeys = "none", nil
	keyless.HitchanceState = map[string]hitchance.State{
		config.HitchanceTarget("model", "", "model"): {Action: "demote", Until: now.Add(time.Hour), UpdatedAt: now, Category: "model", RuleID: "model-route"},
	}
	rec = getStatusHealth(t, statusHealthServer(t, keyless))
	var models map[string]hitchance.State
	if err := json.Unmarshal(rec["endpoint_models"], &models); err != nil || models["model"].RuleID != "model-route" {
		t.Fatalf("keyless model state = %s (%v)", rec["endpoint_models"], err)
	}
	if _, ok := rec["keys"]; ok {
		t.Fatal("keyless upstream reported API keys")
	}
}

func TestStatusHealthReportsHitChancePerEndpointAndKey(t *testing.T) {
	up := statusHealthUp()
	up.APIKeys = []string{"first-key", "second-key", "idle-key"}
	s := statusHealthServer(t, up)
	if rec := getStatusHealth(t, s); rec["hit_chance"] != nil {
		t.Fatalf("no attempts must mean no figure: %s", rec["hit_chance"])
	}
	live := s.Cfg.Get().Upstreams[0]
	for i := 0; i < 3; i++ {
		s.healthManager().RecordAttempt(live, "first-key", true)
	}
	s.healthManager().RecordAttempt(live, "second-key", false)
	rec := getStatusHealth(t, s)
	var chance float64
	var attempts int
	if json.Unmarshal(rec["hit_chance"], &chance) != nil || json.Unmarshal(rec["attempts"], &attempts) != nil || chance != 0.75 || attempts != 4 {
		t.Fatalf("API base hit chance = %s over %s", rec["hit_chance"], rec["attempts"])
	}
	var keys []hitchanceKeyHealth
	if err := json.Unmarshal(rec["keys"], &keys); err != nil || len(keys) != 3 {
		t.Fatalf("keys = %s (%v)", rec["keys"], err)
	}
	if keys[0].HitChance == nil || *keys[0].HitChance != 1 || keys[0].Attempts != 3 {
		t.Fatalf("first key = %+v", keys[0])
	}
	if keys[1].HitChance == nil || *keys[1].HitChance != 0 || keys[1].Attempts != 1 {
		t.Fatalf("second key = %+v", keys[1])
	}
	if keys[2].HitChance != nil || keys[2].Attempts != 0 {
		t.Fatalf("idle key = %+v", keys[2])
	}
}

func TestStatusHealthObservedTransitionsAndImmediateReset(t *testing.T) {
	up := statusHealthUp()
	up.APIKeys = []string{"synthetic-key", "other-key"}
	s := statusHealthServer(t, up)
	s.healthManager().MarkSuccess(s.Cfg.Get().Upstreams[0], "model")
	rec := assertStatusHealth(t, s, "available", "Successful inference observed.")
	if _, ok := rec["last_success_at"]; !ok {
		t.Fatal("real inference evidence lost")
	}
	if err := s.Cfg.ObserveHitchance(s.Cfg.Get().Upstreams[0], "synthetic-key", "model", hitchance.Decision{RuleID: "test", Category: "quota", Action: "demote", Scope: "key", Cooldown: time.Hour}); err != nil {
		t.Fatal(err)
	}
	assertStatusHealth(t, s, "degraded", "Some credentials or model routes are restricted.")
	s.healthManager().MarkFailure(s.Cfg.Get().Upstreams[0], "model", routehealth.Global, time.Hour, "endpoint failure")
	assertStatusHealth(t, s, "unavailable", "Endpoint failure cooldown is active.")
	if err := s.Cfg.ObserveHitchance(s.Cfg.Get().Upstreams[0], "synthetic-key", "model", hitchance.Decision{RuleID: "test", Category: "test", Action: "quarantine", Scope: "endpoint"}); err != nil {
		t.Fatal(err)
	}
	assertStatusHealth(t, s, "unavailable", "Endpoint is quarantined until health reset.")
	r := httptest.NewRequest(http.MethodPost, "/admin/api/hitchance/reset", strings.NewReader(`{"upstream_id":"up"}`))
	r.SetBasicAuth("status-admin", "synthetic-test-password")
	w := httptest.NewRecorder()
	s.Handle(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("reset HTTP %d: %s", w.Code, w.Body.String())
	}
	rec = assertStatusHealth(t, s, "ready", "Eligible; inference availability has not been verified.")
	if len(s.Cfg.Get().Upstreams[0].HitchanceState) != 0 || len(s.Cfg.Get().Upstreams[0].APIKeys) != 2 {
		t.Fatal("reset did not clear only health restrictions")
	}
	for _, field := range []string{"cooldown_until", "last_error", "hitchance_state"} {
		if _, ok := rec[field]; ok {
			t.Fatalf("reset left active %s", field)
		}
	}
}

func TestStatusHealthPolicyDisabledKeepsPermanentBlocks(t *testing.T) {
	up := statusHealthUp()
	up.KeyBlockedModels = map[string][]string{config.KeyFingerprint("synthetic-key"): {"model"}}
	up.HitchanceState = map[string]hitchance.State{"endpoint": {Action: "quarantine", UpdatedAt: time.Now()}}
	s := statusHealthServer(t, up)
	if err := s.Cfg.Update(func(c *config.Config) error { c.Hitchance.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	rec := assertStatusHealth(t, s, "unavailable", "All configured model routes are restricted.")
	if _, ok := rec["hitchance_state"]; ok {
		t.Fatal("disabled policy presented active quarantine")
	}
	if err := s.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].KeyBlockedModels = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	assertStatusHealth(t, s, "ready", "Eligible; inference availability has not been verified.")
}

func TestStatusHealthCatalogDiscoveryDoesNotVerifyInference(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"messages required"}}`))
	}))
	defer api.Close()
	up := statusHealthUp()
	up.BaseURL = api.URL + "/v1"
	s := statusHealthServer(t, up)
	if _, err := s.refreshUpstreamModels(context.Background(), []string{"up"}); err != nil {
		t.Fatal(err)
	}
	rec := assertStatusHealth(t, s, "ready", "Eligible; inference availability has not been verified.")
	if _, ok := rec["last_success_at"]; ok {
		t.Fatal("catalog/protocol evidence fabricated successful inference")
	}
}

func BenchmarkStatusHealthSparseRestrictions(b *testing.B) {
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("keys_and_routes_%d", size), func(b *testing.B) {
			up := statusHealthUp()
			up.APIKeys, up.Models = nil, nil
			for i := 0; i < size; i++ {
				up.APIKeys = append(up.APIKeys, fmt.Sprintf("synthetic-key-%d", i))
				up.Models = append(up.Models, fmt.Sprintf("catalog-model-%d", i))
			}
			up.KeyBlockedModels = map[string][]string{config.KeyFingerprint(up.APIKeys[0]): {up.Models[0]}}
			s := statusHealthServer(b, up)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if rec := s.hitchanceHealth(); len(rec) != 1 || rec[0].Status != "degraded" {
					b.Fatal("sparse block must not exhaust catalog")
				}
			}
		})
	}
}

func TestStatusHealthReadinessContractAndAuthentication(t *testing.T) {
	s := statusHealthServer(t, statusHealthUp())
	w := httptest.NewRecorder()
	s.Handle(w, httptest.NewRequest(http.MethodGet, "/admin/api/upstreams/health", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("health bypassed authentication: %d", w.Code)
	}
	rec := assertStatusHealth(t, s, "ready", "Eligible; inference availability has not been verified.")
	if _, ok := rec["last_success_at"]; ok {
		t.Fatal("readiness fabricated successful inference")
	}
}
