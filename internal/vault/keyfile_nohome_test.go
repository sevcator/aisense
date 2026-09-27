//go:build !windows

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A service or container without a home directory keeps the key beside the
// config instead of refusing to start.
func TestKeyLivesBesideTheConfigWhenThereIsNoHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(KeyPathEnv, "")
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	SetFallbackKeyDir(dir)
	t.Cleanup(func() { SetFallbackKeyDir("") })
	resetKeys(t)

	if _, err := os.UserConfigDir(); err == nil {
		t.Skip("this system still reports a user config directory")
	}
	sealed, err := Seal([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); err != nil {
		t.Fatalf("key was not written beside the config: %v", err)
	}
	if opened, err := Open(sealed); err != nil || string(opened) != "payload" {
		t.Fatalf("round trip: %v", err)
	}
}

// Without a home and without a directory to fall back on, the failure says
// what to do instead of being a bare error.
func TestMissingKeyLocationExplainsItself(t *testing.T) {
	t.Setenv(KeyPathEnv, "")
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	SetFallbackKeyDir("")
	resetKeys(t)
	if _, err := os.UserConfigDir(); err == nil {
		t.Skip("this system still reports a user config directory")
	}
	if _, err := Seal([]byte("payload")); err == nil || !strings.Contains(err.Error(), KeyPathEnv) {
		t.Fatalf("err = %v, want advice naming %s", err, KeyPathEnv)
	}
}
