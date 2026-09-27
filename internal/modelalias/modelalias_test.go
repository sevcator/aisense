package modelalias

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildCanonicalizesAndPreservesRawRoutes(t *testing.T) {
	raw := []string{
		"aug/glm-5.2", "opencode-zen/glm-5.2", "forge/glm-5.2", "oc/glm-5.2",
		"openadapter/free/glm-5-2", "oad/free/glm-5-2", "glm_5_2", "GLM-5.2-thinking",
		"horde/Kimi Coding", "provider/gpt-oss:20b",
	}
	catalog := Build(raw, nil)
	if canonical, ok := Canonical(catalog.Models, "glm-5.2"); !ok || canonical != "glm-5.2" {
		t.Fatalf("glm canonical = %q, %v; models=%#v", canonical, ok, catalog.Models)
	}
	aliases := catalog.Aliases["glm-5.2"]
	if len(aliases) != 8 {
		t.Fatalf("glm aliases = %#v", aliases)
	}
	for _, route := range raw[:8] {
		found := false
		for _, alias := range aliases {
			if alias == route {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("lost raw route %q in %#v", route, aliases)
		}
	}
	for _, want := range []string{"kimi-coding", "gpt-oss-20b"} {
		if _, ok := Canonical(catalog.Models, want); !ok {
			t.Fatalf("missing display model %q in %#v", want, catalog.Models)
		}
	}
}

func TestBuildCollapsesLargeCatalogWithoutLosingRoutes(t *testing.T) {
	raw := make([]string, 0, 2643)
	for i := 0; i < 1012; i++ {
		raw = append(raw, fmt.Sprintf("model-%d", i))
	}
	for i := 0; len(raw) < 2643; i++ {
		family := i % 1012
		raw = append(raw, fmt.Sprintf("provider-%d/model_%d", i, family))
	}
	catalog := Build(raw, nil)
	if len(catalog.Models) != 1012 {
		t.Fatalf("canonical models = %d, want 1012", len(catalog.Models))
	}
	routes := 0
	for _, aliases := range catalog.Aliases {
		routes += len(aliases)
	}
	if routes != 2643 {
		t.Fatalf("preserved routes = %d, want 2643", routes)
	}
}

func TestCandidatesPreferExactAndSeparateThinking(t *testing.T) {
	catalog := Build([]string{
		"aug/glm-5.2", "openadapter/free/glm-5-2", "glm-5.2", "forge/glm_5_2",
		"provider/glm-5.2-thinking",
	}, nil)
	plain := Candidates(catalog.Models, catalog.Aliases, "glm-5.2", false)
	if got := strings.Join(plain, ","); got != "glm-5.2,glm-5-2,aug/glm-5.2,forge/glm_5_2,openadapter/free/glm-5-2" {
		t.Fatalf("plain candidates = %#v", plain)
	}
	thinking := Candidates(catalog.Models, catalog.Aliases, "glm-5.2", true)
	if got := strings.Join(thinking, ","); got != "glm-5.2-thinking,glm-5-2-thinking,provider/glm-5.2-thinking" {
		t.Fatalf("thinking candidates = %#v", thinking)
	}
}

func TestSemanticFamiliesReorderAndMergeUnambiguousShorthands(t *testing.T) {
	if !Matches("claude-opus-4.6", "claude-4.6-opus") {
		t.Fatal("reordered tokens should share a family")
	}
	if !Matches("claude-opus-4.6", "opus4.6") {
		t.Fatal("known shorthand should share the Opus family")
	}

	catalog := Build([]string{"acme-foo-2", "foo2"}, nil)
	if len(catalog.Models) != 1 || catalog.Models[0] != "acme-foo-2" {
		t.Fatalf("unambiguous shorthand catalog = %#v", catalog.Models)
	}
	if got := Candidates(catalog.Models, catalog.Aliases, "foo2", false); len(got) == 0 || got[0] != "acme-foo-2" {
		t.Fatalf("shortened request candidates = %#v", got)
	}

	ambiguous := Build([]string{"acme-foo-2", "other-foo-2", "foo2"}, nil)
	if len(ambiguous.Models) != 3 {
		t.Fatalf("ambiguous shorthand was merged: %#v", ambiguous.Models)
	}
	modified := Build([]string{"claude-opus-4.6", "claude-opus-4.6-high"}, nil)
	if len(modified.Models) != 2 {
		t.Fatalf("quality modifier was treated as shorthand: %#v", modified.Models)
	}
}

func TestOpus46ForcedCandidateOrder(t *testing.T) {
	want := []string{
		"claude-opus-4.6-thinking",
		"claude-opus-4-6-thinking",
		"claude-opus-4.6",
		"claude-opus-4-6",
		"opus4.6",
		"claude-4.6-opus",
	}
	for _, requested := range []string{"claude-opus-4.6", "claude-4.6-opus", "opus4.6"} {
		if got := ForcedCandidates(requested); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("forced candidates for %q = %#v", requested, got)
		}
	}
	catalog := Build([]string{"opus4.6", "claude-4.6-opus", "provider/claude-opus-4-6-thinking"}, nil)
	if len(catalog.Models) != 1 || catalog.Models[0] != "claude-opus-4.6" {
		t.Fatalf("Opus catalog = %#v", catalog.Models)
	}
}

