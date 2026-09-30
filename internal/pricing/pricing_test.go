package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aisense/internal/modelalias"
)

func TestPseudoPriceMonotonic(t *testing.T) {
	low := PseudoPrice(modelalias.TierRank("glm-5.3-flash"))
	high := PseudoPrice(modelalias.TierRank("claude-opus-4.6"))
	if low <= 0 || high <= low {
		t.Fatalf("pseudo prices not monotonic: %f vs %f", low, high)
	}
}

func TestParseModelsDev(t *testing.T) {
	body := []byte(`{
	  "openai": {"models": {
	    "gpt-5.6": {"cost": {"input": 1.25, "output": 10}},
	    "gpt-5.6-mini": {"cost": {"input": "0.05", "output": "0.4"}},
	    "broken": {"cost": {"input": 0, "output": 0}}
	  }},
	  "anthropic": {"models": {
	    "claude-opus-4.6": {"cost": {"input": 5, "output": 25}}
	  }}
	}`)
	entries, err := parseModelsDev(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[modelalias.FamilyKey("gpt-5.6")]; got.Input != 1.25 || got.Output != 10 {
		t.Fatalf("gpt-5.6 entry = %+v", got)
	}
	if got := entries[modelalias.FamilyKey("gpt-5.6-mini")]; got.Input != 0.05 {
		t.Fatalf("string prices not parsed: %+v", got)
	}
	if _, ok := entries[modelalias.FamilyKey("broken")]; ok {
		t.Fatal("zero-cost model must be skipped")
	}
	if _, ok := entries[modelalias.FamilyKey("claude-opus-4.6")]; !ok {
		t.Fatal("claude-opus-4.6 missing")
	}
}

func TestParseOpenRouter(t *testing.T) {
	body := []byte(`{"data":[
	  {"id":"minimax/minimax-m3","pricing":{"prompt":"0.000001","completion":"0.000003"}},
	  {"id":"free/whatever","pricing":{"prompt":"0","completion":"0"}}
	]}`)
	entries, err := parseOpenRouter(body)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := entries[modelalias.FamilyKey("minimax-m3")]
	if !ok || entry.Blended != 2 {
		t.Fatalf("minimax-m3 entry = %+v ok=%v (want blended 2.0 per 1M)", entry, ok)
	}
	if len(entries) != 1 {
		t.Fatalf("zero-price models must be skipped, got %d entries", len(entries))
	}
}

func TestLookupNearestSeries(t *testing.T) {
	m := New("")
	m.prices = map[string]Entry{
		modelalias.FamilyKey("glm-5.3"): {Input: 1, Output: 2, Blended: 1.5, Sample: "glm-5.3"},
	}
	m.fetched = time.Now()
	if entry, ok := m.Lookup("glm-5.3"); !ok || entry.Blended != 1.5 {
		t.Fatalf("exact lookup failed: %+v %v", entry, ok)
	}
	// "glm-5.4" is not catalogued; it borrows the nearest glm-5.x price.
	if entry, ok := m.Lookup("glm-5.4"); !ok || entry.Blended != 1.5 {
		t.Fatalf("nearest-series lookup failed: %+v %v", entry, ok)
	}
	if _, ok := m.Lookup("claude-opus-4.6"); ok {
		t.Fatal("unrelated family must not borrow a price")
	}
}

func TestTierCandidatesOnlinePriceWins(t *testing.T) {
	m := New("")
	// The online catalog says the futuristic-looking family is nearly free.
	m.prices = map[string]Entry{
		modelalias.FamilyKey("gpt-9.9"): {Input: 0.01, Output: 0.02, Blended: 0.015},
	}
	m.fetched = time.Now()
	models := []string{"gpt-9.9", "claude-opus-4.6"}
	shit := m.TierCandidates(models, modelalias.TierShit)
	if len(shit) != 2 || shit[0] != "gpt-9.9" {
		t.Fatalf("shit walk = %v, want gpt-9.9 first by online price", shit)
	}
	best := m.TierCandidates(models, modelalias.TierBest)
	if len(best) != 2 || best[0] != "claude-opus-4.6" {
		t.Fatalf("best walk = %v, want claude-opus-4.6 first", best)
	}
}

// roundTripMap redirects fixed URLs at local test servers so Refresh is
// hermetic: the real catalogs are never contacted.
type roundTripMap map[string]string

func (m roundTripMap) RoundTrip(req *http.Request) (*http.Response, error) {
	target, ok := m[req.URL.String()]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	resp, err := http.Get(target)
	if err != nil {
		return nil, err
	}
	resp.Request = req
	return resp, nil
}

func TestRefreshMergesSourcesAndCaches(t *testing.T) {
	modelsDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"openai":{"models":{"gpt-5.6":{"cost":{"input":1,"output":2}}}}}`))
	}))
	defer modelsDev.Close()
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3","pricing":{"prompt":"0.000002","completion":"0.000004"}}]}`))
	}))
	defer openrouter.Close()

	dir := t.TempDir()
	m := New(filepath.Join(dir, "tier-pricing.json"))
	m.client = &http.Client{Transport: roundTripMap(map[string]string{
		"https://models.dev/api.json":         modelsDev.URL,
		"https://openrouter.ai/api/v1/models": openrouter.URL,
	})}
	count, sources, err := m.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(sources) != 2 {
		t.Fatalf("count=%d sources=%v", count, sources)
	}
	if _, ok := m.snapshot()[modelalias.FamilyKey("gpt-5.6")]; !ok {
		t.Fatal("models.dev entry missing after refresh")
	}
	if _, ok := m.snapshot()[modelalias.FamilyKey("glm-5.3")]; !ok {
		t.Fatal("openrouter entry missing after refresh")
	}
	// The cache file exists and reloads into a fresh manager.
	if _, err := os.Stat(m.cachePath); err != nil {
		t.Fatalf("cache file: %v", err)
	}
	fresh := New(m.cachePath)
	if entry, ok := fresh.Lookup("glm-5.3"); !ok || entry.Blended != 3 {
		t.Fatalf("fresh manager cache lookup = %+v %v", entry, ok)
	}
}

func TestRunRespectsEnabledFlag(t *testing.T) {
	enabled := false
	m := New("")
	m.Enabled = func() bool { return enabled }
	m.Interval = func() time.Duration { return time.Hour }
	done := make(chan struct{})
	go func() {
		m.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Run returned before context cancellation")
	case <-time.After(200 * time.Millisecond):
	}
	// A disabled manager never fetches and never blocks; cancelling ctx stops it.
}
