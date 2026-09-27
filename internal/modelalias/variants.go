package modelalias

import "strings"

type VariantOptions struct {
	Reasoning bool
	Other     bool
	Fast      bool
}

func variantRoot(name string) string {
	name = strings.ToLower(BaseName(name))
	for cut := strings.LastIndex(name, "-"); cut > 0; cut = strings.LastIndex(name, "-") {
		if variantCategory(name[cut+1:]) == "" {
			break
		}
		name = name[:cut]
	}
	return FamilyKey(name)
}

// ListingNames recovers advertised variant identities from persisted alias
// groups, including bases absorbed by older shorthand inference.
func ListingNames(models []string, aliases map[string][]string) []string {
	out := append([]string{}, models...)
	for _, model := range models {
		for _, raw := range aliases[model] {
			if variantRoot(raw) == variantRoot(model) {
				out = append(out, raw)
			}
		}
	}
	return out
}

// Size and modality suffixes are not preference variants.
func variantCategory(s string) string {
	if effortWords[s] || s == "thinking" || s == "extended" || s == "extra" {
		return "reasoning"
	}
	if s == "free" || s == "exp" || s == "turbo" {
		return "other"
	}
	if s == "fast" {
		return "fast"
	}
	return ""
}

func (o VariantOptions) enabled(s string) bool {
	switch variantCategory(s) {
	case "reasoning":
		return o.Reasoning
	case "other":
		return o.Other
	case "fast":
		return o.Fast
	}
	return false
}

// Suffixes ranks requested effort, then free/exp/turbo, then speed.
// Permutations are matched against advertised routes, never network-probed.
func (o VariantOptions) Suffixes(effort string) []string {
	var efforts []string
	if o.Reasoning {
		efforts = effortSuffixes[strings.ToLower(strings.TrimSpace(effort))]
	}
	others := []string{""}
	if o.Other {
		others = []string{"free", "exp", "turbo", ""}
	}
	var out []string
	var permutations func([]string, string)
	permutations = func(parts []string, prefix string) {
		if len(parts) == 0 {
			if prefix != "" {
				out = append(out, prefix)
			}
			return
		}
		for i, part := range parts {
			rest := append([]string{}, parts[:i]...)
			rest = append(rest, parts[i+1:]...)
			next := part
			if prefix != "" {
				next = prefix + "-" + part
			}
			permutations(rest, next)
		}
	}
	// Copy: efforts aliases a package-level map slice; appending to it in
	// place must never write back into that shared backing array.
	for _, effort := range append(append([]string(nil), efforts...), "") {
		for _, other := range others {
			parts := []string{}
			if effort != "" {
				parts = append(parts, effort)
			}
			if other != "" {
				parts = append(parts, other)
			}
			if o.Fast {
				permutations(append(append([]string{}, parts...), "fast"), "")
			}
			permutations(parts, "")
		}
	}
	return out
}
