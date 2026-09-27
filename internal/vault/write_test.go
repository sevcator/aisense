package vault

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSealedReplacesTheFileAndReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("older content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSealed(path, []byte("new content")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(raw)
	if err != nil || string(opened) != "new content" {
		t.Fatalf("opened %q err=%v", opened, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// A replace that fails for a moment — a scanner holding the file — must not
// lose the save.
func TestReplaceRidesOutABusyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	tries := 0
	original := renameFile
	renameFile = func(from, to string) error {
		tries++
		if tries < 3 {
			return errors.New("Access is denied")
		}
		return original(from, to)
	}
	t.Cleanup(func() { renameFile = original })

	if err := WriteSealed(path, []byte("payload")); err != nil {
		t.Fatalf("save lost to a busy file: %v", err)
	}
	if tries != 3 {
		t.Fatalf("tries = %d", tries)
	}
	raw, _ := os.ReadFile(path)
	if opened, err := Open(raw); err != nil || string(opened) != "payload" {
		t.Fatalf("opened %q err=%v", opened, err)
	}
}

// A replace that never succeeds reports the reason instead of pretending.
func TestReplaceGivesUpWithTheReason(t *testing.T) {
	original := renameFile
	renameFile = func(string, string) error { return errors.New("Access is denied") }
	t.Cleanup(func() { renameFile = original })

	err := WriteSealed(filepath.Join(t.TempDir(), "config.json"), []byte("payload"))
	if err == nil {
		t.Fatal("a replace that never succeeds must report an error")
	}
	if got := err.Error(); !strings.Contains(got, "could not replace") || !strings.Contains(got, "Access is denied") {
		t.Fatalf("err = %q", got)
	}
}
