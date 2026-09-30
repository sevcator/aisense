package modelalias

import (
	"regexp"
	"sort"
	"strings"
)

// Built-in price-tier router names. A request for "best" walks the most
// premium models advertised across the enabled catalog; "shit" walks the
// cheapest ones. Like combos, the walk falls through to the next model as
// soon as no upstream can serve the current one. Unlike combos, the list is
// computed live from the catalog, so newly imported models join
// automatically.
const (
	TierBest = "best"
	TierShit = "shit"
)

// MaxTierCandidates bounds the fallback walk behind a tier name: premium and
// budget clusters are ranked tightly, so a handful of steps already covers
// every meaningful price class.
const MaxTierCandidates = 8

// IsTierName reports whether name is one of the built-in price-tier router
// names. Tier names are reserved: they cannot be used for combos and are
// resolved before any concrete model family lookup.
func IsTierName(name string) bool {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	return base == TierBest || base == TierShit
}

// TierName returns the normalized tier name ("best" or "shit") and whether
// the requested name is a tier at all.
func TierName(name string) (string, bool) {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	switch base {
	case TierBest, TierShit:
		return base, true
	}
	return "", false
}

// junkNames are placeholder model names: "free"-style aliases that stand for
// "whatever the provider feels like" rather than a concrete model. They are
// hidden from client-facing lists, never enter tier pools, and explicit
// requests answer 404. The set is deliberately narrow — a name only lands
// here when it cannot plausibly identify a real model.
var junkNames = map[string]bool{
	"free": true, "free-model": true, "free-tier": true, "freetier": true,
	"unknown": true, "placeholder": true, "undefined": true,
	"sample": true, "demo": true, "dummy": true, "test": true, "sandbox": true,
}

// IsJunkName reports whether name is a placeholder model name that should be
// removed from client-facing listings and tier pools. Besides the exact
// placeholder set, provider router names ("router", "router-v1",
// "routerv2", ...) count as junk: a router stands for an unspecified
// backend model.
func IsJunkName(name string) bool {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	if base == "" {
		return false
	}
	if junkNames[base] {
		return true
	}
	for _, segment := range strings.Split(base, "-") {
		if segment == "router" {
			return true
		}
		if strings.HasPrefix(segment, "routerv") && isDigits(segment[len("routerv"):]) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !digit(s[i]) {
			return false
		}
	}
	return true
}

// nonChatMarkers are substrings that identify non-chat modalities. Models
// carrying them (embeddings, image/video/audio generation, speech) do not
// belong in a chat-completions model list and never join price tiers.
var nonChatMarkers = []string{
	"embed", "rerank", "re-rank",
	"image", "img2", "video", "diffusion", "sdxl", "flux",
	"dall-e", "dalle", "photo", "tts", "asr", "speech", "voice",
	"audio", "whisper", "transcri", "moderat", "reranker",
}

// IsChatModel reports whether name looks like a chat/completion model rather
// than an embedding, image, video or audio model.
func IsChatModel(name string) bool {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	for _, marker := range nonChatMarkers {
		if strings.Contains(base, marker) {
			return false
		}
	}
	return true
}

// SelectableModel reports whether name may be shown to chat clients and used
// by the tier router: a real (non-meta, non-junk) chat model.
func SelectableModel(name string) bool {
	base := strings.ToLower(strings.TrimSpace(BaseName(name)))
	return base != "" && base != "*" && !IsMetaName(base) && !IsJunkName(base) && IsChatModel(base)
}

// ---------------------------------------------------------------------------
// price-tier scoring
//
// There is no pricing feed for the exotic providers aisense aggregates, so
// tiers rank by a transparent heuristic: newer versions and premium edition
// words score high; flash/lite/mini classes, open-weights parameter counts
// and budget vendors score low. The order only has to be right at the top
// (most expensive) and the bottom (cheapest), which is where tiers read from.

var (
	versionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)
	paramRe   = regexp.MustCompile(`(?:^|[-.])(\d+(?:\.\d+)?)b(?:$|[-.])`)
	moeRe     = regexp.MustCompile(`(?:^|-)(a\d+b)(?:$|-)`)
)

