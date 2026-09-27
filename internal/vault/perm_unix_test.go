//go:build !windows

package vault

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// A key file that arrived from a backup or a loose umask is tightened, so no
// other account on the machine can read it.
func TestLooseKeyFilePermissionsAreTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.key")
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, key, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KeyPathEnv, path)
	resetKeys(t)

	if _, err := Seal([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("key file mode is %v, want owner-only", perm)
	}
}
