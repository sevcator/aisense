package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aisense/internal/hitchance"
)

func healthPersistenceManager(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "up", BaseURL: "https://synthetic.invalid/v1", AuthMode: "swap", APIKeys: []string{"synthetic-secret"}, Models: []string{"model"}, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return m, path
}
func observeHealth(t *testing.T, m *Manager, snapshot *Upstream, success bool) {
	t.Helper()
	observer, ok := any(m).(interface{ ObserveUpstreamHealth(*Upstream, bool) error })
	if !ok {
		t.Fatal("manager does not persist upstream observations")
	}
	if err := observer.ObserveUpstreamHealth(snapshot, success); err != nil {
		t.Fatal(err)
	}
}
func storedHealth(t *testing.T, m *Manager) map[string]json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(m.Get().Upstreams[0])
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	var h map[string]json.RawMessage
	if raw := obj["health"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
	}
	return h
}
func TestUpstreamHealthPersistsSuccessWithoutLiveCredentials(t *testing.T) {
	m, path := healthPersistenceManager(t)
	if h := storedHealth(t, m); h != nil {
		t.Fatal("untested upstream has invented history")
	}
	snapshot := m.Get().Upstreams[0]
	observeHealth(t, m, snapshot, true)
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := storedHealth(t, reloaded)
	if string(h["status"]) != `"available"` || len(h["last_success_at"]) == 0 || len(h["identity"]) == 0 {
		t.Fatalf("successful status not persisted: %s", h)
	}
	if _, ok := h["last_failure_at"]; ok {
		t.Fatal("invented failure")
	}
	old, _ := json.Marshal(snapshot)
	var oldObj map[string]json.RawMessage
	json.Unmarshal(old, &oldObj)
	if len(oldObj["health"]) > 0 {
		t.Fatal("published snapshot mutated")
	}
	raw, _ := json.Marshal(h)
	if string(raw) == "" {
		t.Fatal("empty health")
	}
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	for field := range fields {
		if field != "status" && field != "identity" && field != "last_success_at" && field != "last_failure_at" {
			t.Fatalf("unexpected persisted field %s", field)
		}
	}
	before, _ := os.ReadFile(path)
	observeHealth(t, m, m.Get().Upstreams[0], true)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("repeated immediate success rewrote config unnecessarily")
	}
}
func TestUpstreamHealthLateSuccessCannotEraseFailure(t *testing.T) {
	m, path := healthPersistenceManager(t)
	old := m.Get().Upstreams[0]
	observeHealth(t, m, old, false)
	before, _ := os.ReadFile(path)
	observeHealth(t, m, old, true)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("older attempt success superseded newer failure")
	}
	if h := storedHealth(t, m); string(h["status"]) != `"unavailable"` || len(h["last_failure_at"]) == 0 {
		t.Fatalf("failure history lost: %s", h)
	}
	observeHealth(t, m, m.Get().Upstreams[0], true)
	if h := storedHealth(t, m); string(h["status"]) != `"available"` {
		t.Fatalf("fresh revalidation ignored: %s", h)
	}
}
func TestUpstreamHealthCannotRacePersistedRestriction(t *testing.T) {
	m, _ := healthPersistenceManager(t)
	old := m.Get().Upstreams[0]
	if err := m.ObserveHitchance(old, "synthetic-secret", "model", hitchance.Decision{Action: "demote", Scope: "endpoint", RuleID: "synthetic", Category: "transient", Cooldown: time.Hour}); err != nil {
		t.Fatal(err)
	}
	observeHealth(t, m, old, true)
	if h := storedHealth(t, m); string(h["status"]) == `"available"` {
		t.Fatal("old success outran persisted failure")
	}
}
func TestUpstreamHealthIgnoresEditedAndDisabledSnapshots(t *testing.T) {
	for _, edit := range []string{"endpoint", "auth", "disabled", "removed"} {
		t.Run(edit, func(t *testing.T) {
			m, path := healthPersistenceManager(t)
			old := m.Get().Upstreams[0]
			observeHealth(t, m, old, true)
			old = m.Get().Upstreams[0]
			if err := m.Update(func(c *Config) error {
				switch edit {
				case "endpoint":
					c.Upstreams[0].BaseURL = "https://replacement.invalid/v1"
				case "auth":
					c.Upstreams[0].AuthMode = "none"
				case "disabled":
					c.Upstreams[0].Enabled = false
				case "removed":
					c.Upstreams = nil
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			observeHealth(t, m, old, true)
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("stale observation changed config")
			}
			if edit == "endpoint" || edit == "auth" {
				if h := storedHealth(t, m); h != nil {
					t.Fatal("edited context inherited old status")
				}
			}
		})
	}
}
func TestUpstreamHealthObservesSuccessWhenPolicyDisabled(t *testing.T) {
	m, _ := healthPersistenceManager(t)
	if err := m.Update(func(c *Config) error { c.Hitchance.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	observeHealth(t, m, m.Get().Upstreams[0], true)
	if h := storedHealth(t, m); string(h["status"]) != `"available"` {
		t.Fatalf("disabled policy lost actual observation: %s", h)
	}
}
