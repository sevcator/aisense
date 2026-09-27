package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The saved config must be useless to anyone who simply takes the file, and
// aisense must still start from it with no password typed in.
func TestSavedConfigIsEncryptedAndStillLoadsByItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "k1", Name: "laptop", Key: "sk-client-secret", Enabled: true}}
		c.Upstreams = []*Upstream{{ID: "up", Enabled: true, BaseURL: "https://api.example.com/v1", Models: []string{"model"}, APIKeys: []string{"upstream-secret"}}}
		c.Server.Admin.Password = "admin-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-client-secret", "upstream-secret", "admin-secret", "api.example.com"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q is readable in the saved file", secret)
		}
	}
	if json.Valid(raw) {
		t.Fatal("the saved file is still plain JSON")
	}
	// A restart reads it back without anyone entering anything.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := reloaded.Get()
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != "sk-client-secret" {
		t.Fatalf("api keys after restart = %+v", cfg.APIKeys)
	}
	if len(cfg.Upstreams) != 1 || len(cfg.Upstreams[0].APIKeys) != 1 {
		t.Fatalf("upstreams after restart = %+v", cfg.Upstreams)
	}
}

// An installation that is still in plain text is encrypted on the next start,
// without losing anything.
func TestPlainConfigIsEncryptedOnTheNextStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	plain := `{"upstreams":[{"id":"up","enabled":true,"base_url":"https://api.example.com/v1","api_keys":["upstream-secret"],"models":["model"],"priority":10}],"api_keys":[]}`
	if err := os.WriteFile(path, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "upstream-secret") {
		t.Fatal("the plain file was left readable")
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ups := reloaded.Get().Upstreams; len(ups) != 1 || ups[0].APIKeys[0] != "upstream-secret" {
		t.Fatalf("upstreams = %+v", ups)
	}
}

// A file that cannot be decrypted must stop aisense, never start it empty and
// overwrite what is still on disk.
func TestUnreadableConfigRefusesToStartInsteadOfWipingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "k1", Key: "sk-client-secret", Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), raw...)
	damaged[len(damaged)-1] ^= 0xff
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a damaged config must not load")
	}
	after, err := os.ReadFile(path)
	if err != nil || len(after) != len(damaged) {
		t.Fatalf("the file was rewritten: err=%v", err)
	}
}

// A config written before the cooling retry existed must get the default, and
// an explicit zero must stay off.
func TestCoolingRetryDefaultsForOlderConfigs(t *testing.T) {
	for _, tc := range []struct {
		name, hitchance string
		want            int
	}{
		{"older config", `{"enabled":true,"retry_cycles":2}`, 60},
		{"explicitly off", `{"enabled":true,"retry_cycles":2,"cooling_retry_seconds":0}`, 0},
		{"explicit value", `{"enabled":true,"retry_cycles":2,"cooling_retry_seconds":45}`, 45},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"hitchance":`+tc.hitchance+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := manager.Get().Hitchance.CoolingRetrySeconds; got != tc.want {
			t.Fatalf("%s: cooling_retry_seconds = %d, want %d", tc.name, got, tc.want)
		}
	}
}
