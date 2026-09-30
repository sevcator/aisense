package modelalias

import (
	"sort"
	"strings"
	"unicode"
)

const MaxCandidates = 32

// metaNames are placeholder/router model names that must never be advertised
// in client-facing model lists. They are not real models: "*" is the wildcard
// marker, "auto"/"default"/... are provider-side router aliases. The check is
// applied to the base name so provider prefixes (auto/best-chat) are never
// mistaken for the meta provider itself.
var metaNames = map[string]bool{
	"*": true, "auto": true, "auto-beta": true, "auto-router": true,
	"router": true, "default": true, "none": true, "null": true,
	"any": true, "latest": true, "base": true, "standard": true,
}

// IsMetaName reports whether name is a meta/placeholder model name that should
// be removed from model lists. The wildcard "*" keeps its routing semantics
// elsewhere; it is only hidden from listings.
func IsMetaName(name string) bool {
	return metaNames[strings.ToLower(BaseName(name))]
}

// variantSuffixWords are trailing "-word" suffixes that mark a specialized
// variant of a base model (size class, reasoning effort, or misc tiers).
// "thinking" is included so chained forms like "-thinking-low" strip to the
// root base. "fast" is handled separately: it only counts as a variant when
// the operator enables fast mode.
var variantSuffixWords = map[string]bool{
	"small": true, "mini": true, "nano": true, "lite": true,
	"tiny": true, "micro": true,
	"minimal": true, "low": true, "medium": true, "high": true,
	"xhigh": true, "max": true,
	"extra": true, "extended": true, "thinking": true,
}

// SplitVariant strips recognized trailing variant suffixes from a model name
// and returns the remaining base plus the stripped suffixes (in original
// order). The name is never stripped down to nothing: a bare "fast" or "low"
// is a real model name, not a variant. includeFast controls whether "-fast"
// counts as a variant suffix.
func SplitVariant(name string, includeFast bool) (string, []string) {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	parts := strings.Split(base, "-")
	var suffixes []string
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		if !variantSuffixWords[last] && !(includeFast && last == "fast") {
			break
		}
		suffixes = append([]string{last}, suffixes...)
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "-"), suffixes
}

// IsVariant reports whether name ends with a recognized variant suffix.
func IsVariant(name string, includeFast bool) bool {
	_, suffixes := SplitVariant(name, includeFast)
	return len(suffixes) > 0
}

// effortSuffixes maps a client reasoning effort to the ordered list of
// matching variant suffixes. Unknown/empty efforts map to nothing. The
// request-side preference order (including fast and other categories) is
// built by VariantOptions.Suffixes from these chains.
var effortSuffixes = map[string][]string{
	"minimal": {"minimal"},
	"low":     {"low"},
	"medium":  {"medium"},
	"high":    {"high"},
	"xhigh":   {"max", "high"},
	"max":     {"max", "high"},
}

type Catalog struct {
	Models  []string
	Aliases map[string][]string
}

type synonymFamily struct {
	Canonical string
	Forms     []string
	Forced    []string
}

var synonymFamilies = []synonymFamily{
	{
		Canonical: "claude-opus-4.6",
		Forms:     []string{"claude-opus-4.6", "claude-4.6-opus", "opus4.6"},
		Forced: []string{
			"claude-opus-4.6-thinking",
			"claude-opus-4-6-thinking",
			"claude-opus-4.6",
			"claude-opus-4-6",
			"opus4.6",
			"claude-4.6-opus",
		},
	},
}

func BaseName(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "/"); i >= 0 && i < len(model)-1 {
		model = model[i+1:]
	}
	return strings.TrimSpace(model)
}

// agentPrefixes mark account-bound agent routes (Antigravity and similar).
// "ag/claude-opus-4.6" is served by a different backend than
// "claude-opus-4.6": it must stay a distinct visible model, never merge into
// the generic family, and keep its prefix in listings and routing.
var agentPrefixes = []string{"ag/", "antigravity/"}

// AgentPrefix returns the lowercase agent-route prefix of model ("ag/",
// "antigravity/") or "" when the name carries no such prefix.
func AgentPrefix(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range agentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return prefix
		}
	}
	return ""
}

func stripThinking(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{"-thinking", "_thinking", ".thinking", ":thinking", " thinking"} {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimSpace(name[:len(name)-len(suffix)])
		}
	}
	return strings.TrimSpace(name)
}

