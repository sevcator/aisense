package config

import (
	"aisense/internal/hitchance"
	"encoding/json"
)

// Disk-only migration: retired filters and modes have no routing authority.
// Explicit canonical Hitchance fields always win, including invalid values that
// must reach validation instead of silently being replaced by legacy settings.
func migrateHitchanceScheduler(data []byte, policy *hitchance.Config) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var old map[string]json.RawMessage
	// Only the numeric object is relevant; removed policy fields are ignored.
	if raw := fields["failover"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		name          string
		target        *int
		fallback, max int
	}{
		{"retry_cycles", &policy.RetryCycles, 1, 10},
		{"response_start_timeout_seconds", &policy.ResponseStartTimeoutSeconds, 20, 120},
		{"upstream_cache_ttl_hours", &policy.UpstreamCacheTTLHours, 24, 720},
		{"sticky_upstream_ttl_hours", &policy.StickyUpstreamTTLHours, 24, 720},
	} {
		if jsonFieldPresent(data, "hitchance", f.name) {
			continue
		}
		value := f.fallback
		if raw, ok := old[f.name]; ok {
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
		}
		if value < 1 {
			value = f.fallback
		}
		if value > f.max {
			value = f.max
		}
		*f.target = value
	}
	// A config written before the cooling retry existed gets the default; a
	// config that spells the field out keeps its own value, zero included.
	if !jsonFieldPresent(data, "hitchance", "cooling_retry_seconds") {
		policy.CoolingRetrySeconds = hitchance.Default().CoolingRetrySeconds
	}
	return nil
}
