package modelalias

import "testing"

func TestFuzzyMatch(t *testing.T) {
	for _, tc := range []struct {
		name, requested, target string
		want                    bool
	}{
		{"boundary", "abcdefghijklmnopqrsx", "abcdefghijklmnopqrst", true},
		{"below", "abcdefghijklmnopqrx", "abcdefghijklmnopqrs", false},
		{"prefix", "client/CLAUDEe-sonnet-4.6-thinking", "provider/claude-sonnet-4.6-thinking", true},
		{"version", "claudee-sonnet-4.6", "claude-sonnet-4.5", false},
		{"numeric boundaries", "claudee-sonnet-5.6", "claude-sonnet-56", false},
		{"size", "abcdefghijklmnopqrst-20b", "abcdefghijklmnopqrst-21b", false},
		{"type", "abcdefghijklmnopqrst-opus", "abcdefghijklmnopqrst-sonnet", false},
		{"reasoning", "abcdefghijklmnopqrst-low", "abcdefghijklmnopqrst-high", false},
		{"suffix typo", "abcdefghijklmnopqrst-minj", "abcdefghijklmnopqrst-mini", false},
		{"suffix removal", "abcdefghijklmnopqrst-thinking", "abcdefghijklmnopqrst", false},
		{"pro", "abcdefghijklmnopqrst-pro", "abcdefghijklmnopqrst", false},
		{"codex", "abcdefghijklmnopqrst-codex", "abcdefghijklmnopqrst", false},
		{"audio", "abcdefghijklmnopqrst-audio", "abcdefghijklmnopqrst-image", false},
		{"gpt version", "gpt5.6", "gpt5.5", false},
		{"gpt digits", "gpt5.6", "gpt56", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FuzzyMatch([]string{tc.target}, tc.requested)
			if ok != tc.want || ok && got != tc.target {
				t.Fatalf("got %q, %v", got, ok)
			}
		})
	}
}

func TestFuzzyMatchAmbiguityAndNoListingMerge(t *testing.T) {
	a, b := "abcdefghijklmnopqrsa", "abcdefghijklmnopqrsb"
	if _, ok := FuzzyMatch([]string{a, b}, "abcdefghijklmnopqrsx"); ok {
		t.Fatal("accepted tied families")
	}
	if got, ok := FuzzyMatch([]string{a, b}, b); !ok || got != b {
		t.Fatalf("exact precedence: %q %v", got, ok)
	}
	if _, ok := FuzzyMatch([]string{"one/" + a, "two/" + a}, "abcdefghijklmnopqrsx"); !ok {
		t.Fatal("same family treated as ambiguous")
	}
	if got := Build([]string{a, b}, nil); len(got.Models) != 2 {
		t.Fatalf("fuzzy listing merge: %#v", got)
	}
	if Matches(a, b) {
		t.Fatal("fuzzy leaked into policy")
	}
}
