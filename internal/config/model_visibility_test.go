package config

import (
	"path/filepath"
	"testing"
)

func TestHiddenAndPerKeyBlocksPersistCloneAndMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := KeyFingerprint("secret")
	if err := cfg.Update(func(c *Config) error {
		c.Upstreams = []*Upstream{{ID: "up", Enabled: true, HiddenInvalid: true, Models: []string{"provider/gpt-test"}, APIKeys: []string{"secret"}, KeyBlockedModels: map[string][]string{hash: {"provider/gpt-test"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := cfg.Get()
	if err := cfg.Update(func(c *Config) error { c.Upstreams[0].KeyBlockedModels[hash][0] = "other"; return nil }); err != nil {
		t.Fatal(err)
	}
	if before.Upstreams[0].KeyBlockedModels[hash][0] != "provider/gpt-test" {
		t.Fatal("snapshot map mutated")
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Get().Upstreams[0].HiddenInvalid || !reloaded.Get().Upstreams[0].KeyModelBlocked("secret", "other") {
		t.Fatal("fields lost on reload")
	}
	dst := &Upstream{}
	MergeUpstreamKeys(dst, before.Upstreams[0])
	if !dst.KeyModelBlocked("secret", "provider/gpt-test") {
		t.Fatal("merged credentials lost route blocks")
	}
	if dst.KeyModelBlocked("another-key", "provider/gpt-test") || dst.KeyModelBlocked("secret", "gpt-test") {
		t.Fatal("block expanded across credentials or aliases")
	}
}