func IsThinking(model string) bool {
	base := strings.TrimSpace(BaseName(model))
	return stripThinking(base) != base
}

type modelTokens struct {
	words   []string
	digits  string
	wordSet map[string]int
}

func tokenize(model string) modelTokens {
	base := strings.ToLower(stripThinking(BaseName(model)))
	var words []string
	var word strings.Builder
	var digits strings.Builder
	flushWord := func() {
		if word.Len() == 0 {
			return
		}
		words = append(words, word.String())
		word.Reset()
	}
	for _, r := range base {
		switch {
		case unicode.IsLetter(r):
			word.WriteRune(r)
		case unicode.IsNumber(r):
			flushWord()
			digits.WriteRune(r)
		default:
			flushWord()
		}
	}
	flushWord()
	sort.Strings(words)
	set := make(map[string]int, len(words))
	for _, value := range words {
		set[value]++
	}
	return modelTokens{words: words, digits: digits.String(), wordSet: set}
}

func basicFamilyKey(model string) string {
	return familyKeyMemo.get(model, buildBasicFamilyKey)
}

func buildBasicFamilyKey(model string) string {
	tokens := tokenize(model)
	if len(tokens.words) == 0 && tokens.digits == "" {
		return ""
	}
	return strings.Join(tokens.words, "") + tokens.digits
}

// synonymByKey indexes every spelling of a family once, at start-up. FamilyKey
// runs on every request and hundreds of times per save, so it must not re-scan
// and re-tokenize the whole table on each call.
type synonymEntry struct {
	family synonymFamily
	key    string // basicFamilyKey of the family's canonical name
}

var synonymByKey = func() map[string]synonymEntry {
	out := map[string]synonymEntry{}
	for _, family := range synonymFamilies {
		entry := synonymEntry{family: family, key: basicFamilyKey(family.Canonical)}
		for _, form := range family.Forms {
			out[basicFamilyKey(form)] = entry
		}
	}
	return out
}()

func synonymForKey(key string) (synonymFamily, bool) {
	entry, ok := synonymByKey[key]
	return entry.family, ok
}

// FamilyKey is the routing identity shared by discovery, lists, policy, and
// forwarding. Provider paths, case, thinking suffixes, and punctuation do not
// affect the family. Agent route prefixes (ag/, antigravity/) are wrappers:
// the model name is the last path segment, and prefixed raw routes simply
// become additional routes of the same family (the proxy sanitizes them).
func FamilyKey(model string) string {
	key := basicFamilyKey(model)
	if entry, ok := synonymByKey[key]; ok {
		return entry.key
	}
	return key
}

func digit(c byte) bool { return c >= '0' && c <= '9' }

// DisplayName creates a stable client-facing spelling while FamilyKey remains
// deliberately more permissive for routing. The client-facing name is the
// last path segment: however many slashes a raw route has, the model is
// advertised under its bare name.
func DisplayName(model string) string {
	return displayNameMemo.get(model, buildDisplayName)
}

func buildDisplayName(model string) string {
	name := strings.ToLower(stripThinking(BaseName(model)))
	var normalized strings.Builder
	lastDash := false
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '.' && r != '-' {
			if !lastDash {
				normalized.WriteByte('-')
				lastDash = true
			}
			continue
		}
		normalized.WriteRune(r)
		lastDash = r == '-'
	}
	name = strings.Trim(normalized.String(), "-")
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '-' && i > 0 && i+1 < len(name) &&
			digit(name[i-1]) && digit(name[i+1]) &&
			(i < 2 || !digit(name[i-2])) &&
			(i+2 >= len(name) || !digit(name[i+2])) {
			out.WriteByte('.')
			continue
		}
		out.WriteByte(name[i])
	}
	display := out.String()
	if family, ok := synonymForKey(basicFamilyKey(display)); ok {
		return family.Canonical
	}
	return display
}

// boundaryDashCount counts dashes sitting at a letter↔digit boundary
// (gpt-5, claude-opus-4.6). Vendor-canonical spellings put a dash between the
// family word and the version number, so a higher count means a nicer
// client-facing spelling (gpt-5.6-sol beats gpt5.6-sol).
func boundaryDashCount(name string) int {
	n := 0
	for i := 1; i+1 < len(name); i++ {
		if name[i] != '-' {
			continue
		}
		a, b := name[i-1], name[i+1]
		aDigit, bDigit := digit(a), digit(b)
		aLetter := (a >= 'a' && a <= 'z') || (a >= 'A' && a <= 'Z')
		bLetter := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
		if (aLetter && bDigit) || (aDigit && bLetter) {
			n++
		}
	}
	return n
}

