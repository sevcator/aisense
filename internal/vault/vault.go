// Package vault keeps aisense's files unreadable outside the account that
// wrote them. The payload is encrypted with a fresh AES-256-GCM key, and that
// key is wrapped by the operating system itself: Windows DPAPI for the account
// aisense runs as, or a key file in the user's own config directory. Nothing
// has to be typed at startup, so aisense still starts unattended, but a copy of
// the file on another machine or under another account is useless.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// magic marks a sealed file and carries the format version.
const magic = "AISENSE\x01"

const (
	keySize   = 32
	nonceSize = 12
	// A wrapped key stays small; anything larger is a damaged or foreign file.
	maxWrappedKey = 8 << 10
)

// ErrNotSealed reports a file that was not written by this package.
var ErrNotSealed = errors.New("not a sealed aisense file")

// IsSealed reports whether data was written by Seal.
func IsSealed(data []byte) bool {
	return len(data) >= len(magic) && string(data[:len(magic)]) == magic
}

// The data key is drawn once per process and reused with a fresh nonce for
// every save: asking the operating system to wrap a new key on each write
// costs two system calls and buys nothing, since the wrapping is the same.
var (
	keyMu       sync.Mutex
	sealKey     []byte
	sealWrapped []byte
	openedKeys  sync.Map // wrapped key -> unwrapped key, so re-reads skip the OS too
)

func sealingKey() (key, wrapped []byte, err error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	if sealKey != nil {
		return sealKey, sealWrapped, nil
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, fmt.Errorf("vault: random key: %w", err)
	}
	if wrapped, err = wrapKey(key); err != nil {
		return nil, nil, err
	}
	if len(wrapped) > maxWrappedKey {
		return nil, nil, errors.New("vault: wrapped key is too large")
	}
	sealKey, sealWrapped = key, wrapped
	openedKeys.Store(string(wrapped), key)
	return key, wrapped, nil
}

// Seal encrypts plain for this account. The result is verified before it is
// returned, so a file that cannot be read back is never written to disk.
func Seal(plain []byte) ([]byte, error) {
	key, wrapped, err := sealingKey()
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: random nonce: %w", err)
	}
	out := make([]byte, 0, len(magic)+4+len(wrapped)+nonceSize+len(plain)+gcm.Overhead())
	out = append(out, magic...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(wrapped)))
	out = append(out, wrapped...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plain, nil)
	// Never hand back something this machine cannot read again.
	if check, err := Open(out); err != nil {
		return nil, fmt.Errorf("vault: sealed data failed verification: %w", err)
	} else if string(check) != string(plain) {
		return nil, errors.New("vault: sealed data did not verify")
	}
	return out, nil
}

// Open decrypts data written by Seal on this machine and account.
func Open(data []byte) ([]byte, error) {
	if !IsSealed(data) {
		return nil, ErrNotSealed
	}
	rest := data[len(magic):]
	if len(rest) < 4 {
		return nil, errors.New("vault: file is truncated")
	}
	size := binary.BigEndian.Uint32(rest[:4])
	rest = rest[4:]
	if size > maxWrappedKey || len(rest) < int(size)+nonceSize {
		return nil, errors.New("vault: file is damaged")
	}
	key, err := openKey(rest[:size])
	if err != nil {
		return nil, err
	}
	rest = rest[size:]
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, rest[:nonceSize], rest[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("vault: file was changed or belongs to another account: %w", err)
	}
	return plain, nil
}

// OpenOrPlain reads both sealed files and files still in plain text, which is
// what an installation looks like before its first encrypted save.
func OpenOrPlain(data []byte) (plain []byte, sealed bool, err error) {
	if !IsSealed(data) {
		return data, false, nil
	}
	plain, err = Open(data)
	return plain, true, err
}

// openKey unwraps a file's data key, remembering the handful of keys this
// process meets so repeated reads do not call into the operating system.
func openKey(wrapped []byte) ([]byte, error) {
	if cached, ok := openedKeys.Load(string(wrapped)); ok {
		return cached.([]byte), nil
	}
	key, err := unwrapKey(wrapped)
	if err != nil {
		return nil, err
	}
	openedKeys.Store(string(wrapped), key)
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: gcm: %w", err)
	}
	return gcm, nil
}
