package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aisense/internal/vault"
)

func TestLegacySealedConfigMigratesToPlainSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := []byte(`{"server":{"admin":{"username":"seva","password":"old-password","port":8082}},"api_keys":[{"id":"client","key":"client-secret"}],"upstreams":[{"id":"up","api_keys":["up-secret"],"base_url":"https://api.example.com/v1"}]}`)
	if err := vault.WriteSealed(path, legacy); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Get().Server.Admin.Password != "old-password" || m.Get().APIKeys[0].Key != "client-secret" || m.Get().Upstreams[0].APIKeys[0] != "up-secret" {
		t.Fatal("legacy credentials were not preserved")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || strings.Contains(string(raw), "old-password") || strings.Contains(string(raw), "client-secret") || strings.Contains(string(raw), "up-secret") {
		t.Fatal("legacy settings were not safely split")
	}
}

func TestCredentialVersionsKeepOldConfigReadableAcrossReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "client", Key: "old-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldSettings, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		copy := *c.APIKeys[0]
		copy.Key = "new-key"
		c.APIKeys[0] = &copy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, oldSettings, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().APIKeys[0].Key; got != "old-key" {
		t.Fatalf("old config loaded key %q", got)
	}
}

func TestSettingsUpdateDoesNotRewriteCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credentialsPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.Server.OpenAI.Port = 9010
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(credentialsPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("changing a harmless setting rewrote the credential store")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"port": 9010`) {
		t.Fatal("port is not readable in settings")
	}
}

func TestManualPlainSettingsEditKeepsCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "client", Key: "keep-me"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]json.RawMessage
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal(public["server"], &server); err != nil {
		t.Fatal(err)
	}
	var openai map[string]json.RawMessage
	if err := json.Unmarshal(server["openai"], &openai); err != nil {
		t.Fatal(err)
	}
	openai["port"] = json.RawMessage("9011")
	server["openai"], _ = json.Marshal(openai)
	public["server"], _ = json.Marshal(server)
	raw, _ = json.MarshalIndent(public, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().Server.OpenAI.Port != 9011 || reloaded.Get().APIKeys[0].Key != "keep-me" {
		t.Fatal("manual settings edit changed the credentials")
	}
}

func TestCredentialedURLsStayOutOfPlainConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "up", BaseURL: "https://user:password@api.example.com/v1?token=query-secret", OAuth: &OAuthCfg{TokenURL: "https://auth.example.com/token?key=oauth-query", ClientID: "private-id", ClientSecret: "oauth-secret"}}}
		c.Proxies.List = []*ProxyEntry{{URL: "http://proxy-user:proxy-secret@proxy.example.com:8080"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"password", "query-secret", "oauth-query", "private-id", "oauth-secret", "proxy-user", "proxy-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q leaked into plain config", secret)
		}
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().Upstreams[0].BaseURL != m.Get().Upstreams[0].BaseURL || reloaded.Get().Upstreams[0].OAuth.TokenURL != m.Get().Upstreams[0].OAuth.TokenURL || reloaded.Get().Proxies.List[0].URL != m.Get().Proxies.List[0].URL {
		t.Fatal("credentialed URLs changed on reload")
	}
}

func TestReadAndWriteFileKeepBothStoresInSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "client", Key: "secret"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	complete, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(complete, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeys[0].Key != "secret" {
		t.Fatal("ReadFile omitted the key")
	}
	cfg.Server.OpenAI.Port = 9999
	complete, _ = json.Marshal(&cfg)
	if err := WriteFile(path, complete); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().Server.OpenAI.Port != 9999 || reloaded.Get().APIKeys[0].Key != "secret" {
		t.Fatal("WriteFile did not preserve settings and credentials")
	}
}

func TestExportCanMigrateWithoutTheOriginalVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "client", Key: "transfer-secret"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	export, err := ExportFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(export, []byte(`"credentials_revision"`)) || !bytes.Contains(export, []byte("transfer-secret")) {
		t.Fatal("export must contain the key but not a reference to the old vault")
	}
	destination := filepath.Join(t.TempDir(), "transfer.json")
	if err := os.WriteFile(destination, export, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(destination)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Get().APIKeys[0].Key != "transfer-secret" {
		t.Fatal("the destination lost the exported key")
	}
}

func TestMissingSettingsDoNotReplaceExistingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		c.APIKeys = []*APIKey{{ID: "client", Key: "preserve-me"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	protected, err := os.ReadFile(credentialsPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("missing settings silently replaced existing credentials")
	}
	after, err := os.ReadFile(credentialsPath(path))
	if err != nil || !bytes.Equal(protected, after) {
		t.Fatal("credential store changed when settings were missing")
	}
}
