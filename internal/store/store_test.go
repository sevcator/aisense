package store

import (
	"path/filepath"
	"testing"
)

// Clear wipes every bucket and the empty state is what gets persisted, so a
// reopened store stays empty too.
func TestClearWipesAllBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Add("key", "model", 10, 20, true)
	s.Add("key", "model", 1, 2, false)
	if snap := s.Snapshot(); len(snap) != 1 {
		t.Fatalf("buckets before clear = %d, want 1", len(snap))
	}
	s.Clear()
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Fatalf("buckets after clear = %d, want 0", len(snap))
	}
	if in, out := s.DayTokens("key"); in != 0 || out != 0 {
		t.Fatalf("day tokens after clear = %d/%d, want 0/0", in, out)
	}
	s.Close()
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if snap := reopened.Snapshot(); len(snap) != 0 {
		t.Fatalf("reopened store has %d buckets, want 0", len(snap))
	}
}
