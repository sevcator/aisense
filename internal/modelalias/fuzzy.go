package modelalias

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

// normalizedNames remembers the normalized form of every model name seen.
// Listing models compares each name with every other one, so without this the
// same handful of names is re-normalized thousands of times per request.
var (
	normalizedNames sync.Map // name -> fuzzyForm
	normalizedCount atomic.Int64
)

// The cache holds names, which are a small stable set in practice; the cap
// keeps a client that asks for endless made-up models from growing it.
const maxNormalizedNames = 8192

type fuzzyForm struct {
	runes []rune // read-only: callers must not write through it
	guard string
}

func fuzzyNormalize(name string) ([]rune, string) {
	if cached, ok := normalizedNames.Load(name); ok {
		form := cached.(fuzzyForm)
		return form.runes, form.guard
	}
	runes, guard := buildFuzzyForm(name)
	if normalizedCount.Load() < maxNormalizedNames {
		if _, loaded := normalizedNames.LoadOrStore(name, fuzzyForm{runes: runes, guard: guard}); !loaded {
			normalizedCount.Add(1)
		}
	}
	return runes, guard
}

func buildFuzzyForm(name string) ([]rune, string) {
	name = strings.ToLower(BaseName(name))
	parts := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	var normalized, protected strings.Builder
	firstWord := true
	for _, part := range parts {
		runes := []rune(part)
		for start := 0; start < len(runes); {
			end := start + 1
			for end < len(runes) && unicode.IsNumber(runes[end]) == unicode.IsNumber(runes[start]) {
				end++
			}
			token := string(runes[start:end])
			numeric := unicode.IsNumber(runes[start])
			// Even a leading size/type/reasoning word is not typo-correctable.
			semantic := variantSuffixWords[token] || shorthandModifiers[token] || strings.Contains("|large|base|reasoning|codex|coding|image|audio|video|vision|text|chat|embedding|embeddings|instruct|instant|turbo|realtime|tts|b|", "|"+token+"|")
			if numeric || !firstWord || semantic {
				protected.WriteString("|" + token)
			}
			if !numeric {
				firstWord = false
			}
			normalized.WriteString(token)
			normalized.WriteByte('|')
			start = end
		}
	}
	return []rune(strings.TrimSuffix(normalized.String(), "|")), protected.String()
}

// FuzzyMatch selects a unique best family at >=95% normalized Levenshtein
// similarity. Routing callers must try known aliases first; presentation may
// use it without changing the underlying catalog or policy matching.
// Numeric runs and all words except the leading model name are immutable.
// This deliberately rejects suffix typos as well as variant substitutions.
func FuzzyMatch(names []string, requested string) (string, bool) {
	a, guard := fuzzyNormalize(requested)
	if len(a) == 0 || IsMetaName(requested) {
		return "", false
	}
	best, bestDistance, bestLength, tied := "", 1, 1, false
	for _, name := range names {
		if IsMetaName(name) {
			continue
		}
		b, targetGuard := fuzzyNormalize(name)
		length := max(len(a), len(b))
		if guard != targetGuard || 20*abs(len(a)-len(b)) > length {
			continue
		}
		row := make([]int, len(b)+1)
		for j := range row {
			row[j] = j
		}
		for i, ar := range a {
			previous := row[0]
			row[0] = i + 1
			for j, br := range b {
				old := row[j+1]
				cost := 0
				if ar != br {
					cost = 1
				}
				row[j+1] = min(row[j]+1, old+1, previous+cost)
				previous = old
			}
		}
		distance := row[len(b)]
		if 20*distance > length {
			continue
		}
		comparison := distance*bestLength - bestDistance*length
		if best == "" || comparison < 0 {
			best, bestDistance, bestLength, tied = name, distance, length, false
		} else if comparison == 0 && FamilyKey(name) != FamilyKey(best) {
			tied = true
		}
	}
	return best, best != "" && !tied
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
