package main

import "testing"

func TestCategoryNeverReturnsInput(t *testing.T) {
	for _, s := range []string{"Get https://secret@example.invalid/models?key=private: EOF", "Bearer private", "https://example.invalid?token=private deadline exceeded"} {
		got := category(s)
		if got != "eof" && got != "other" && got != "deadline" {
			t.Fatalf("unsafe category %q", got)
		}
	}
}
