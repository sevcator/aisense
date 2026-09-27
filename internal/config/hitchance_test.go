package config

import (
	"aisense/internal/hitchance"
	"path/filepath"
	"testing"
	"time"
)

func TestHitchanceDeleteScopedAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{
			{ID: "a", BaseURL: "https://one.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"bad"}},
			{ID: "b", BaseURL: "https://two.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"bad"}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := m.Get().Upstreams[0]
	d := hitchance.Classify(m.Get().Hitchance, hitchance.Input{Status: 401, Body: []byte("Your key is invalid")})
	if err = m.ObserveHitchance(old, "bad", "model", d); err != nil {
		t.Fatal(err)
	}
	if len(m.Get().Upstreams) != 1 || m.Get().Upstreams[0].ID != "b" {
		t.Fatal("upstream with its last invalid key must be removed")
	}
	if len(old.APIKeys) != 1 || len(old.HitchanceState) != 0 {
		t.Fatal("published snapshot mutated")
	}
	if len(m.Get().Upstreams[0].APIKeys) != 1 {
		t.Fatal("removed same key from unrelated endpoint")
	}
	reload, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reload.Get().Upstreams) != 1 || reload.Get().Upstreams[0].ID != "b" {
		t.Fatal("deletion lost on restart")
	}
}
func TestHitchanceConfirmationSuccessAndExactModelScope(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Update(func(c *Config) error {
		c.Hitchance.InvalidConfirmations = 2
		c.Upstreams = []*Upstream{{ID: "a", BaseURL: "https://one.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"bad", "good"}}}
		return nil
	})
	d := hitchance.Decision{Action: "delete", Scope: "key", Category: "invalid_key", RuleID: "invalid", Cooldown: time.Second}
	up := m.Get().Upstreams[0]
	m.ObserveHitchance(up, "bad", "m", d)
	if len(m.Get().Upstreams[0].APIKeys) != 2 {
		t.Fatal("deleted before threshold")
	}
	m.ObserveHitchance(m.Get().Upstreams[0], "bad", "m", hitchance.Decision{})
	m.ObserveHitchance(up, "bad", "m", d)
	if len(m.Get().Upstreams[0].APIKeys) != 2 {
		t.Fatal("success did not reset confirmations")
	}
	m.ObserveHitchance(up, "bad", "m", d)
	if len(m.Get().Upstreams[0].APIKeys) != 1 {
		t.Fatal("threshold did not delete")
	}
	md := hitchance.Decision{Action: "demote", Scope: "model", Category: "model", RuleID: "model", Cooldown: time.Minute}
	m.ObserveHitchance(up, "good", "provider/model", md)
	states := m.Get().Upstreams[0].HitchanceState
	if !states["model:"+KeyFingerprint("good")+":provider/model"].Until.After(time.Now()) {
		t.Fatal("exact key-model state missing")
	}
	if states["key:"+KeyFingerprint("good")].Until.After(time.Now()) {
		t.Fatal("model failure disabled entire key")
	}
}
func TestHitchanceStaleSuccessAndEndpointEdit(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "a", BaseURL: "https://one.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"key"}}}
		return nil
	})
	old := m.Get().Upstreams[0]
	d := hitchance.Decision{Action: "demote", Scope: "key", Category: "quota", RuleID: "q", Cooldown: time.Hour}
	m.ObserveHitchance(old, "key", "m", d)
	m.ObserveHitchance(old, "key", "m", hitchance.Decision{})
	if len(m.Get().Upstreams[0].HitchanceState) == 0 {
		t.Fatal("stale in-flight success cleared a newer failure")
	}
	m.Update(func(c *Config) error { c.Upstreams[0].BaseURL = "https://two.invalid"; return nil })
	if !m.Get().Upstreams[0].HitchanceReady("key", "m", time.Now()) {
		t.Fatal("old endpoint health leaked into edited endpoint")
	}
	m.ObserveHitchance(old, "key", "m", d)
	if !m.Get().Upstreams[0].HitchanceReady("key", "m", time.Now()) {
		t.Fatal("stale observation applied after edit")
	}
}
func TestHitchanceRejectsInvalidConfiguration(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Update(func(c *Config) error {
		c.Hitchance.Rules = []hitchance.Rule{{ID: "danger", Action: "delete", Scope: "key", Category: "bad", StatusCodes: []int{403}, CooldownSeconds: 10}}
		return nil
	})
	if err == nil {
		t.Fatal("status-only deletion accepted")
	}
}