func TestConfiguredAliasesAreAuthoritativeAcrossDifferentNames(t *testing.T) {
	catalog := Build(
		[]string{"primary-model", "completely-different-route"},
		map[string][]string{"primary-model": {"provider/primary-model", "completely-different-route"}},
	)
	if len(catalog.Models) != 1 || catalog.Models[0] != "primary-model" {
		t.Fatalf("configured alias catalog = %#v", catalog.Models)
	}
	want := []string{"provider/primary-model", "completely-different-route"}
	if got := catalog.Aliases["primary-model"]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("configured routes = %#v", got)
	}
	got := Candidates(catalog.Models, catalog.Aliases, "completely-different-route", false)
	found := false
	for _, candidate := range got {
		found = found || candidate == "completely-different-route"
	}
	if len(got) == 0 || got[0] != "primary-model" || !found {
		t.Fatalf("configured alias request candidates = %#v", got)
	}
}

func TestCandidatesAreBounded(t *testing.T) {
	raw := make([]string, 40)
	for i := range raw {
		raw[i] = fmt.Sprintf("provider-%02d/glm-5.2", i)
	}
	catalog := Build(raw, nil)
	if got := len(Candidates(catalog.Models, catalog.Aliases, "glm_5_2", false)); got != MaxCandidates {
		t.Fatalf("candidate count = %d, want %d", got, MaxCandidates)
	}
}

