//go:build windows

package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// On Windows the data key is protected by the account itself, so a working
// installation keeps no key file anywhere: nothing to copy, nothing to lose.
func TestWindowsKeepsNoKeyFile(t *testing.T) {
	t.Setenv(KeyPathEnv, "")
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	resetKeys(t)

	sealed, err := Seal([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if tag := sealed[len(magic)+4]; tag != tagDPAPI {
		t.Fatalf("wrapped with method %d, want the account's own protection", tag)
	}
	if _, err := os.Stat(filepath.Join(home, "aisense", "vault.key")); !os.IsNotExist(err) {
		t.Fatalf("a key file was written: %v", err)
	}
	opened, err := Open(sealed)
	if err != nil || !bytes.Equal(opened, []byte("payload")) {
		t.Fatalf("round trip: %v", err)
	}
}
