package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The sealing key is cached for the process; tests that change where the key
// lives start from a clean slate.
func resetKeys(t *testing.T) {
	t.Helper()
	keyMu.Lock()
	sealKey, sealWrapped = nil, nil
	keyMu.Unlock()
	openedKeys = sync.Map{}
	t.Cleanup(func() {
		keyMu.Lock()
		sealKey, sealWrapped = nil, nil
		keyMu.Unlock()
		openedKeys = sync.Map{}
	})
}

// The key file is how every system other than Windows protects the data key,
// and how any system does it when the key is given a home of its own.
func TestKeyFileSealsAndOpensOnEverySystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.key")
	t.Setenv(KeyPathEnv, path)
	resetKeys(t)

	plain := []byte(`{"api_keys":[{"key":"sk-secret-value"}]}`)
	sealed, err := Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-secret-value")) {
		t.Fatal("the secret is readable in the sealed file")
	}
	// The wrapped key must say it came from the key file, not from DPAPI.
	if tag := sealed[len(magic)+4]; tag != tagKeyFile {
		t.Fatalf("wrapped with method %d, want the key file", tag)
	}
	if key, err := os.ReadFile(path); err != nil || len(key) != keySize {
		t.Fatalf("key file: %d bytes, err=%v", len(key), err)
	}
	opened, err := Open(sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("round trip: %v", err)
	}
	// Another machine, with its own key file, cannot read it.
	other := filepath.Join(t.TempDir(), "vault.key")
	t.Setenv(KeyPathEnv, other)
	resetKeys(t)
	if _, err := Open(sealed); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("a foreign key file opened the file: %v", err)
	}
}