// PreferredDisplayName picks the best client-facing spelling among equivalent
// forms of one model family. Boundary-dashed vendor spellings win
// (gpt-5.6-sol over gpt5.6-sol); ties break on length then alphabetically.
func PreferredDisplayName(names []string) string {
	best := ""
	for _, n := range names {
		d := DisplayName(n)
		if d == "" || d == "*" || IsMetaName(d) {
			continue
		}
		if best == "" || preferredLess(d, best) {
			best = d
		}
	}
	return best
}

func preferredLess(a, b string) bool {
	ca, cb := boundaryDashCount(a), boundaryDashCount(b)
	if ca != cb {
		return ca > cb
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var shorthandModifiers = map[string]bool{
	"batch": true, "beta": true, "fast": true, "flash": true,
	"high": true, "latest": true, "low": true, "max": true,
	"medium": true, "mini": true, "preview": true, "pro": true,
}

func isStrictWordSubset(shorter, longer modelTokens) bool {
	if shorter.digits == "" || shorter.digits != longer.digits || len(shorter.words) == 0 || len(shorter.words)+1 != len(longer.words) {
		return false
	}
	for word, count := range shorter.wordSet {
		if longer.wordSet[word] < count {
			return false
		}
	}
	for word, count := range longer.wordSet {
		if count > shorter.wordSet[word] && (shorthandModifiers[word] || variantCategory(word) != "") {
			return false
		}
	}
	return true
}

// mergeUnambiguousShorthands joins a shortened family only when exactly one
// fuller family in this concrete catalog can own it. This keeps broad alias
// inference out of global policy checks where ambiguity cannot be measured.
func mergeUnambiguousShorthands(routes map[string][]string) {
	tokens := make(map[string]modelTokens, len(routes))
	for key, values := range routes {
		if len(values) > 0 {
			tokens[key] = tokenize(values[0])
		}
	}
	merges := map[string]string{}
	for shortKey, shortTokens := range tokens {
		matches := make([]string, 0, 2)
		for fullKey, fullTokens := range tokens {
			if shortKey != fullKey && isStrictWordSubset(shortTokens, fullTokens) {
				matches = append(matches, fullKey)
			}
		}
		if len(matches) == 1 {
			merges[shortKey] = matches[0]
		}
	}
	for shortKey, fullKey := range merges {
		if _, targetAlsoMerges := merges[fullKey]; targetAlsoMerges {
			continue
		}
		routes[fullKey] = append(routes[fullKey], routes[shortKey]...)
		delete(routes, shortKey)
	}
}

// Build groups raw routes into one canonical model per family. Existing alias
// values are authoritative; a canonical model is used as its own raw route
// only when no alias group exists for that family.
func Build(models []string, existing map[string][]string) Catalog {
	routes := map[string][]string{}
	seenRoutes := map[string]map[string]bool{}
	existingFamilies := map[string]bool{}
	explicitCanonical := map[string]string{}
	claimedRawFamilies := map[string]string{}
	addToFamily := func(key, raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "*" {
			return
		}
		if key == "" {
			return
		}
		if seenRoutes[key] == nil {
			seenRoutes[key] = map[string]bool{}
		}
		if seenRoutes[key][raw] {
			return
		}
		seenRoutes[key][raw] = true
		routes[key] = append(routes[key], raw)
	}
	add := func(raw string) {
		addToFamily(FamilyKey(raw), raw)
	}

	aliasKeys := make([]string, 0, len(existing))
	filterExisting := len(models) > 0 && !containsWildcard(models)
	allowedFamilies := map[string]bool{}
	for _, model := range models {
		if model != "*" && !IsMetaName(model) {
			allowedFamilies[FamilyKey(model)] = true
		}
	}
	for canonical := range existing {
		if IsMetaName(canonical) {
			continue // meta placeholder names are removed entirely
		}
		aliasKeys = append(aliasKeys, canonical)
	}
	sort.Strings(aliasKeys)
	for _, canonical := range aliasKeys {
		key := FamilyKey(canonical)
		if filterExisting && !allowedFamilies[key] {
			continue
		}
		explicitCanonical[key] = strings.TrimSpace(canonical)
		values := existing[canonical]
		if len(values) == 0 {
			addToFamily(key, canonical)
			existingFamilies[key] = true
			continue
		}
		for _, raw := range values {
			addToFamily(key, raw)
			claimedRawFamilies[FamilyKey(raw)] = key
		}
		existingFamilies[key] = true
	}
	for _, model := range models {
		if strings.TrimSpace(model) == "*" || IsMetaName(model) {
			continue
		}
		key := FamilyKey(model)
		if owner := claimedRawFamilies[key]; owner != "" && owner != key {
			continue
		}
		if !existingFamilies[key] {
			add(model)
		}
	}
	mergeUnambiguousShorthands(routes)

	canonicalByKey := map[string]string{}
	for key, values := range routes {
		if family, ok := synonymForKey(key); ok {
			canonicalByKey[key] = family.Canonical
			continue
		}
		if canonical := explicitCanonical[key]; canonical != "" {
			canonicalByKey[key] = DisplayName(canonical)
			continue
		}
		matching := make([]string, 0, len(values))
		for _, raw := range values {
			if FamilyKey(raw) == key {
				matching = append(matching, raw)
			}
		}
		if len(matching) == 0 {
			matching = values
		}
		canonicalByKey[key] = PreferredDisplayName(matching)
	}
	result := Catalog{Models: make([]string, 0, len(routes)), Aliases: map[string][]string{}}
	for key, canonical := range canonicalByKey {
		result.Models = append(result.Models, canonical)
		// A model that routes to itself needs no persisted alias entry.
		if len(routes[key]) != 1 || routes[key][0] != canonical {
			result.Aliases[canonical] = append([]string(nil), routes[key]...)
		}
	}
	sort.Strings(result.Models)
	if containsWildcard(models) {
		result.Models = append([]string{"*"}, result.Models...)
	}
	return result
}

func containsWildcard(models []string) bool {
	for _, model := range models {
		if strings.TrimSpace(model) == "*" {
			return true
		}
	}
	return false
}

func Canonical(models []string, requested string) (string, bool) {
	key := FamilyKey(requested)
	if key == "" {
		return "", false
	}
	for _, model := range models {
		if model != "*" && FamilyKey(model) == key {
			return model, true
		}
	}
	return "", false
}

func Matches(a, b string) bool {
	ka, kb := FamilyKey(a), FamilyKey(b)
	return ka != "" && ka == kb
}

func canonicalForCatalog(models []string, aliases map[string][]string, requested string) (string, bool) {
	if canonical, ok := Canonical(models, requested); ok {
		return canonical, true
	}
	requestedKey := FamilyKey(requested)
	for _, canonical := range models {
		for _, raw := range aliases[canonical] {
			if FamilyKey(raw) == requestedKey {
				return canonical, true
			}
		}
	}
	return "", false
}

type rankedRoute struct {
	raw   string
	score [5]int
}

func numericHyphenName(name string) string {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '.' && i > 0 && i+1 < len(name) && digit(name[i-1]) && digit(name[i+1]) {
			out.WriteByte('-')
			continue
		}
		out.WriteByte(name[i])
	}
	return out.String()
}

func appendUnique(out []string, seen map[string]bool, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || seen[value] || len(out) == MaxCandidates {
		return out
	}
	seen[value] = true
	return append(out, value)
}

func synthesizedCandidates(canonical string, routes []string, preferThinking bool) []string {
	if family, ok := synonymForKey(FamilyKey(canonical)); ok {
		out := make([]string, 0, len(family.Forced))
		for _, candidate := range family.Forced {
			if IsThinking(candidate) == preferThinking {
				out = append(out, candidate)
			}
		}
		return out
	}

	hasThinking := false
	hasBare := false
	for _, route := range routes {
		if IsThinking(route) {
			hasThinking = true
		}
		if !strings.Contains(strings.TrimSpace(route), "/") {
			hasBare = true
		}
	}
	if !hasBare {
		return nil
	}
	base := DisplayName(canonical)
	hyphen := numericHyphenName(base)
	if preferThinking && hasThinking {
		out := []string{base + "-thinking"}
		if hyphen != base {
			out = append(out, hyphen+"-thinking")
		}
		return out
	}
	if preferThinking {
		return nil
	}
	out := []string{base}
	if hyphen != base {
		out = append(out, hyphen)
	}
	return out
}

// ForcedCandidates returns explicit provider-independent spellings that must
// lead the complete retry sequence for a known semantic model family.
func ForcedCandidates(model string) []string {
	if family, ok := synonymForKey(FamilyKey(model)); ok {
		return append([]string(nil), family.Forced...)
	}
	return nil
}

// Normalize returns the catalog unchanged when it is already canonical, and
// builds it otherwise. Saving a config runs this for every upstream, so the
// common case must not rebuild anything.
func Normalize(models []string, aliases map[string][]string) ([]string, map[string][]string) {
	models, aliases = normalizedCatalog(models, aliases)
	for canonical, routes := range aliases {
		if len(routes) == 1 && routes[0] == canonical {
			delete(aliases, canonical)
		}
	}
	return models, aliases
}

func normalizedCatalog(models []string, aliases map[string][]string) ([]string, map[string][]string) {
	normalized := aliases != nil
	if normalized {
		for canonical := range aliases {
			found := false
			for _, model := range models {
				if model == canonical {
					found = true
					break
				}
			}
			if !found {
				normalized = false
				break
			}
		}
		for _, model := range models {
			if model != "*" && IsMetaName(model) {
				normalized = false
				break
			}
			if len(aliases[model]) == 0 {
				for canonical, routes := range aliases {
					if canonical != model {
						for _, raw := range routes {
							if raw == model {
								normalized = false
								break
							}
						}
					}
				}
			}
		}
	}
	if normalized {
		return models, aliases
	}
	catalog := Build(models, aliases)
	return catalog.Models, catalog.Aliases
}

// SupportsThinking reports whether the resolved catalog family advertises at
// least one thinking route. Explicit forced families are handled separately.
func SupportsThinking(models []string, aliases map[string][]string, requested string) bool {
	models, aliases = normalizedCatalog(models, aliases)
	canonical, ok := canonicalForCatalog(models, aliases, requested)
	if !ok {
		return false
	}
	for _, route := range aliases[canonical] {
		if IsThinking(route) {
			return true
		}
	}
	return false
}

// Candidates returns one thinking/plain class at a time so the proxy can
// retain its existing two-pass behavior.
func Candidates(models []string, aliases map[string][]string, requested string, preferThinking bool) []string {
	models, aliases = normalizedCatalog(models, aliases)
	canonical, ok := canonicalForCatalog(models, aliases, requested)
	if !ok {
		if containsWildcard(models) {
			return []string{BaseName(requested)}
		}
		return nil
	}
	routes := append([]string(nil), aliases[canonical]...)
	if len(routes) == 0 {
		routes = []string{canonical}
	}
	generated := synthesizedCandidates(canonical, routes, preferThinking)
	preferred := make([]string, 0, len(routes))
	fallback := make([]string, 0, len(routes))
	for _, raw := range routes {
		if IsThinking(raw) == preferThinking {
			preferred = append(preferred, raw)
		} else {
			fallback = append(fallback, raw)
		}
	}
	if len(preferred) == 0 {
		preferred = fallback
	}
	requestedBase := strings.ToLower(BaseName(requested))
	requestedDisplay := DisplayName(requested)
	ranked := make([]rankedRoute, 0, len(preferred))
	for i, raw := range preferred {
		ranked = append(ranked, rankedRoute{raw: raw, score: [5]int{
			boolInt(!strings.EqualFold(BaseName(raw), requestedBase)),
			boolInt(DisplayName(raw) != requestedDisplay),
			strings.Count(raw, "/"),
			len(raw),
			i,
		}})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		for n := 0; n < len(ranked[i].score); n++ {
			if ranked[i].score[n] != ranked[j].score[n] {
				return ranked[i].score[n] < ranked[j].score[n]
			}
		}
		return strings.ToLower(ranked[i].raw) < strings.ToLower(ranked[j].raw)
	})
	out := make([]string, 0, min(len(generated)+len(ranked), MaxCandidates))
	seen := map[string]bool{}
	// Older catalogs may have absorbed a base into its free/exp/turbo family.
	// An explicitly advertised identity must not inherit that family's default.
	if !Matches(canonical, requested) && variantRoot(canonical) == variantRoot(requested) {
		for _, route := range ranked {
			if Matches(route.raw, requested) {
				out = appendUnique(out, seen, route.raw)
			}
		}
	}
	for _, route := range generated {
		out = appendUnique(out, seen, route)
	}
	for _, route := range ranked {
		out = appendUnique(out, seen, route.raw)
	}
	return out
}

