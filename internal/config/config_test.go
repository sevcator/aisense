package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelDiscoveryDefaultsToHTTPSSchemeDetection(t *testing.T) {
	for _, raw := range []string{`{}`, `{"model_discovery":{}}`, `{"model_discovery":{"auto_fix_problems":false}}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Get().ModelDiscovery.AutoFixProblems {
			t.Fatal("HTTP/HTTPS detection should be enabled by default")
		}
	}
}

func TestLoadMigratesLegacyAPIKeyAndConsolidatesExactDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
  "upstreams": [
    {"id":"first","type":"openai","base_url":"https://API.example/v1/","api_key":" key-one ","models":["a"],"enabled":true},
    {"id":"second","type":"openai","base_url":"https://api.example/v1","api_keys":["key-two","key-one"],"models":["b"],"enabled":true}
  ]
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ups := m.Get().Upstreams
	if len(ups) != 1 || ups[0].ID != "first" {
		t.Fatalf("upstreams = %#v", ups)
	}
	if got := strings.Join(ups[0].APIKeys, ","); got != "key-one,key-two" {
		t.Fatalf("api keys = %q", got)
	}
	persisted, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), `"api_key"`) || !strings.Contains(string(persisted), `"api_keys"`) {
		t.Fatalf("legacy field was not migrated: %s", persisted)
	}
}

func TestUpstreamJSONAcceptsLegacyButOnlyEmitsCanonicalKeys(t *testing.T) {
	var up Upstream
	if err := json.Unmarshal([]byte(`{"id":"up","api_key":"legacy","api_keys":["new","legacy"]}`), &up); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(up.APIKeys, ","); got != "new,legacy" {
		t.Fatalf("api keys = %q", got)
	}
	out, err := json.Marshal(&up)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"api_key"`) || !strings.Contains(string(out), `"api_keys"`) {
		t.Fatalf("unexpected JSON: %s", out)
	}
}

func TestEndpointIdentityRetainsTypePortAndPath(t *testing.T) {
	a := &Upstream{Type: "openai", BaseURL: "http://Example.com:8443/v1/"}
	b := &Upstream{Type: "openai", BaseURL: "https://example.com:8443/v1"}
	c := &Upstream{Type: "openai", BaseURL: "https://example.com:8443/other"}
	if ProtocolEndpointIdentity(a) != ProtocolEndpointIdentity(b) {
		t.Fatalf("protocol twins were not matched: %q != %q", ProtocolEndpointIdentity(a), ProtocolEndpointIdentity(b))
	}
	if ExactEndpointIdentity(a) == ExactEndpointIdentity(b) {
		t.Fatal("exact identity ignored protocol")
	}
	if ProtocolEndpointIdentity(a) == ProtocolEndpointIdentity(c) {
		t.Fatal("protocol identity ignored path")
	}
}

func TestLoadMigratesRawModelsToCanonicalAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"upstreams":[{"id":"up","type":"openai","base_url":"https://example.com/v1","models":["aug/glm-5.2","glm-5.2","oad/free/glm-5-2","provider/GLM_5_2-thinking"],"enabled":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	up := m.Get().Upstreams[0]
	if len(up.Models) != 1 || up.Models[0] != "glm-5.2" {
		t.Fatalf("canonical models = %#v", up.Models)
	}
	if aliases := up.ModelAliases["glm-5.2"]; len(aliases) != 4 {
		t.Fatalf("aliases = %#v", aliases)
	}
	persisted, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"model_aliases"`) {
		t.Fatalf("migration was not persisted: %s", persisted)
	}
}

func TestHitchanceDefaultsAndLegacyCycleMigration(t *testing.T) {
	newPath := filepath.Join(t.TempDir(), "new.json")
	newManager, err := Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := newManager.Get().Hitchance; got.RetryCycles != 0 || got.ResponseStartTimeoutSeconds != 20 {
		t.Fatalf("new defaults = %#v", got)
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"failover":{"mode":"stop_on_selected","default_fail_codes":[500]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyManager, err := Load(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := legacyManager.Get().Hitchance; got.RetryCycles != 0 || got.ResponseStartTimeoutSeconds != 20 {
		t.Fatalf("legacy migration = %#v", got)
	}
	persisted, _ := ReadFile(legacyPath)
	if !strings.Contains(string(persisted), `"retry_cycles": 0`) || !strings.Contains(string(persisted), `"response_start_timeout_seconds": 20`) {
		t.Fatalf("migration not persisted: %s", persisted)
	}
}

func TestHitchanceTTLDefaultsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	mgr, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fo := mgr.Get().Hitchance
	if fo.UpstreamCacheTTLHours != 24 {
		t.Fatalf("expected UpstreamCacheTTLHours = 24, got %d", fo.UpstreamCacheTTLHours)
	}
	if fo.StickyUpstreamTTLHours != 24 {
		t.Fatalf("expected StickyUpstreamTTLHours = 24, got %d", fo.StickyUpstreamTTLHours)
	}

	err = mgr.Update(func(c *Config) error {
		c.Hitchance.UpstreamCacheTTLHours = 48
		c.Hitchance.StickyUpstreamTTLHours = 12
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fo2 := reloaded.Get().Hitchance
	if fo2.UpstreamCacheTTLHours != 48 {
		t.Fatalf("expected UpstreamCacheTTLHours = 48, got %d", fo2.UpstreamCacheTTLHours)
	}
	if fo2.StickyUpstreamTTLHours != 12 {
		t.Fatalf("expected StickyUpstreamTTLHours = 12, got %d", fo2.StickyUpstreamTTLHours)
	}
}
