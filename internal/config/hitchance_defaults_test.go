package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"aisense/internal/hitchance"
)

func TestUntouchedLegacyDefaultRulesMoveToCurrentDefaults(t *testing.T) {
	write := func(rules []hitchance.Rule) string {
		cfg := Default()
		cfg.Hitchance.Rules = rules
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write(hitchance.LegacyDefaultRules())
	for i := 0; i < 2; i++ { // migrated in memory, then persisted
		m, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m.Get().Hitchance.Rules, hitchance.Default().Rules) {
			t.Fatalf("load %d kept the legacy rules: %d rules", i, len(m.Get().Hitchance.Rules))
		}
	}
	customised := hitchance.LegacyDefaultRules()[:12]
	m, err := Load(write(customised))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Get().Hitchance.Rules, customised) {
		t.Fatal("customised rules must never be replaced")
	}
}

func TestRepeatedFailuresDoubleTheCooldown(t *testing.T) {
	for _, tc := range []struct {
		failures, max int
		want          time.Duration
	}{
		{1, 86400, time.Minute}, {2, 86400, 2 * time.Minute}, {3, 86400, 4 * time.Minute}, {30, 3600, time.Hour},
	} {
		if got := EscalatedCooldown(time.Minute, tc.failures, tc.max); got != tc.want {
			t.Fatalf("failures=%d max=%d: %v want %v", tc.failures, tc.max, got, tc.want)
		}
	}

	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "a", BaseURL: "https://one.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"k", "other"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A spent quota does not clear by itself, so repeats rest the key longer.
	limit := hitchance.Classify(m.Get().Hitchance, hitchance.Input{Status: 402})
	target := HitchanceTarget("key", "k", "")
	cooldown := func() time.Duration {
		s := m.Get().Upstreams[0].HitchanceState[target]
		return s.Until.Sub(s.UpdatedAt).Round(time.Second)
	}
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if err := m.ObserveHitchance(m.Get().Upstreams[0], "k", "model", limit); err != nil {
			t.Fatal(err)
		}
		if got := cooldown(); got != want {
			t.Fatalf("failure %d cooldown %v want %v", i+1, got, want)
		}
	}
	// A success clears the streak, so the next failure starts from the base again.
	if err := m.ObserveHitchance(m.Get().Upstreams[0], "k", "model", hitchance.Decision{}); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveHitchance(m.Get().Upstreams[0], "k", "model", limit); err != nil {
		t.Fatal(err)
	}
	if got := cooldown(); got != time.Minute {
		t.Fatalf("after success cooldown %v want 1m", got)
	}
}

// A rate limit rests the key briefly and never past a minute. Parallel requests
// that hit it together count once; only a hit after the rest ended is a repeat.
func TestRateLimitRestIsShortAndCapped(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "a", BaseURL: "https://one.invalid", AuthMode: "swap", Enabled: true, APIKeys: []string{"k", "other"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	limit := hitchance.Classify(m.Get().Hitchance, hitchance.Input{Status: 429, Body: []byte(`{"error":{"message":"TPM limit reached"}}`)})
	if limit.Category != "rate_limit" {
		t.Fatalf("429 TPM classified as %+v", limit)
	}
	target := HitchanceTarget("key", "k", "")
	state := func() hitchance.State { return m.Get().Upstreams[0].HitchanceState[target] }
	observe := func() {
		t.Helper()
		if err := m.ObserveHitchance(m.Get().Upstreams[0], "k", "model", limit); err != nil {
			t.Fatal(err)
		}
	}
	// Ends the current rest, as if its time had passed.
	expire := func() {
		t.Helper()
		if err := m.Update(func(c *Config) error {
			s := c.Upstreams[0].HitchanceState[target]
			s.Until = time.Now().Add(-time.Second)
			c.Upstreams[0].HitchanceState[target] = s
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rest := func() time.Duration { s := state(); return s.Until.Sub(s.UpdatedAt).Round(time.Second) }

	observe()
	observe() // a parallel request hitting the same limit
	observe()
	if got := rest(); got != 10*time.Second || state().Failures != 1 {
		t.Fatalf("parallel hits: rest %v failures %d, want 10s and 1", got, state().Failures)
	}
	for _, want := range []time.Duration{20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
		expire()
		observe()
		if got := rest(); got != want {
			t.Fatalf("repeat rest %v want %v", got, want)
		}
	}

	// A Retry-After is honoured as sent, even past a minute, and not doubled.
	withHeader := hitchance.Classify(m.Get().Hitchance, hitchance.Input{Status: 429, Header: map[string][]string{"Retry-After": {"90"}}})
	if withHeader.Category != "rate_limit" || withHeader.Cooldown != 90*time.Second {
		t.Fatalf("Retry-After decision %+v", withHeader)
	}
	limit = withHeader
	expire()
	observe()
	if got := rest(); got != 90*time.Second {
		t.Fatalf("Retry-After rest %v want 90s", got)
	}
}
