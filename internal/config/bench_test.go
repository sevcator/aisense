package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"aisense/internal/modelalias"
	"aisense/internal/vault"
)

// realisticConfig mirrors the shape of a working installation: 20 upstreams,
// ~110 models spread over them, aliases for most of them, 28 upstream keys.
func realisticConfig() *Config {
	cfg := Default()
	sizes := []int{5, 9, 9, 20, 8, 6, 9, 2, 1, 10, 1, 1, 1, 2, 1, 7, 2, 10, 1, 7}
	for i, size := range sizes {
		up := &Upstream{
			ID: fmt.Sprintf("up-%02d", i), Enabled: i < 14, Priority: 10,
			BaseURL:      fmt.Sprintf("https://api%02d.example.com/v1", i),
			APIKeys:      []string{fmt.Sprintf("key-%02d-a", i), fmt.Sprintf("key-%02d-b", i)},
			ModelAliases: map[string][]string{},
		}
		for m := 0; m < size; m++ {
			model := fmt.Sprintf("model-%02d-%02d", i, m)
			up.Models = append(up.Models, model)
			up.ModelAliases[model] = []string{model + "-raw", "vendor/" + model}
		}
		cfg.Upstreams = append(cfg.Upstreams, up)
	}
	cfg.APIKeys = []*APIKey{{ID: "k1", Key: "sk-one", Enabled: true}, {ID: "k2", Key: "sk-two", Enabled: true}}
	return cfg
}

func benchManager(b *testing.B) *Manager {
	b.Helper()
	path := filepath.Join(b.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := m.Update(func(c *Config) error {
		base := realisticConfig()
		c.Upstreams, c.APIKeys = base.Upstreams, base.APIKeys
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return m
}

// One Update is the whole save path: clone, validate, normalize, encrypt, write.
func BenchmarkUpdate(b *testing.B) {
	m := benchManager(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.Update(func(c *Config) error {
			c.Upstreams[i%len(c.Upstreams)].Priority = 10 + i%3
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNormalizeUpstreams(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeUpstreams(cfg)
	}
}

func BenchmarkMarshalConfig(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.MarshalIndent(cfg, "", "  "); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSealConfig(b *testing.B) {
	cfg := realisticConfig()
	plain, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(plain)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := vault.Seal(plain); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoad(b *testing.B) {
	m := benchManager(b)
	path := m.path
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Load(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildCatalogs(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, up := range cfg.Upstreams {
			catalog := modelalias.Build(up.Models, up.ModelAliases)
			_ = catalog
		}
	}
}

func BenchmarkConsolidate(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConsolidateExactUpstreams(cfg.Upstreams)
	}
}

func BenchmarkHitchanceIdentity(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, up := range cfg.Upstreams {
			_ = up.HitchanceIdentity()
		}
	}
}

func BenchmarkNormalizeAPIKeysAll(b *testing.B) {
	cfg := realisticConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, up := range cfg.Upstreams {
			_ = NormalizeAPIKeys(up.APIKeys)
		}
	}
}

// An observation that changes nothing must not encrypt or write anything.
func BenchmarkUpdateNoChange(b *testing.B) {
	m := benchManager(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.Update(func(c *Config) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
