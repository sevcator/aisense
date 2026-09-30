package main

import (
	"testing"

	"aisense/internal/config"
)

func TestTierPricingWaitsForEnabledUpstream(t *testing.T) {
	cfg := config.Default()
	if tierPricingEnabled(cfg) {
		t.Fatal("an empty installation should not fetch public prices")
	}
	cfg.Upstreams = []*config.Upstream{{ID: "off", Enabled: false}}
	if tierPricingEnabled(cfg) {
		t.Fatal("a disabled upstream should not start price refreshes")
	}
	cfg.Upstreams[0].Enabled = true
	if !tierPricingEnabled(cfg) {
		t.Fatal("an enabled upstream should start price refreshes")
	}
	cfg.TierPricing.Enabled = false
	if tierPricingEnabled(cfg) {
		t.Fatal("the pricing setting must still control refreshes")
	}
}
