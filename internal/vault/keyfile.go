package vault

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// How a data key was wrapped. The tag travels with the file so a machine can
// still open what it wrote before, whichever method was available then.
const (
	tagDPAPI   byte = 1 // Windows, bound to the account aisense runs as
	tagKeyFile byte = 2 // master key in the user's own config directory
)

// KeyPathEnv names a file to keep the master key in. It is how a server or a
// container without a home directory, or an installation that keeps the key on
// removable media, chooses the location.
const KeyPathEnv = "AISENSE_VAULT_KEY"

var (
	keyFileMu sync.Mutex
	// fallbackDir is used when the operating system reports no user config
	// directory, which happens for services and containers with no home.
	fallbackDir   string
	warnedNextTo  bool
	fallbackDirMu sync.RWMutex
)

// SetFallbackKeyDir names the directory to keep the master key in when the
// system has no user config directory. aisense passes the directory of the
// config file, so it still starts where no home exists.
func SetFallbackKeyDir(dir string) {
	fallbackDirMu.Lock()
	fallbackDir = dir
	fallbackDirMu.Unlock()
}

// keyFilePath is the master key, by preference outside the project folder, so
// copying the folder (or a cloud-synced backup of it) does not carry the key.
func keyFilePath() (string, error) {
	if path := os.Getenv(KeyPathEnv); path != "" {
		return path, nil
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "aisense", "vault.key"), nil
	}
	fallbackDirMu.RLock()
	dir, warned := fallbackDir, warnedNextTo
	fallbackDirMu.RUnlock()
	if dir == "" {
		return "", fmt.Errorf("vault: no user config directory; set %s to a file only this account can read", KeyPathEnv)
	}
	if !warned {
		fallbackDirMu.Lock()
		warnedNextTo = true
		fallbackDirMu.Unlock()
		log.Printf("[vault] no user config directory: keeping the key in %s. A copy of that whole directory can be read elsewhere; set %s to move the key out.", dir, KeyPathEnv)
	}
	return filepath.Join(dir, "vault.key"), nil
}

// masterKey loads the machine's master key, creating it on first use.
func masterKey() ([]byte, error) {
	keyFileMu.Lock()
	defer keyFileMu.Unlock()
	path, err := keyFilePath()
	if err != nil {
		return nil, err
	}
	switch key, err := os.ReadFile(path); {
	case err == nil && len(key) == keySize:
		restrictKeyFile(path)
		return key, nil
	case err == nil:
		return nil, fmt.Errorf("vault: %s is not a %d-byte key", path, keySize)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("vault: read key: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("vault: random key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("vault: create key directory: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("vault: write key: %w", err)
	}
	return key, nil
}

func wrapWithKeyFile(key []byte) ([]byte, error) {
	master, err := masterKey()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: random nonce: %w", err)
	}
	out := append([]byte{tagKeyFile}, nonce...)
	return gcm.Seal(out, nonce, key, nil), nil
}

func unwrapWithKeyFile(blob []byte) ([]byte, error) {
	if len(blob) < nonceSize {
		return nil, errors.New("vault: wrapped key is damaged")
	}
	master, err := masterKey()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	key, err := gcm.Open(nil, blob[:nonceSize], blob[nonceSize:], nil)
	if err != nil {
		path, _ := keyFilePath()
		return nil, fmt.Errorf("vault: this file was sealed with a different key (%s): %w", path, err)
	}
	return key, nil
}
