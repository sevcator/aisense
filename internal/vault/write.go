package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// renameFile is swapped in tests to rehearse a failing replace.
var renameFile = os.Rename

// WriteSealed encrypts plain and puts it at path, replacing whatever is there
// in one step so a crash can never leave a half-written file.
//
// The replace is retried for a moment: on Windows an antivirus scanner or the
// search indexer can hold a file open just long enough for a single rename to
// fail with "Access is denied", and losing a config save to that would be
// absurd.
func WriteSealed(path string, plain []byte) error {
	sealed, err := Seal(plain)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func replaceFile(tmp, path string) error {
	var err error
	for wait := 10 * time.Millisecond; ; wait *= 2 {
		if err = renameFile(tmp, path); err == nil {
			return nil
		}
		if os.IsNotExist(err) || wait > 320*time.Millisecond {
			break
		}
		time.Sleep(wait)
	}
	return fmt.Errorf("vault: could not replace %s: %w", path, err)
}
