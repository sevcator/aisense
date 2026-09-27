package routehealth

import (
	"sync"
	"testing"
	"time"

	"aisense/internal/config"
)

func testUp(id string, priority int) *config.Upstream {
	return &config.Upstream{ID: id, Type: "openai", BaseURL: "https://" + id + ".example/v1", Models: []string{"*"}, Priority: priority, Enabled: true}
}

func TestOrderUsesHealthBeforePriority(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	high := testUp("high", 1)
	low := testUp("low", 10)
	m.MarkFailure(high, "glm-5.2", Global, time.Minute, "network error")
	ordered := m.Order([]*config.Upstream{high, low}, "glm-5.2")
	if ordered[0] != low {
		t.Fatalf("ordered = %s, %s", ordered[0].ID, ordered[1].ID)
	}
}

func TestOrderUsesLeastInFlightAndRoundRobinTies(t *testing.T) {
	m := New()
	a, b := testUp("a", 10), testUp("b", 10)
	done := m.Begin(a)
	if got := m.Order([]*config.Upstream{a, b}, "model")[0].ID; got != "b" {
		t.Fatalf("least-loaded first = %q", got)
	}
	done()
	first := m.Order([]*config.Upstream{a, b}, "other")[0].ID
	second := m.Order([]*config.Upstream{a, b}, "other")[0].ID
	if first == second {
		t.Fatalf("round robin did not rotate: %q, %q", first, second)
	}
}

func TestModelFailureDoesNotHideOtherModels(t *testing.T) {
	m := New()
	up := testUp("up", 1)
	m.MarkSuccess(up, "good-model")
	m.MarkFailure(up, "bad-model", Model, time.Minute, "model unavailable")
	snapshot := m.Snapshot([]*config.Upstream{up})[0]
	if snapshot.Status != "degraded" || len(snapshot.UnavailableModels) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	other := testUp("other", 2)
	if got := m.Order([]*config.Upstream{up, other}, "good-model")[0].ID; got != "up" {
		t.Fatalf("unrelated model was penalized: %q", got)
	}
}

func TestCooldownRecoveryAndEndpointEditUseFreshIdentity(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	up := testUp("up", 1)
	m.MarkFailure(up, "model", Global, time.Minute, "network error")
	if got := m.Snapshot([]*config.Upstream{up})[0].Status; got != "unavailable" {
		t.Fatalf("status during cooldown = %q", got)
	}
	now = now.Add(2 * time.Minute)
	if got := m.Snapshot([]*config.Upstream{up})[0].Status; got != "ready" {
		t.Fatalf("status after cooldown = %q", got)
	}
	edited := *up
	edited.BaseURL = "https://new.example/v1"
	if got := m.Snapshot([]*config.Upstream{&edited})[0].Status; got != "ready" {
		t.Fatalf("edited endpoint inherited health = %q", got)
	}
}

func TestConcurrentOrderingAndAccounting(t *testing.T) {
	m := New()
	ups := []*config.Upstream{testUp("a", 1), testUp("b", 1), testUp("c", 1)}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ordered := m.Order(ups, "model")
			done := m.Begin(ordered[0])
			m.MarkSuccess(ordered[0], "model")
			done()
		}()
	}
	wg.Wait()
	for _, record := range m.Snapshot(ups) {
		if record.InFlight != 0 {
			t.Fatalf("leaked in-flight count: %#v", record)
		}
	}
}

func TestOrderUsesPriorityBeforeInFlight(t *testing.T) {
	m := New()
	high := testUp("high", 1) // higher priority (lower number)
	low := testUp("low", 2)   // lower priority, but idle

	// Both have known-good health so rank == 0 for both.
	m.MarkSuccess(high, "model")
	m.MarkSuccess(low, "model")

	// Put load on high; low is completely idle.
	done1 := m.Begin(high)
	done2 := m.Begin(high)
	defer done1()
	defer done2()

	ordered := m.Order([]*config.Upstream{high, low}, "model")
	if ordered[0].ID != "high" {
		t.Fatalf("priority should beat in-flight load: got %q first (want %q)", ordered[0].ID, "high")
	}
}

func TestOrderPrefersExplicitCatalogOverWildcardAtSameRankAndPriority(t *testing.T) {
	m := New()
	wildcard := testUp("a-wildcard", 10)
	explicit := testUp("z-explicit", 10)
	explicit.Models = []string{"claude-4.6-opus"}

	for i := 0; i < 4; i++ {
		ordered := m.Order([]*config.Upstream{wildcard, explicit}, "claude-opus-4.6")
		if ordered[0] != explicit {
			t.Fatalf("explicit catalog should precede wildcard, got %q first", ordered[0].ID)
		}
	}
}

func TestOrderKeepsConfiguredPriorityAheadOfCatalogSpecificity(t *testing.T) {
	m := New()
	wildcard := testUp("wildcard", 1)
	explicit := testUp("explicit", 2)
	explicit.Models = []string{"model"}

	ordered := m.Order([]*config.Upstream{explicit, wildcard}, "model")
	if ordered[0] != wildcard {
		t.Fatalf("configured priority should remain authoritative, got %q first", ordered[0].ID)
	}
}
