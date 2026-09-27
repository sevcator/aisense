package config

import (
	"path/filepath"
	"testing"
)

func TestAutoModelsDiscoveryPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Get().AutoModelsDiscovery {
		t.Fatal("default must be false")
	}
	for _, enabled := range []bool{true, false} {
		if err := cfg.Update(func(c *Config) error { c.AutoModelsDiscovery = enabled; return nil }); err != nil {
			t.Fatal(err)
		}
		reloaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.Get().AutoModelsDiscovery != enabled {
			t.Fatal("setting not persisted")
		}
	}
}
