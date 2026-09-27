package hitchance

import (
	"fmt"
	"testing"
)

func TestSchedulerDecodeDefaultsAndValidation(t *testing.T) {
	base := Default()
	old := base
	old.RetryCycles = 0
	old.ResponseStartTimeoutSeconds = 0
	old.UpstreamCacheTTLHours = 0
	old.StickyUpstreamTTLHours = 0
	if c, err := Decode(old, []byte(`{"enabled":false}`)); err != nil || c.RetryCycles != 0 || c.ResponseStartTimeoutSeconds != 20 || c.UpstreamCacheTTLHours != 24 || c.StickyUpstreamTTLHours != 24 {
		t.Fatalf("old partial not normalized: %+v err=%v", c, err)
	}
	for _, tc := range []struct {
		name string
		max  int
	}{
		{"retry_cycles", 10}, {"response_start_timeout_seconds", 120}, {"upstream_cache_ttl_hours", 720}, {"sticky_upstream_ttl_hours", 720},
	} {
		invalid := []string{"-1", fmt.Sprint(tc.max + 1), "null"}
		if tc.name != "retry_cycles" {
			invalid = append(invalid, "0")
		}
		for _, value := range invalid {
			if _, err := Decode(base, []byte(fmt.Sprintf(`{"%s":%s}`, tc.name, value))); err == nil {
				t.Errorf("accepted %s=%s", tc.name, value)
			}
		}
		valid := []int{1, tc.max}
		if tc.name == "retry_cycles" {
			valid = append(valid, 0)
		}
		for _, value := range valid {
			if _, err := Decode(base, []byte(fmt.Sprintf(`{"%s":%d}`, tc.name, value))); err != nil {
				t.Errorf("rejected valid %s=%d: %v", tc.name, value, err)
			}
		}
	}
	if _, err := Decode(base, []byte(`{"Retry_cycles":3}`)); err == nil {
		t.Fatal("accepted noncanonical scheduler field")
	}
}
