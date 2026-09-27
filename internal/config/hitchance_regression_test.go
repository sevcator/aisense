package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/hitchance"
)

func hitchanceRegressionManager(t *testing.T) *Manager {
	t.Helper()
	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "up", BaseURL: "https://synthetic.invalid", Enabled: true, AuthMode: "swap", APIKeys: []string{"a", "b"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHitchanceRestrictionsCannotBeWeakenedByLaterFailures(t *testing.T) {
	for _, scope := range []string{"key", "model", "endpoint"} {
		for _, firstAction := range []string{"demote", "quarantine"} {
			t.Run(scope+"/"+firstAction, func(t *testing.T) {
				m := hitchanceRegressionManager(t)
				old := m.Get().Upstreams[0]
				first := hitchance.Decision{Action: firstAction, Scope: scope, RuleID: "long-retry-after", Category: "quota", Cooldown: 24 * time.Hour}
				if err := m.ObserveHitchance(old, "a", "m", first); err != nil {
					t.Fatal(err)
				}
				target := HitchanceTarget(scope, "a", "m")
				before := m.Get().Upstreams[0].HitchanceState[target]
				shorter := hitchance.Decision{Action: "demote", Scope: scope, RuleID: "late-auth", Category: "permission", Cooldown: time.Second}
				if err := m.ObserveHitchance(old, "a", "m", shorter); err != nil {
					t.Fatal(err)
				}
				after := m.Get().Upstreams[0].HitchanceState[target]
				if after.Action != firstAction || after.Until.Before(before.Until) {
					t.Fatalf("restriction weakened: before=%+v after=%+v", before, after)
				}
				if err := m.ObserveHitchance(old, "a", "m", hitchance.Decision{}); err != nil {
					t.Fatal(err)
				}
				if m.Get().Upstreams[0].HitchanceReady("a", "m", time.Now()) {
					t.Fatal("stale success cleared newer failure")
				}
				if firstAction == "quarantine" {
					if err := m.ObserveHitchance(m.Get().Upstreams[0], "a", "m", hitchance.Decision{}); err != nil {
						t.Fatal(err)
					}
					if m.Get().Upstreams[0].HitchanceReady("a", "m", time.Now().Add(30*24*time.Hour)) {
						t.Fatal("quarantine did not survive success and expiration")
					}
				}
			})
		}
	}
}

func hitchanceWatcherStacks() map[string]string {
	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	out := map[string]string{}
	for _, stack := range strings.Split(stacks, "\n\n") {
		if strings.Contains(stack, "config.(*Manager).Watch.func1()") {
			out[strings.Fields(stack)[1]] = stack
		}
	}
	return out
}

func waitHitchanceWatcher(t *testing.T, blocked bool, selectID func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for id, stack := range hitchanceWatcherStacks() {
			if !selectID(id) {
				continue
			}
			if blocked && strings.Contains(stack, ".Lock(") || !blocked && strings.Contains(stack, "[chan receive]") {
				return id
			}
		}
		runtime.Gosched()
	}
	t.Fatal("watcher did not reach synchronization barrier")
	return ""
}

func TestHitchanceWatchCannotRepublishPreObservationSnapshot(t *testing.T) {
	m := hitchanceRegressionManager(t)
	if err := m.Update(func(c *Config) error { c.LoggingEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	entered, release, reloaded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var validations atomic.Int32
	if err := m.SetLoggingValidator(func(bool) error {
		switch validations.Add(1) {
		case 2:
			close(entered)
			<-release
		case 3:
			close(reloaded)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	existing := hitchanceWatcherStacks()
	m.Watch(time.Millisecond)
	watcher := waitHitchanceWatcher(t, false, func(id string) bool { _, found := existing[id]; return !found })
	old := m.Get().Upstreams[0]
	done := make(chan error, 1)
	go func() {
		done <- m.ObserveHitchance(old, "a", "m", hitchance.Decision{Action: "delete", Scope: "key", RuleID: "invalid", Category: "invalid_key", Cooldown: time.Minute})
	}()
	<-entered // Observation holds the update lock, before persistence.
	// Trigger a reload of the pre-observation file while that lock is held.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(m.path, future, future); err != nil {
		t.Fatal(err)
	}
	waitHitchanceWatcher(t, true, func(id string) bool { return id == watcher })
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not reload")
	}
	// The update lock also serves as a publication barrier, then tests durability.
	if err := m.Update(func(c *Config) error { c.Hitchance.HonorRetryAfter = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if keys := m.Get().Upstreams[0].APIKeys; len(keys) != 1 || keys[0] != "b" {
		t.Fatalf("watcher resurrected rejected key: %v", keys)
	}
	reload, err := Load(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if keys := reload.Get().Upstreams[0].APIKeys; len(keys) != 1 || keys[0] != "b" {
		t.Fatalf("stale reload became durable: %v", keys)
	}
	if len(reload.Get().Upstreams[0].HitchanceState) == 0 {
		t.Fatal("failure state lost")
	}
}