func TestRetryableErrorOnlyMatchesModelOrProviderRoutingFailures(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"model_not_found"}}`,
		`{"error":{"message":"No active credentials for provider: forge"}}`,
		`{"error":{"message":"model glm-5.2 is not available"}}`,
	} {
		if !RetryableError(400, body) {
			t.Fatalf("expected retry for %s", body)
		}
	}
	for _, body := range []string{"invalid api key", "internal server error", "not found"} {
		if RetryableError(500, body) {
			t.Fatalf("unexpected retry for %q", body)
		}
	}
	// 503: model-specific body → should trigger alias retry
	if !RetryableError(503, `{"error":{"message":"model not available"}}`) {
		t.Fatal("503 with model error should be retryable")
	}
	// 503: generic overload with no "model" mention → should NOT trigger alias retry
	if RetryableError(503, `{"error":{"message":"service overloaded"}}`) {
		t.Fatal("503 generic overload should not be retryable")
	}
	// 503: provider + quota → retryable via existing provider phrase check
	if !RetryableError(503, `{"error":{"message":"provider quota exceeded"}}`) {
		t.Fatal("503 provider quota should be retryable")
	}
	// 410: exact NVIDIA NIM EOL payload with RFC 7807 problem details
	nvidiaEOL := `{"type":"about:blank","title":"Gone","status":410,"detail":"The model 'deepseek-ai/deepseek-v4-pro' has reached its end of life on 2026-08-07T09:00:00Z and is no longer available."}`
	if !RetryableError(410, nvidiaEOL) {
		t.Fatal("410 with NVIDIA EOL detail should be retryable")
	}
	// 410: deprecated / retired model error
	if !RetryableError(410, `{"error":{"message":"model claude-2 has been retired and is decommissioned"}}`) {
		t.Fatal("410 retired model should be retryable")
	}
	if !Matches("glm-5.2", "GLM_5_2") || !Matches("glm-5.2", "provider/glm@5@2") ||
		FamilyKey("provider/glm-5-2") != "glm52" || DisplayName("GLM@5@2-thinking") != "glm-5.2" ||
		strings.Contains(DisplayName("GLM_5_2-thinking"), "thinking") {
		t.Fatal("family normalization mismatch")
	}
}

func TestIsMetaName(t *testing.T) {
	for _, name := range []string{"*", "auto", "auto-beta", "auto-router", "router", "default", "none", "null", "any", "latest", "base", "standard", "AUTO", " Auto "} {
		if !IsMetaName(name) {
			t.Fatalf("expected %q to be meta", name)
		}
	}
	for _, name := range []string{"gpt-5.6-sol", "auto/best-chat", "mimo-auto", "gpt-5", "chat", "fast", "work"} {
		if IsMetaName(name) {
			t.Fatalf("unexpected meta classification for %q", name)
		}
	}
}

func TestBuildRemovesMetaModels(t *testing.T) {
	catalog := Build([]string{"auto", "auto-beta", "glm-5.2", "*"}, map[string][]string{
		"auto":    {"tr/auto", "trae/auto"},
		"glm-5.2": {"aug/glm-5.2"},
	})
	for _, m := range catalog.Models {
		if m == "auto" || m == "auto-beta" {
			t.Fatalf("meta model %q leaked into catalog: %#v", m, catalog.Models)
		}
	}
	if _, ok := Canonical(catalog.Models, "glm-5.2"); !ok {
		t.Fatalf("glm-5.2 missing: %#v", catalog.Models)
	}
	if catalog.Models[0] != "*" {
		t.Fatalf("wildcard must be preserved for routing, got %#v", catalog.Models)
	}
}

func TestSplitVariant(t *testing.T) {
	cases := []struct {
		name        string
		includeFast bool
		base        string
		suffixes    []string
	}{
		{"gpt-5.6-sol-low", false, "gpt-5.6-sol", []string{"low"}},
		{"gpt-5.6-sol", false, "gpt-5.6-sol", nil},
		{"claude-opus-4.7-fast-low", false, "claude-opus-4.7-fast", []string{"low"}},
		{"claude-opus-4.7-fast-low", true, "claude-opus-4.7", []string{"fast", "low"}},
		{"claude-opus-4.6-thinking-high", false, "claude-opus-4.6", []string{"thinking", "high"}},
		{"gpt-5.6-sol-extra-high", false, "gpt-5.6-sol", []string{"extra", "high"}},
		{"mistral-small-2501", false, "mistral-small-2501", nil}, // variant word not trailing
		{"fast", true, "fast", nil},                              // never strip to nothing
		{"glm-5.2-fast", false, "glm-5.2-fast", nil},             // fast gated off
		{"glm-5.2-fast", true, "glm-5.2", []string{"fast"}},      // fast gated on
		{"provider/gpt-5-mini", false, "gpt-5", []string{"mini"}},
		{"text-embedding-3-small", false, "text-embedding-3", []string{"small"}},
	}
	for _, c := range cases {
		base, suffixes := SplitVariant(c.name, c.includeFast)
		if base != c.base || strings.Join(suffixes, ",") != strings.Join(c.suffixes, ",") {
			t.Fatalf("SplitVariant(%q, %v) = (%q, %v), want (%q, %v)", c.name, c.includeFast, base, suffixes, c.base, c.suffixes)
		}
	}
}

func TestVariantSuffixesFor(t *testing.T) {
	cases := []struct {
		effort string
		fast   bool
		want   string
	}{
		{"", false, ""},
		{"low", false, "low"},
		{"medium", false, "medium"},
		{"high", false, "high"},
		{"minimal", false, "minimal"},
		{"xhigh", false, "max,high"},
		{"max", false, "max,high"},
		{"", true, "fast"},
		{"low", true, "low-fast,fast-low,low,fast"},
		{"bogus", false, ""},
		{"LOW", false, "low"},
	}
	for _, c := range cases {
		got := strings.Join((VariantOptions{Reasoning: true, Fast: c.fast}).Suffixes(c.effort), ",")
		if got != c.want {
			t.Fatalf("Suffixes(%q, fast=%v) = %q, want %q", c.effort, c.fast, got, c.want)
		}
	}
}

func TestPreferredDisplayName(t *testing.T) {
	if got := PreferredDisplayName([]string{"gpt5.6-sol", "gpt-5.6-sol"}); got != "gpt-5.6-sol" {
		t.Fatalf("preferred = %q, want gpt-5.6-sol", got)
	}
	if got := PreferredDisplayName([]string{"gpt5", "gpt-5"}); got != "gpt-5" {
		t.Fatalf("preferred = %q, want gpt-5", got)
	}
	if got := PreferredDisplayName([]string{"tacotron2"}); got != "tacotron2" {
		t.Fatalf("single name must stay, got %q", got)
	}
	if got := PreferredDisplayName([]string{"gemma4-31b", "gemma-4-31b"}); got != "gemma-4-31b" {
		t.Fatalf("preferred = %q, want gemma-4-31b", got)
	}
}

func TestBuildCanonicalPrefersBoundaryDashes(t *testing.T) {
	catalog := Build([]string{"gpt5.6-sol", "provider/gpt-5.6-sol"}, nil)
	if len(catalog.Models) != 1 || catalog.Models[0] != "gpt-5.6-sol" {
		t.Fatalf("canonical = %#v, want [gpt-5.6-sol]", catalog.Models)
	}
	if got := len(catalog.Aliases["gpt-5.6-sol"]); got != 2 {
		t.Fatalf("aliases lost: %#v", catalog.Aliases)
	}
}

func TestCandidatesBoostPrefersEffortVariant(t *testing.T) {
	models := []string{"gpt-5.6-sol", "gpt-5.6-sol-low", "gpt-5.6-sol-high"}
	catalog := Build(models, nil)
	boost := (VariantOptions{Reasoning: true}).Suffixes("low")
	got := CandidatesBoost(catalog.Models, catalog.Aliases, "gpt-5.6-sol", false, boost)
	if len(got) == 0 || got[0] != "gpt-5.6-sol-low" {
		t.Fatalf("boosted candidates = %#v, want gpt-5.6-sol-low first", got)
	}
	found := false
	for _, c := range got {
		if c == "gpt-5.6-sol" {
			found = true
		}
	}
	if !found {
		t.Fatalf("base route must remain as fallback: %#v", got)
	}
	// Unknown effort variant -> plain base candidates
	got = CandidatesBoost(catalog.Models, catalog.Aliases, "gpt-5.6-sol", false, (VariantOptions{Reasoning: true}).Suffixes("medium"))
	if len(got) == 0 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("no medium variant: candidates = %#v, want base first", got)
	}
	// No boost at all -> unchanged
	got = CandidatesBoost(catalog.Models, catalog.Aliases, "gpt-5.6-sol", false, nil)
	if len(got) == 0 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("no boost: candidates = %#v", got)
	}
}

func TestCandidatesBoostDoesNotRestackEffort(t *testing.T) {
	models := []string{"gpt-5.6-sol", "gpt-5.6-sol-low"}
	catalog := Build(models, nil)
	// Explicit variant request must not be boosted onto another effort.
	got := CandidatesBoost(catalog.Models, catalog.Aliases, "gpt-5.6-sol-low", false, (VariantOptions{Reasoning: true}).Suffixes("high"))
	for _, c := range got {
		if strings.Contains(c, "low-high") || strings.Contains(c, "high") {
			t.Fatalf("explicit variant request was re-boosted: %#v", got)
		}
	}
	// Size variant request still accepts an effort boost.
	models = []string{"gpt-5-mini", "gpt-5-mini-low"}
	catalog = Build(models, nil)
	got = CandidatesBoost(catalog.Models, catalog.Aliases, "gpt-5-mini", false, (VariantOptions{Reasoning: true}).Suffixes("low"))
	if len(got) == 0 || got[0] != "gpt-5-mini-low" {
		t.Fatalf("mini+low boost = %#v, want gpt-5-mini-low first", got)
	}
}

func TestBoostedVariantCandidatesIgnoreWildcard(t *testing.T) {
	// Wildcard-only upstream must not fabricate variant routes.
	got := BoostedVariantCandidates([]string{"*"}, nil, "gpt-5.6-sol", false, []string{"low"})
	if len(got) != 0 {
		t.Fatalf("wildcard fabricated variant routes: %#v", got)
	}
}

func TestAgentRoutesMergeIntoBareModelFamily(t *testing.T) {
	// The model name is the last path segment: ag/ and antigravity/ raw
	// routes belong to the same family as the bare model.
	if FamilyKey("ag/claude-opus-4.6") != FamilyKey("claude-opus-4.6") {
		t.Fatal("ag/ route must share the bare model family")
	}
	if FamilyKey("antigravity/gemini-2.5-flash") != FamilyKey("gemini-2.5-flash") {
		t.Fatal("antigravity/ route must share the bare model family")
	}
	if got := DisplayName("ag/claude-opus-4-6-thinking"); got != "claude-opus-4.6" {
		t.Fatalf("DisplayName = %q, want bare claude-opus-4.6", got)
	}
	catalog := Build([]string{"claude-opus-4.6", "ag/claude-opus-4-6-thinking", "antigravity/gemini-2.5-flash"}, nil)
	for _, want := range []string{"claude-opus-4.6", "gemini-2.5-flash"} {
		found := false
		for _, m := range catalog.Models {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %q in %#v", want, catalog.Models)
		}
	}
	for _, m := range catalog.Models {
		if strings.HasPrefix(m, "ag/") || strings.HasPrefix(m, "antigravity/") {
			t.Fatalf("prefixed name leaked into catalog: %#v", catalog.Models)
		}
	}
	// Prefixed raw routes survive as routes of the merged family.
	routes := catalog.Aliases["claude-opus-4.6"]
	found := false
	for _, r := range routes {
		if r == "ag/claude-opus-4-6-thinking" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ag/ raw route lost: %#v", routes)
	}
}