// effortWords are the variant suffixes that express a reasoning effort level.
var effortWords = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true,
	"xhigh": true, "max": true,
}

// BoostedVariantCandidates returns only advertised routes in preference order.
// Explicit preference variants are preserved; size variants can accept boosts.
func BoostedVariantCandidates(models []string, aliases map[string][]string, requested string, preferThinking bool, boost []string) []string {
	if len(boost) == 0 {
		return nil
	}
	base := strings.TrimSpace(BaseName(requested))
	if cut := strings.LastIndex(base, "-"); cut > 0 && variantCategory(strings.ToLower(base[cut+1:])) != "" {
		return nil // Honor explicit variants rather than silently changing them.
	}
	models, aliases = normalizedCatalog(models, aliases)
	var out []string
	seen := map[string]bool{}
	for _, suffix := range boost {
		variant := base + "-" + suffix
		// Only boost onto variants that actually exist in this catalog;
		// wildcard passthrough must not fabricate variant routes.
		if _, ok := canonicalForCatalog(models, aliases, variant); !ok {
			continue
		}
		canonical, _ := canonicalForCatalog(models, aliases, variant)
		routes := aliases[canonical]
		if len(routes) == 0 {
			routes = []string{canonical}
		}
		for _, route := range routes {
			if Matches(route, variant) {
				out = appendUnique(out, seen, route)
			}
		}
	}
	return out
}

