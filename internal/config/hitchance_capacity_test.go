package config

import (
	"fmt"
	"testing"
	"time"

	"aisense/internal/hitchance"
)

func TestHitchanceCapacityPreservesEnforcement(t *testing.T) {
	for _, history := range []string{"expired", "recent-expired-models", "all-active", "all-quarantined", "active-with-endpoint"} {
		for _, decision := range []struct{ name, action, scope string }{{"quota", "demote", "key"}, {"delete", "delete", "key"}, {"quarantine", "quarantine", "key"}, {"endpoint", "demote", "endpoint"}, {"model", "demote", "model"}} {
			t.Run(history+"/"+decision.name, func(t *testing.T) {
				m := hitchanceRegressionManager(t)
				now := time.Now()
				if err := m.Update(func(c *Config) error {
					up := c.Upstreams[0]
					up.HitchanceState = map[string]hitchance.State{}
					for i := 0; i < 512; i++ {
						state := hitchance.State{Identity: up.HitchanceIdentity(), Action: "demote", Category: "model", RuleID: "old", UpdatedAt: now, Until: now.Add(time.Hour), Failures: 1}
						if history == "expired" {
							state.UpdatedAt = now.Add(-48 * time.Hour)
							state.Until = now.Add(-24 * time.Hour)
						}
						if history == "recent-expired-models" {
							state.Until = now.Add(-time.Minute)
						}
						if history == "all-quarantined" {
							state.Action = "quarantine"
						}
						target := HitchanceTarget("model", "b", fmt.Sprintf("old-%d", i))
						if history == "active-with-endpoint" && i == 0 {
							target = "endpoint"
						}
						up.HitchanceState[target] = state
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				old := m.Get().Upstreams[0]
				d := hitchance.Decision{Action: decision.action, Scope: decision.scope, RuleID: "new", Category: decision.name, Cooldown: time.Minute}
				if err := m.ObserveHitchance(old, "a", "new-model", d); err != nil {
					t.Fatal(err)
				}
				up := m.Get().Upstreams[0]
				if len(up.HitchanceState) > 512 {
					t.Fatalf("unbounded state: %d", len(up.HitchanceState))
				}
				if decision.action == "delete" {
					if len(up.APIKeys) != 1 || up.APIKeys[0] != "b" {
						t.Fatalf("capacity silently discarded deletion: %v", up.APIKeys)
					}
				} else if up.HitchanceReady("a", "new-model", time.Now()) {
					t.Fatal("capacity silently discarded new restriction")
				}
				if decision.action == "quarantine" && up.HitchanceReady("a", "new-model", now.Add(365*24*time.Hour)) {
					t.Fatal("quarantine lost at capacity")
				}
				expired := history == "expired" || history == "recent-expired-models"
				if expired && decision.scope != "endpoint" && !up.HitchanceReady("b", "other", time.Now()) {
					t.Fatal("reclaimable histories needlessly widened scope")
				}
				if !expired && up.HitchanceReady("b", "old-511", time.Now()) {
					t.Fatal("overflow evicted an active restriction")
				}
				if err := m.ObserveHitchance(old, "b", "other", hitchance.Decision{}); err != nil {
					t.Fatal(err)
				}
				if decision.action != "delete" && m.Get().Upstreams[0].HitchanceReady("a", "new-model", time.Now()) {
					t.Fatal("stale success cleared overflow failure")
				}
				reload, err := Load(m.path)
				if err != nil {
					t.Fatal(err)
				}
				if len(reload.Get().Upstreams[0].HitchanceState) > 512 {
					t.Fatal("disk state exceeds capacity")
				}
				if decision.action != "delete" && reload.Get().Upstreams[0].HitchanceReady("a", "new-model", time.Now()) {
					t.Fatal("enforcement lost on reload")
				}
			})
		}
	}
}
