//go:build !windows

package vault

import "fmt"

// Everywhere but Windows the data key is wrapped with the master key file in
// the user's own config directory (0600), which stays outside the project.
func wrapKey(key []byte) ([]byte, error) { return wrapWithKeyFile(key) }

func unwrapKey(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("vault: wrapped key is missing")
	}
	switch blob[0] {
	case tagKeyFile:
		return unwrapWithKeyFile(blob[1:])
	case tagDPAPI:
		return nil, fmt.Errorf("vault: this file was sealed by Windows and can only be read there")
	default:
		return nil, fmt.Errorf("vault: unknown key protection %d", blob[0])
	}
}
