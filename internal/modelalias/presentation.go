package modelalias

import (
	"sort"
	"strings"
)

// PresentationNames cleans a picker without changing catalogs, aliases or policy.
// A variant is grouped only when its base is also known.
func PresentationNames(models []string, options VariantOptions) []string {
	presentRaw := map[string]bool{}
	for _, raw := range models {
		presentRaw[strings.ToLower(BaseName(raw))] = true
	}
	families := map[string][]string{}
	for _, raw := range models {
		if IsMetaName(raw) {
			continue
		}
		base := strings.ToLower(BaseName(raw))
		var suffixes []string
		for cut := strings.LastIndex(base, "-"); cut > 0; cut = strings.LastIndex(base, "-") {
			if variantCategory(base[cut+1:]) == "" {
				break
			}
			suffixes = append([]string{base[cut+1:]}, suffixes...)
			base = base[:cut]
		}
		// Strip enabled parts of a chain without swallowing disabled categories.
		// A partial chain can group only into another actually advertised name.
		var grouped func(string, int, bool) bool
		grouped = func(prefix string, index int, removed bool) bool {
			if index == len(suffixes) {
				return removed && presentRaw[prefix]
			}
			suffix := suffixes[index]
			return (options.enabled(suffix) && grouped(prefix, index+1, true)) || grouped(prefix+"-"+suffix, index+1, removed)
		}
		if grouped(base, 0, false) {
			continue
		}
		name := DisplayName(raw)
		if IsThinking(raw) {
			name += "-thinking"
		}
		if name == "" || IsMetaName(name) {
			continue
		}
		_, guard := fuzzyNormalize(name)
		key := FamilyKey(name) + guard
		if IsThinking(name) {
			key += "-thinking"
		}
		families[key] = append(families[key], name)
	}
	names := []string{}
	present := map[string]bool{}
	for _, family := range families {
		name := PreferredDisplayName(family)
		if IsThinking(family[0]) {
			name += "-thinking"
		}
		names = append(names, name)
		present[name] = true
	}
	sort.Strings(names)
	visible := names[:0]
	for _, name := range names {
		// 0g is an explicit provider wrapper, not a 95% spelling match.
		if strings.HasPrefix(name, "0g-") && present[strings.TrimPrefix(name, "0g-")] {
			continue
		}
		visible = append(visible, name)
	}
	// Only isolated, mutually unique pairs merge. Never merge ambiguous clusters
	// transitively, or let a hidden intermediate spelling absorb another model.
	peers := make([][]int, len(visible))
	for i, name := range visible {
		for j := i + 1; j < len(visible); j++ {
			if _, ok := FuzzyMatch([]string{visible[j]}, name); ok {
				peers[i] = append(peers[i], j)
				peers[j] = append(peers[j], i)
			}
		}
	}
	out := []string{}
	for i, name := range visible {
		if len(peers[i]) == 1 {
			j := peers[i][0]
			if len(peers[j]) == 1 && preferredLess(visible[j], name) {
				continue
			}
		}
		out = append(out, name)
	}
	return out
}
