package proxy

import (
	"net/http"
	"path/filepath"
	"testing"

	"aisense/internal/config"
	"aisense/internal/store"
)

// A key's blacklist refuses a model even when its whitelist would allow it,
// and an upstream's whitelist narrows what that upstream serves.
func TestModelPoliciesBlacklistAndWhitelist(t *testing.T) {
	// Policy lists match by model family (the same semantics as blacklists
	// always had); "*" alone means everything.
	key := &config.APIKey{ID: "k", Name: "k", AllowedModels: []string{"glm-5.2", "glm-5.3"}, BlockedModels: []string{"glm-5.3"}}
	if allowedModel(key, "glm-5.3") {
		t.Fatal("blacklisted model must be refused despite the whitelist")
	}
	if !allowedModel(key, "glm-5.2") {
		t.Fatal("whitelisted model must pass")
	}
	if allowedModel(key, "gpt-5.6") {
		t.Fatal("model outside the whitelist must be refused")
	}
	onlyBlacklist := &config.APIKey{ID: "k", Name: "k", BlockedModels: []string{"gpt-5.6"}}
	if allowedModel(onlyBlacklist, "gpt-5.6") || !allowedModel(onlyBlacklist, "glm-5.2") {
		t.Fatal("a blacklist without a whitelist must block only listed families")
	}
	all := &config.APIKey{ID: "k", Name: "k", BlockedModels: []string{"*"}}
	if allowedModel(all, "glm-5.2") {
		t.Fatal("the * blacklist must block everything")
	}
}

func TestUpstreamWhitelistGuardsRoutingAndListing(t *testing.T) {
	up := &config.Upstream{ID: "up", Enabled: true, Models: []string{"glm-5.2", "glm-5.3", "gpt-5.6"}, AllowedModels: []string{"glm-5.2", "glm-5.3"}}
	if upstreamModelBlocked(up, "glm-5.2") {
		t.Fatal("whitelisted model must route")
	}
	if !upstreamModelBlocked(up, "gpt-5.6") {
		t.Fatal("model outside the whitelist must not route on this upstream")
	}
	if !up.ModelVisible("glm-5.2") || up.ModelVisible("gpt-5.6") {
		t.Fatal("whitelist must govern visibility as well")
	}
	noWhitelist := &config.Upstream{ID: "up", Enabled: true, Models: []string{"glm-5.2", "gpt-5.6"}}
	if upstreamModelBlocked(noWhitelist, "gpt-5.6") {
		t.Fatal("an empty whitelist must not restrict anything")
	}
}

// The served-model history seeded from persisted cached routes survives a
// restart: a fresh proxy reports the models its previous life served.
func TestUsedModelsSeededFromPersistedRoutes(t *testing.T) {
	up, _ := tierUpstream(t)
	defer up.Close()
	dir := t.TempDir()
	manager, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "gateway", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Type: "openai", BaseURL: up.URL, Models: []string{"glm-x"}, AuthMode: "none"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	p := New(manager, usage)
	if res := tierRequest(p, "glm-x"); res.Code != http.StatusOK {
		t.Fatalf("warmup request status = %d", res.Code)
	}
	if got := p.UsedModels(); len(got) != 1 || got[0] != "glm-x" {
		t.Fatalf("used models after a request = %v", got)
	}
	// Simulate a restart: a fresh proxy over the same config (the cached route
	// persisted) must already report the served model before any traffic.
	restarted := New(manager, usage)
	if got := restarted.UsedModels(); len(got) != 1 || got[0] != "glm-x" {
		t.Fatalf("used models after restart = %v, want [glm-x]", got)
	}
}
