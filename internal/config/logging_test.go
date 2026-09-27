package config

import (
	"aisense/internal/debuglog"
	"path/filepath"
	"testing"
)

func TestLoggingSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := debuglog.NewDynamic(filepath.Dir(path), "", func() bool { return m.Get().LoggingEnabled })
	defer logger.Close()
	if err := m.SetLoggingValidator(logger.Validate); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		if err := m.Update(func(c *Config) error { c.LoggingEnabled = enabled; return nil }); err != nil {
			t.Fatal(err)
		}
		reloaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.Get().LoggingEnabled != enabled {
			t.Fatalf("persisted logging != %v", enabled)
		}
	}
}
