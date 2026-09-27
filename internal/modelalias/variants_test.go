package modelalias

import (
	"fmt"
	"slices"
	"testing"
)

func TestVariantToggleMatrix(t *testing.T) {
	base := "deepseek-v4-flash"
	models := []string{base, "0g-" + base, base + "-low", base + "-high", base + "-free", base + "-exp", base + "-turbo", base + "-fast", base + "-low-free-fast", base + "-vision-exp", "orphan-free", base + "-mini"}
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			o := VariantOptions{Reasoning: mask&1 != 0, Other: mask&2 != 0, Fast: mask&4 != 0}
			list := PresentationNames(models, o)
			for suffix, hidden := range map[string]bool{"low": o.Reasoning, "high": o.Reasoning, "free": o.Other, "exp": o.Other, "turbo": o.Other, "fast": o.Fast} {
				if slices.Contains(list, base+"-"+suffix) == hidden {
					t.Fatalf("%s: %v", suffix, list)
				}
			}
			for _, name := range []string{base, base + "-vision-exp", "orphan-free", base + "-mini"} {
				if !slices.Contains(list, name) {
					t.Fatalf("lost %s: %v", name, list)
				}
			}
			if slices.Contains(list, "0g-"+base) || slices.Contains(list, base+"-vision") {
				t.Fatal(list)
			}
			partial := PresentationNames([]string{base, base + "-low", base + "-free", base + "-low-free", base + "-free-low"}, o)
			for _, chain := range []string{"low-free", "free-low"} {
				if slices.Contains(partial, base+"-"+chain) == (o.Reasoning || o.Other) {
					t.Fatalf("partial chain %s: %v", chain, partial)
				}
			}
			want := base
			if o.Reasoning {
				want += "-low"
			}
			if o.Other {
				want += "-free"
			}
			if o.Fast {
				want += "-fast"
			}
			catalog := append(append([]string{}, models...), want)
			routes := CandidatesBoost(catalog, nil, base, false, o.Suffixes("low"))
			if len(routes) == 0 || routes[0] != want {
				t.Fatalf("routes %v want %s", routes, want)
			}
			routes = CandidatesBoost(catalog, nil, base+"-high", false, o.Suffixes("low"))
			if routes[0] != base+"-high" {
				t.Fatal(routes)
			}
			for _, route := range BoostedVariantCandidates(catalog, nil, base, false, o.Suffixes("")) {
				if slices.Contains([]string{base + "-low", base + "-high", base + "-low-free-fast"}, route) {
					t.Fatalf("invented effort: %v", route)
				}
			}
		})
	}
	if list := PresentationNames([]string{base + "-vision", base + "-vision-exp"}, VariantOptions{Other: true}); !slices.Equal(list, []string{base + "-vision"}) {
		t.Fatal(list)
	}
}

func TestPersistedAbsorbedBase(t *testing.T) {
	base := "deepseek-v4-flash"
	catalog := Build([]string{base + "-free", "0g-" + base}, map[string][]string{base + "-free": {base + "-free", base}, "0g-" + base: {"0g-" + base}})
	for _, other := range []bool{false, true} {
		o := VariantOptions{Other: other}
		want := []string{base}
		if !other {
			want = append(want, base+"-free")
		}
		if got := PresentationNames(ListingNames(catalog.Models, catalog.Aliases), o); !slices.Equal(got, want) {
			t.Fatalf("list %v want %v", got, want)
		}
		routes := CandidatesBoost(catalog.Models, catalog.Aliases, base, false, o.Suffixes(""))
		first := base
		if other {
			first += "-free"
		}
		if routes[0] != first {
			t.Fatalf("routes %v want %s", routes, first)
		}
	}
}
