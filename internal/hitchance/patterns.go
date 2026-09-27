package hitchance

import (
	"regexp"
	"sync"
)

// compiled caches the rule patterns. Validate runs on every config save and
// Classify on every failed request, so the same handful of expressions would
// otherwise be re-compiled thousands of times.
var compiled sync.Map // pattern string -> *regexp.Regexp or error

type badPattern struct{ err error }

// compilePattern returns the compiled form of a rule pattern, remembering both
// the successes and the failures so neither is paid for twice.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if cached, ok := compiled.Load(pattern); ok {
		if bad, isBad := cached.(badPattern); isBad {
			return nil, bad.err
		}
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		compiled.Store(pattern, badPattern{err: err})
		return nil, err
	}
	compiled.Store(pattern, re)
	return re, nil
}
