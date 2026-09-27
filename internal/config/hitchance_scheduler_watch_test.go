package config

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestHitchanceSchedulerWatchMigration(t *testing.T) {
	m := hitchanceRegressionManager(t)
	entered := make(chan struct{}, 1)
	if err := m.SetLoggingValidator(func(bool) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	existing := hitchanceWatcherStacks()
	m.Watch(time.Millisecond)
	waitHitchanceWatcher(t, false, func(id string) bool { _, ok := existing[id]; return !ok })
	data := []byte(`{"logging_enabled":true,"failover":{"mode":"none","retry_cycles":5,"response_start_timeout_seconds":33,"upstream_cache_ttl_hours":44,"sticky_upstream_ttl_hours":55},"hitchance":{"retry_cycles":3}}`)
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(m.path, future, future); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not reload")
	}
	m.mu.Lock()
	c := m.cfg
	m.mu.Unlock()
	if c.Hitchance.RetryCycles != 0 || c.Hitchance.ResponseStartTimeoutSeconds != 33 || c.Hitchance.UpstreamCacheTTLHours != 44 || c.Hitchance.StickyUpstreamTTLHours != 55 {
		t.Fatalf("watch migration: %+v", c.Hitchance)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(b, &fields)
	if _, ok := fields["failover"]; ok {
		t.Fatal("watch output retained failover")
	}
}
