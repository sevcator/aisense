package routehealth

import (
	"testing"

	"aisense/internal/config"
)

func TestHitRatesPerEndpointAndKey(t *testing.T) {
	m := New()
	up := &config.Upstream{ID: "up", BaseURL: "https://one.invalid/v1", Enabled: true, APIKeys: []string{"good", "bad"}}
	if rate, keys := m.HitRates(up); rate.Attempts != 0 || len(keys) != 0 {
		t.Fatal("no attempts yet")
	}
	for i := 0; i < 3; i++ {
		m.RecordAttempt(up, "good", true)
	}
	m.RecordAttempt(up, "bad", false)
	rate, keys := m.HitRates(up)
	if rate.Attempts != 4 || rate.Chance != 0.75 {
		t.Fatalf("endpoint = %+v", rate)
	}
	if good := keys[config.KeyFingerprint("good")]; good.Attempts != 3 || good.Chance != 1 {
		t.Fatalf("good key = %+v", good)
	}
	if bad := keys[config.KeyFingerprint("bad")]; bad.Attempts != 1 || bad.Chance != 0 {
		t.Fatalf("bad key = %+v", bad)
	}

	// Only the most recent attempts count: old failures age out.
	for i := 0; i < hitWindowSize; i++ {
		m.RecordAttempt(up, "bad", true)
	}
	_, keys = m.HitRates(up)
	if bad := keys[config.KeyFingerprint("bad")]; bad.Attempts != hitWindowSize || bad.Chance != 1 {
		t.Fatalf("window = %+v", bad)
	}

	// A keyless attempt counts for the endpoint only; an edited endpoint starts afresh.
	m.RecordAttempt(up, "", false)
	edited := *up
	edited.BaseURL = "https://two.invalid/v1"
	if rate, _ := m.HitRates(&edited); rate.Attempts != 0 {
		t.Fatalf("edited endpoint inherited %+v", rate)
	}
}
