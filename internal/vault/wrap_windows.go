//go:build windows

package vault

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Windows protects the data key with DPAPI under the account aisense runs as:
// no secret of ours is stored anywhere, and another account or machine cannot
// unprotect the blob even with the file in hand.
var (
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	kernel32dll       = syscall.NewLazyDLL("kernel32.dll")
	procProtectData   = crypt32.NewProc("CryptProtectData")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree     = kernel32dll.NewProc("LocalFree")
)

// cryptProtectUIForbidden: never prompt, aisense runs unattended.
const cryptProtectUIForbidden = 0x1

// appEntropy binds a blob to aisense, so another DPAPI user of this account
// cannot unprotect it by accident.
var appEntropy = []byte("aisense.vault.v1")

type dataBlob struct {
	size uint32
	data *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{size: uint32(len(b)), data: &b[0]}
}

func (b dataBlob) bytes() []byte {
	if b.data == nil || b.size == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice(b.data, b.size)...)
}

func (b dataBlob) free() {
	if b.data != nil {
		_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(b.data)))
	}
}

func wrapKey(key []byte) ([]byte, error) {
	// A named key file is a deliberate choice — a portable installation, or one
	// key shared by a service account — so it wins over DPAPI.
	if os.Getenv(KeyPathEnv) != "" {
		return wrapWithKeyFile(key)
	}
	if blob, err := protectDPAPI(key); err == nil {
		return append([]byte{tagDPAPI}, blob...), nil
	}
	// A locked-down or roaming profile can refuse DPAPI; the key file in the
	// user's config directory keeps the file readable on this machine.
	return wrapWithKeyFile(key)
}

func unwrapKey(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("vault: wrapped key is missing")
	}
	switch blob[0] {
	case tagDPAPI:
		return unprotectDPAPI(blob[1:])
	case tagKeyFile:
		return unwrapWithKeyFile(blob[1:])
	default:
		return nil, fmt.Errorf("vault: unknown key protection %d", blob[0])
	}
}

func protectDPAPI(plain []byte) ([]byte, error) {
	in, entropy, out := newBlob(plain), newBlob(appEntropy), dataBlob{}
	ret, _, err := procProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, uintptr(unsafe.Pointer(&entropy)), 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("vault: windows could not protect the key: %w", err)
	}
	defer out.free()
	return out.bytes(), nil
}

func unprotectDPAPI(sealed []byte) ([]byte, error) {
	in, entropy, out := newBlob(sealed), newBlob(appEntropy), dataBlob{}
	ret, _, err := procUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, uintptr(unsafe.Pointer(&entropy)), 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("vault: this file belongs to another Windows account or machine: %w", err)
	}
	defer out.free()
	return out.bytes(), nil
}
