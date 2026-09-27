package routehealth

import (
	"path/filepath"
	"testing"

	"aisense/internal/config"
)

// The hit chance is counted in memory, so it is saved and restored: a restart
// shows the recent numbers instead of "no requests yet".
func TestHitWindowsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: "https://api.example.com/v1", Models: []string{"model"}, APIKeys: []string{"first", "second"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	live := manager.Get().Upstreams[0]
	health := New()
	for _, ok := range []bool{true, true, false, true} {
		health.RecordAttempt(live, "first", ok)
	}
	health.RecordAttempt(live, "second", false)
	if err := manager.SaveHitWindows(health.HitSnapshot()); err != nil {
		t.Fatal(err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New()
	endpoint, keys := restarted.HitRates(reloaded.Get().Upstreams[0])
	if endpoint.Attempts != 5 || endpoint.Chance != 0.6 {
		t.Fatalf("API base after restart = %+v", endpoint)
	}
	if first := keys[config.KeyFingerprint("first")]; first.Attempts != 4 || first.Chance != 0.75 {
		t.Fatalf("first key after restart = %+v", first)
	}
	if second := keys[config.KeyFingerprint("second")]; second.Attempts != 1 || second.Chance != 0 {
		t.Fatalf("second key after restart = %+v", second)
	}
	// Live attempts continue the restored window instead of replacing it.
	restarted.RecordAttempt(reloaded.Get().Upstreams[0], "first", true)
	if endpoint, _ := restarted.HitRates(reloaded.Get().Upstreams[0]); endpoint.Attempts != 6 {
		t.Fatalf("after one more attempt = %+v", endpoint)
	}
}

// Editing the endpoint or its authentication starts the history afresh.
func TestSavedHitWindowsAreDroppedWhenTheEndpointChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: "https://api.example.com/v1", Models: []string{"model"}, APIKeys: []string{"first"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	health := New()
	health.RecordAttempt(manager.Get().Upstreams[0], "first", true)
	if err := manager.SaveHitWindows(health.HitSnapshot()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.Upstreams[0].BaseURL = "https://other.example.com/v1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := New().HitRates(manager.Get().Upstreams[0])
	if endpoint.Attempts != 0 {
		t.Fatalf("history of the old endpoint = %+v", endpoint)
	}
	// A snapshot from the old endpoint is not written onto the new one either.
	if err := manager.SaveHitWindows(health.HitSnapshot()); err != nil {
		t.Fatal(err)
	}
	if hits := manager.Get().Upstreams[0].Health; hits != nil && hits.Hits != nil && hits.Hits.Count != 0 {
		t.Fatalf("stale hits were saved: %+v", hits.Hits)
	}
}
