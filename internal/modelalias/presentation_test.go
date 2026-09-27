package modelalias

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestPresentationNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want []string
		fast     bool
	}{
		{"wrapper", []string{"0g-deepseek-v4-flash", "deepseek-v4-flash"}, []string{"deepseek-v4-flash"}, false},
		{"wrapper alone", []string{"0g-deepseek-v4-flash"}, []string{"0g-deepseek-v4-flash"}, false},
		{"other wrapper", []string{"foo-deepseek-v4-flash", "deepseek-v4-flash"}, []string{"foo-deepseek-v4-flash", "deepseek-v4-flash"}, false},
		{"fuzzy pair", []string{"abcdefghijklmnopqrst", "abcdefghijklmnopqrsx"}, []string{"abcdefghijklmnopqrst"}, false},
		{"ambiguous", []string{"abcdefghijklmnopqrsa", "abcdefghijklmnopqrsb", "abcdefghijklmnopqrsx"}, []string{"abcdefghijklmnopqrsa", "abcdefghijklmnopqrsb", "abcdefghijklmnopqrsx"}, false},
		{"protected", []string{"longmodelname-5.6", "longmodelname-56", "longmodelname-5.5", "longmodelname-20b", "longmodelname-21b", "longmodelname-audio", "longmodelname-image"}, []string{"longmodelname-5.6", "longmodelname-56", "longmodelname-5.5", "longmodelname-20b", "longmodelname-21b", "longmodelname-audio", "longmodelname-image"}, false},
		{"standalone variants", []string{"model-low", "model-high", "model-medium", "model-minimal", "model-extra", "model-extended", "model-max", "model-xhigh", "model-mini", "model-small", "model-nano", "model-lite", "model-tiny", "model-micro", "model-thinking", "model-thinking-low", "*", "auto"}, []string{}, false},
		{"fast off", []string{"model-fast"}, []string{"model-fast"}, false},
		{"fast on", []string{"model-fast", "model-fast-high"}, []string{"model-fast"}, true},
		{"spellings", []string{"provider/gpt5.6-sol", "gpt-5.6-sol"}, []string{"gpt-5.6-sol"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "standalone variants" {
				tc.want = append([]string{}, tc.in[:len(tc.in)-2]...)
				// thinking-low groups into the advertised thinking route.
				tc.want = tc.want[:len(tc.want)-1]
			}
			before := append([]string(nil), tc.in...)
			sort.Strings(tc.want)
			if got := PresentationNames(tc.in, VariantOptions{Reasoning: true, Fast: tc.fast}); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(before, tc.in) {
				t.Fatal("source models mutated")
			}
			for i, j := 0, len(tc.in)-1; i < j; i, j = i+1, j-1 {
				tc.in[i], tc.in[j] = tc.in[j], tc.in[i]
			}
			if got := PresentationNames(tc.in, VariantOptions{Reasoning: true, Fast: tc.fast}); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("order-dependent: %v", got)
			}
		})
	}
}

func TestAgentPrefixMergesIntoBareModelPresentation(t *testing.T) {
	// Prefixed wrapper routes collapse into the bare model: one picker entry.
	list := PresentationNames([]string{"claude-opus-4.6", "ag/claude-opus-4.6", "antigravity/claude-opus-4.6", "cf/@cf/meta/llama-3.1-70b", "cloudflare-ai/@cf/meta/llama-3.1-70b"}, VariantOptions{})
	for _, want := range []string{"claude-opus-4.6", "llama-3.1-70b"} {
		if !slices.Contains(list, want) {
			t.Fatalf("presentation lost %q: %#v", want, list)
		}
	}
	for _, name := range list {
		if strings.Contains(name, "/") {
			t.Fatalf("slash name leaked into picker: %#v", list)
		}
	}
}
