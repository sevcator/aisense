package modelalias

import (
	"reflect"
	"testing"
)

func TestTierNames(t *testing.T) {
	for _, name := range []string{"best", "BEST", "Shit", "ag/best", " provider/best"} {
		if tier, ok := TierName(name); !ok || (tier != TierBest && tier != TierShit) {
			t.Fatalf("TierName(%q) = %q, %v", name, tier, ok)
		}
	}
	for _, name := range []string{"", "*", "bestx", "gpt-best", "shitstorm", "auto", "free"} {
		if _, ok := TierName(name); ok {
			t.Fatalf("TierName(%q) must not be a tier", name)
		}
		if IsTierName(name) {
			t.Fatalf("IsTierName(%q) = true", name)
		}
	}
}

func TestIsJunkName(t *testing.T) {
	for _, name := range []string{"free", "FREE", "free-model", "unknown", "placeholder", "test", "demo", "sandbox", "step-router-v1", "router", "routerv2", "ag/router-v1"} {
		if !IsJunkName(name) {
			t.Fatalf("IsJunkName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"model", "chat", "assistant", "gpt-4-free", "glm-5.3-flash", "ornit-assistant", "gpt-test", "testmodel", "main-model", "freechat"} {
		if IsJunkName(name) {
			t.Fatalf("IsJunkName(%q) = true, want false", name)
		}
	}
}

func TestIsChatModel(t *testing.T) {
	for _, name := range []string{"text-embedding-v4", "text-embedding-v3", "agnes-image-2.5-flash", "agnes-video-2.5", "stepaudio-2.5-chat", "whisper-large", "reranker-v2"} {
		if IsChatModel(name) {
			t.Fatalf("IsChatModel(%q) = true, want false", name)
		}
	}
	for _, name := range []string{"glm-5.3", "claude-opus-4.6", "minimax-m3", "kimi-k3"} {
		if !IsChatModel(name) {
			t.Fatalf("IsChatModel(%q) = false, want true", name)
		}
	}
}

func TestTierCandidatesHeuristicOrder(t *testing.T) {
	models := []string{
		"gpt-5.6", "claude-opus-4.6", "glm-5.3-flash", "minimax-m2.7-highspeed",
		"free", "text-embedding-v3", "MiniMax-M2.1-highspeed",
	}
	best := TierCandidates(models, TierBest)
	if len(best) == 0 || best[0] != "claude-opus-4.6" {
		t.Fatalf("best[0] = %v, want claude-opus-4.6 first", best)
	}
	shit := TierCandidates(models, TierShit)
	if len(shit) == 0 || shit[0] != "minimax-m2.1-highspeed" {
		t.Fatalf("shit[0] = %v, want minimax-m2.1-highspeed first", shit)
	}
	// Junk, non-chat and duplicate families never appear.
	for _, name := range append(append([]string(nil), best...), shit...) {
		if !SelectableModel(name) {
			t.Fatalf("tier walk contains unselectable model %q", name)
		}
	}
	for _, walk := range [][]string{best, shit} {
		for i, name := range walk {
			for _, later := range walk[i+1:] {
				if FamilyKey(name) == FamilyKey(later) {
					t.Fatalf("tier walk repeats family: %q and %q", name, later)
				}
			}
		}
	}
	if !reflect.DeepEqual(best, reverse(shit)) && len(best) != len(shit) {
		t.Fatalf("tier walks disagree in length: %v vs %v", best, shit)
	}
}

func TestTierCandidatesCap(t *testing.T) {
	models := make([]string, 0, MaxTierCandidates+8)
	for i := 0; i < MaxTierCandidates+8; i++ {
		models = append(models, "model-"+string(rune('a'+i)))
	}
	walk := TierCandidates(models, TierBest)
	if len(walk) != MaxTierCandidates {
		t.Fatalf("tier walk length = %d, want %d", len(walk), MaxTierCandidates)
	}
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
