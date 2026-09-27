package admin

import (
	"path/filepath"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

func TestUpstreamStatusSurvivesConfigReloadAndNewAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Server.Admin.Username = "status-admin"
		c.Server.Admin.Password = "synthetic-test-password"
		c.Upstreams = []*config.Upstream{statusHealthUp()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ObserveUpstreamHealth(cfg.Get().Upstreams[0], true); err != nil {
		t.Fatal(err)
	}
	reload := func() *Server {
		t.Helper()
		m, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return &Server{Cfg: m}
	}
	fresh := reload()
	rec := assertStatusHealth(t, fresh, "available", "Successful inference observed.")
	if len(rec["last_success_at"]) == 0 {
		t.Fatal("saved success time missing after restart")
	}
	up := cfg.Get().Upstreams[0]
	if err := cfg.ObserveUpstreamHealth(up, false); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ObserveHitchance(up, "synthetic-key", "model", hitchance.Decision{RuleID: "quota", Category: "quota", Action: "demote", Scope: "key", Cooldown: time.Hour}); err != nil {
		t.Fatal(err)
	}
	failed := reload()
	rec = assertStatusHealth(t, failed, "unavailable", "All effective credentials are restricted.")
	if len(rec["last_failure_at"]) == 0 {
		t.Fatal("saved failure time missing after restart")
	}
	if err := failed.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].HitchanceState = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	recovered := reload()
	assertStatusHealth(t, recovered, "ready", "Eligible; inference availability has not been verified.")
	if err := recovered.Cfg.ObserveUpstreamHealth(recovered.Cfg.Get().Upstreams[0], true); err != nil {
		t.Fatal(err)
	}
	assertStatusHealth(t, reload(), "available", "Successful inference observed.")
	if err := recovered.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].BaseURL = "https://replacement.invalid/v1"; return nil }); err != nil {
		t.Fatal(err)
	}
	edited := reload()
	rec = assertStatusHealth(t, edited, "ready", "Eligible; inference availability has not been verified.")
	if len(rec["last_success_at"]) > 0 {
		t.Fatal("replacement endpoint inherited saved success")
	}
	if err := edited.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	assertStatusHealth(t, reload(), "disabled", "Disabled by configuration.")
}