// CandidatesBoost is Candidates with variant-aware boosting: when the client
// signals a reasoning effort (or fast mode), routes of the matching
// specialized variant family (e.g. gpt-5.6-sol-low) lead the list, followed
// by the regular family routes as fallback.
func CandidatesBoost(models []string, aliases map[string][]string, requested string, preferThinking bool, boost []string) []string {
	base := Candidates(models, aliases, requested, preferThinking)
	variants := BoostedVariantCandidates(models, aliases, requested, preferThinking, boost)
	if len(variants) == 0 {
		return base
	}
	out := make([]string, 0, len(variants)+len(base))
	seen := map[string]bool{}
	for _, route := range variants {
		out = appendUnique(out, seen, route)
	}
	for _, route := range base {
		out = appendUnique(out, seen, route)
	}
	return out
}

// RetryableError recognizes failures where changing the raw model/provider
// route can change the outcome. Generic endpoint, authentication, network,
// and server errors intentionally remain upstream-level failures.
func RetryableError(status int, body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"model_not_found", "unknown model", "unsupported model",
		"model unavailable", "model does not exist", "model is not available",
		"no longer available", "end of life", "model catalog", "decommissioned",
		"deprecated", "model retired", "model is retired", "model has been retired",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	modelMention := strings.Contains(lower, "model")
	if modelMention && (strings.Contains(lower, "not found") || strings.Contains(lower, "not available") ||
		strings.Contains(lower, "no longer available") || strings.Contains(lower, "end of life") ||
		strings.Contains(lower, "does not exist") || strings.Contains(lower, "unsupported") ||
		strings.Contains(lower, "catalog") || strings.Contains(lower, "deprecated") ||
		strings.Contains(lower, "decommissioned") || strings.Contains(lower, "retired") ||
		strings.Contains(lower, "gone")) {
		return true
	}
	providerMention := strings.Contains(lower, "provider")
	if providerMention && (strings.Contains(lower, "credential") || strings.Contains(lower, "not configured") ||
		strings.Contains(lower, "not available") || strings.Contains(lower, "unavailable") ||
		strings.Contains(lower, "no active") || strings.Contains(lower, "quota")) {
		return true
	}
	// Some OpenAI-compatible gateways use a generic 400/404/410/422/503 with a
	// concise "model ..." error that does not match the longer phrases above.
	// 503 is included because some providers (e.g. OpenRouter) signal model-
	// or provider-route unavailability via 503 rather than 404/422.
	// 410 is included for models that have reached End of Life / permanent removal.
	// hasErrField matches standard JSON errors ("error") and RFC 7807 Problem Details ("title", "detail").
	hasErrField := strings.Contains(lower, "error") || strings.Contains(lower, "detail") || strings.Contains(lower, "title")
	return (status == 400 || status == 404 || status == 410 || status == 422 || status == 503) && modelMention && hasErrField
}
