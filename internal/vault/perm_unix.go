//go:build !windows

package vault

import (
	"log"
	"os"
)

// restrictKeyFile keeps the master key readable only by its owner. A key that
// arrived from a backup, a copy or a loose umask is tightened in place.
func restrictKeyFile(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("[vault] %s can be read by other accounts and could not be tightened: %v", path, err)
	}
}
