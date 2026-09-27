package routehealth

import (
	"testing"
	"time"

	"aisense/internal/config"
)

func TestSnapshotAuthContextDoesNotInheritRuntimeEvidence(t *testing.T) {
	for _, edit := range []struct {
		name   string
		change func(*config.Upstream)
	}{
		{"auth", func(up *config.Upstream) { up.AuthMode = "none" }},
		{"oauth", func(up *config.Upstream) { up.OAuth = &config.OAuthCfg{ClientID: "different-context"} }},
		{"proxy", func(up *config.Upstream) { up.UseProxy = true }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			m := New()
			up := testUp("edited", 1)
			up.AuthMode = "swap"
			m.MarkSuccess(up, "model")
			m.MarkFailure(up, "model", Global, time.Hour, "old failure")
			changed := *up
			edit.change(&changed)
			rec := m.Snapshot([]*config.Upstream{&changed})[0]
			if rec.Status != "ready" || rec.LastSuccessAt != nil || rec.LastFailureAt != nil || rec.LastError != "" {
				t.Fatalf("edited auth inherited runtime evidence: %#v", rec)
			}
		})
	}
}

func TestSnapshotExpiredFailuresDoNotPresentActiveError(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	up := testUp("expired", 1)
	m.MarkFailure(up, "model", Model, time.Minute, "old model failure")
	now = now.Add(time.Minute)
	rec := m.Snapshot([]*config.Upstream{up})[0]
	if rec.Status != "ready" || rec.LastError != "" || rec.CooldownUntil != nil || len(rec.UnavailableModels) != 0 {
		t.Fatalf("expired failure presented as active: %#v", rec)
	}
	if rec.LastFailureAt == nil {
		t.Fatal("historical failure timestamp lost")
	}
}

func TestSnapshotSameClockFailureStillNeedsRevalidation(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	up := testUp("clock", 1)
	m.MarkSuccess(up, "model")
	m.MarkFailure(up, "model", Global, time.Minute, "network failure")
	now = now.Add(time.Minute)
	if rec := m.Snapshot([]*config.Upstream{up})[0]; rec.Status != "ready" {
		t.Fatalf("clock equality promoted expired failure to success: %#v", rec)
	}
	m.MarkSuccess(up, "model")
	if rec := m.Snapshot([]*config.Upstream{up})[0]; rec.Status != "available" {
		t.Fatalf("new successful inference did not revalidate: %#v", rec)
	}
}

func TestSnapshotKeepsFailuresForCurrentRawAliases(t *testing.T) {
	m := New()
	up := testUp("alias", 1)
	up.Models = []string{"friendly-model"}
	up.ModelAliases = map[string][]string{"friendly-model": {"provider/deployment-1", "provider/deployment-2"}}
	m.MarkFailure(up, "provider/deployment-1", Model, time.Hour, "model failure")
	rec := m.Snapshot([]*config.Upstream{up})[0]
	if rec.Status != "degraded" || len(rec.UnavailableModels) != 1 {
		t.Fatalf("current raw alias failure was discarded: %#v", rec)
	}
	up.ModelAliases = nil
	rec = m.Snapshot([]*config.Upstream{up})[0]
	if rec.Status != "ready" || len(rec.UnavailableModels) != 0 || rec.LastError != "" {
		t.Fatalf("removed raw alias still presented as failed: %#v", rec)
	}
}

func TestSnapshotReadinessDoesNotInventInferenceEvidence(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	up := testUp("ready", 1)
	up.ModelsRefreshedAt = now // Catalog evidence is not inference evidence.
	assert := func(want string, success bool) {
		t.Helper()
		rec := m.Snapshot([]*config.Upstream{up})[0]
		if rec.Status != want || (rec.LastSuccessAt != nil) != success {
			t.Fatalf("want %s, inference evidence=%v; got %#v", want, success, rec)
		}
	}
	assert("ready", false)
	done := m.Begin(up)
	assert("ready", false)
	done()
	m.MarkSuccess(up, "model")
	assert("available", true)
	now = now.Add(time.Second)
	m.MarkFailure(up, "model", Global, time.Minute, "network failure")
	assert("unavailable", true)
	now = now.Add(time.Minute)
	assert("ready", true) // Recovery is eligible, not revalidated.
	m.MarkSuccess(up, "model")
	assert("available", true)
	up.Enabled = false
	assert("disabled", false)
	up.Enabled = true
	if rec := New().Snapshot([]*config.Upstream{up})[0]; rec.Status != "ready" || rec.LastSuccessAt != nil {
		t.Fatalf("restart fabricated inference: %#v", rec)
	}
}