// premiumSegment scores a single premium edition word.
var premiumSegments = map[string]float64{
	"opus": 8, "ultra": 6, "pro": 4, "max": 4,
	"thinking": 2, "reasoning": 1, "plus": 1, "preview": 1,
}

// cheapClasses are mutually exclusive edition classes, most cheap first.
var cheapClasses = []struct {
	segments []string
	score    float64
}{
	{[]string{"flash", "lite"}, -9}, // flash-lite: the floor of every catalog
	{[]string{"tiny"}, -7},
	{[]string{"nano"}, -6}, {[]string{"micro"}, -6},
	{[]string{"flash"}, -6}, {[]string{"highspeed"}, -6},
	{[]string{"air"}, -5}, {[]string{"flashx"}, -5}, {[]string{"fast"}, -5},
	{[]string{"lite"}, -5}, {[]string{"instant"}, -4},
	{[]string{"mini"}, -4}, {[]string{"turbo"}, -4},
	{[]string{"exp"}, -2},
}

// vendorPriors adjust whole families whose naming conventions are not
// comparable across vendors (an open-weights "6.8" is not a frontier "5.3").
var vendorPriors = []struct {
	prefix string
	score  float64
}{
	{"gpt", 3}, {"claude", 3}, {"gemini", 2}, {"grok", 2}, {"kimi", 1},
	{"sensenova", -5}, {"gemma", -3}, {"lfm", -4}, {"ornit", -4},
}

// preReleaseSegments are edition words that mark an unfinished release.
var preReleaseSegments = map[string]bool{"alpha": true, "beta": true, "rc": true}

// TierRank scores one model name; higher means more premium/expensive.
func TierRank(model string) float64 {
	base := strings.ToLower(strings.TrimSpace(BaseName(model)))
	score := 10.0
	if match := versionRe.FindStringSubmatch(base); match != nil {
		score += versionScore(match[1], match[2])
	}
	segments := strings.Split(base, "-")
	segmentSet := map[string]bool{}
	for _, segment := range segments {
		segmentSet[segment] = true
		score += premiumSegments[segment]
		if preReleaseSegments[segment] {
			score--
		}
	}
	for _, class := range cheapClasses {
		matched := true
		for _, segment := range class.segments {
			if !segmentSet[segment] {
				matched = false
				break
			}
		}
		if matched {
			score += class.score
			for _, segment := range class.segments {
				delete(segmentSet, segment)
			}
			break
		}
	}
	if paramRe.MatchString(base) {
		score -= 5 // explicit open-weights parameter count
	}
	if moeRe.MatchString(base) {
		score -= 5 // small activated-parameter MoE suffix (a3b/a4b)
	}
	for _, prior := range vendorPriors {
		if strings.HasPrefix(base, prior.prefix) {
			score += prior.score
			break
		}
	}
	return score
}

func versionScore(major, minor string) float64 {
	value := 0.0
	for i := 0; i < len(major); i++ {
		value = value*10 + float64(major[i]-'0')
	}
	value *= 2
	if minor != "" {
		value += float64(minor[0]-'0') * 0.5
	}
	if value > 12 {
		value = 12 // version schemes are not comparable; clamp the contribution
	}
	return value
}

// TierCandidates ranks every selectable chat model and returns the ordered
// walk for a tier: most premium first for "best", cheapest first for "shit".
// Duplicate families collapse to one entry, and the walk is capped.
func TierCandidates(models []string, tier string) []string {
	desc := tier == TierBest
	type entry struct {
		name  string
		score float64
	}
	seen := map[string]bool{}
	var entries []entry
	for _, model := range models {
		if !SelectableModel(model) {
			continue
		}
		name := DisplayName(model)
		if !SelectableModel(name) {
			continue
		}
		key := FamilyKey(name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, entry{name: name, score: TierRank(name)})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			if desc {
				return entries[i].score > entries[j].score
			}
			return entries[i].score < entries[j].score
		}
		return entries[i].name < entries[j].name
	})
	if len(entries) > MaxTierCandidates {
		entries = entries[:MaxTierCandidates]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}
