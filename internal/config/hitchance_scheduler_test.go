package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHitchanceSchedulerMigration(t *testing.T) {
	for _, tc := range []struct {
		name, disk string
		want       []int
	}{
		{"new", "", []int{0, 20, 24, 24}},
		{"old-missing", `{}`, []int{0, 20, 24, 24}},
		{"old-policy", `{"failover":{"mode":"none","retry_cycles":4,"response_start_timeout_seconds":35,"upstream_cache_ttl_hours":48,"sticky_upstream_ttl_hours":12,"blacklist":{"messages":["*"]}},"upstreams":[{"id":"a","fail_codes":[418]}]}`, []int{0, 35, 48, 12}},
		{"explicit-wins", `{"failover":{"retry_cycles":4,"response_start_timeout_seconds":35,"upstream_cache_ttl_hours":48,"sticky_upstream_ttl_hours":12},"hitchance":{"retry_cycles":3,"response_start_timeout_seconds":17,"upstream_cache_ttl_hours":6,"sticky_upstream_ttl_hours":7}}`, []int{0, 17, 6, 7}},
		{"partial", `{"failover":{"retry_cycles":4,"response_start_timeout_seconds":35},"hitchance":{"enabled":false,"retry_cycles":3}}`, []int{0, 35, 24, 24}},
		{"existing-hitchance", `{"hitchance":{"enabled":false,"rules":[]}}`, []int{0, 20, 24, 24}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.disk != "" {
				if err := os.WriteFile(path, []byte(tc.disk), 0600); err != nil {
					t.Fatal(err)
				}
			}
			m, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				b, err := json.Marshal(m.Get())
				if err != nil {
					t.Fatal(err)
				}
				var obj map[string]json.RawMessage
				json.Unmarshal(b, &obj)
				if _, ok := obj["failover"]; ok {
					t.Error("canonical config retains failover")
				}
				var fields map[string]json.RawMessage
				json.Unmarshal(obj["hitchance"], &fields)
				for n, name := range []string{"retry_cycles", "response_start_timeout_seconds", "upstream_cache_ttl_hours", "sticky_upstream_ttl_hours"} {
					var v int
					json.Unmarshal(fields[name], &v)
					if v != tc.want[n] {
						t.Errorf("%s=%d want=%d", name, v, tc.want[n])
					}
				}
				var ups []map[string]json.RawMessage
				json.Unmarshal(obj["upstreams"], &ups)
				for _, up := range ups {
					if _, ok := up["fail_codes"]; ok {
						t.Error("canonical upstream retains fail_codes")
					}
				}
				m, err = Load(path)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
