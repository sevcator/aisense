package pricing

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aisense/internal/modelalias"
)

// Manager keeps a periodically refreshed map of model family → market price,
// fetched from public model catalogs (models.dev, OpenRouter). Tier ranking
// reads it live, so "best"/"shit" always reflect the current online top
// models instead of a frozen list. When a family has no online price, the
// modelalias heuristic pseudo-price fills the gap.
type Manager struct {
	// Enabled and Interval are read live so operators can retune the refresh
	// through the config file without a restart.
	Enabled  func() bool
	Interval func() time.Duration

	// IgnoreCerts mirrors the model-discovery TLS setting for catalog fetches.
	IgnoreCerts func() bool
	// Debug receives refresh events; may be nil.
	Debug func(event string, fields map[string]any)

	cachePath string
	client    *http.Client

	mu       sync.RWMutex
	prices   map[string]Entry // key: modelalias.FamilyKey
	fetched  time.Time
	lastErr  string
	sources  []string
	loadOnce sync.Once
}

// Entry is the market price of one model family, USD per 1M tokens. Sample
// keeps one catalogued spelling of the family so nearest-series matching can
// compare real version numbers — the FamilyKey itself is punctuation-free
// ("glm-5.3" → "glm53") and would misread "53" as a major version.
type Entry struct {
	Input   float64 `json:"input"`
	Output  float64 `json:"output"`
	Blended float64 `json:"blended"`
	Source  string  `json:"source"`
	Sample  string  `json:"sample,omitempty"`
}

// Price converts the modelalias heuristic score into a plausible market price
// (USD per 1M blended tokens) so online prices and heuristic estimates share
// one ordering scale. Known-price families always come from the network.
func PseudoPrice(score float64) float64 {
	price := 0.05 * math.Pow(1.16, score)
	if price < 0.01 {
		price = 0.01
	}
	if price > 3 {
		price = 3
	}
	return price
}

// New creates a manager with a JSON cache beside the config. The cache is
// loaded lazily on first use so a missing or corrupt file never blocks start.
func New(cachePath string) *Manager {
	return &Manager{cachePath: cachePath, prices: map[string]Entry{}}
}

func (m *Manager) httpClient() *http.Client {
	if m.client != nil {
		return m.client
	}
	transport := &http.Transport{MaxIdleConns: 4}
	if m.IgnoreCerts != nil && m.IgnoreCerts() {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	m.client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	return m.client
}

// Run refreshes when enabled and checks periodically for configuration changes.
// An installation with no enabled upstreams does not need an online catalog;
// the first refresh begins shortly after an upstream is added.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var lastRefresh time.Time
	wasEnabled := false
	for {
		enabled := m.Enabled == nil || m.Enabled()
		if enabled && (!wasEnabled || time.Since(lastRefresh) >= m.currentInterval()) {
			lastRefresh = time.Now()
			m.refreshLogged(ctx)
		}
		wasEnabled = enabled
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) currentInterval() time.Duration {
	if m.Interval != nil {
		if d := m.Interval(); d >= time.Minute {
			return d
		}
	}
	return time.Hour
}

func (m *Manager) refreshLogged(ctx context.Context) {
	if m.Enabled != nil && !m.Enabled() {
		return
	}
	started := time.Now()
	count, sources, err := m.Refresh(ctx)
	if err != nil && count == 0 {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		log.Printf("[aisense] tier pricing refresh failed: %v (keeping %d cached prices)", err, len(m.snapshot()))
		m.event("pricing.refresh_failed", map[string]any{"error": err.Error()})
		return
	}
	m.mu.Lock()
	m.lastErr = ""
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
	log.Printf("[aisense] public tier price catalog refreshed: %d entries from %s in %s (not configured models)", count, strings.Join(sources, "+"), time.Since(started).Round(time.Millisecond))
	m.event("pricing.refreshed", map[string]any{"models": count, "sources": sources, "duration_ms": time.Since(started).Milliseconds()})
}

func (m *Manager) event(name string, fields map[string]any) {
	if m.Debug != nil {
		m.Debug(name, fields)
	}
}

type fetchResult struct {
	entries map[string]Entry
	source  string
}

// Refresh fetches every catalog source and merges them into the cache.
// models.dev wins on overlap; OpenRouter fills families it does not list.
// A source that fails does not abort the refresh; total failure returns err.
func (m *Manager) Refresh(ctx context.Context) (int, []string, error) {
	var results []fetchResult
	var errs []string
	for _, source := range []struct {
		url   string
		parse func([]byte) (map[string]Entry, error)
		label string
	}{
		{"https://models.dev/api.json", parseModelsDev, "models.dev"},
		{"https://openrouter.ai/api/v1/models", parseOpenRouter, "openrouter"},
	} {
		entries, err := m.fetch(ctx, source.url, source.parse)
		if err != nil {
			errs = append(errs, source.label+": "+err.Error())
			continue
		}
		results = append(results, fetchResult{entries: entries, source: source.label})
	}
	if len(results) == 0 {
		return 0, nil, fmt.Errorf("all sources failed: %s", strings.Join(errs, "; "))
	}
	merged := map[string]Entry{}
	var sources []string
	for _, result := range results {
		sources = append(sources, result.source)
		for key, entry := range result.entries {
			if _, exists := merged[key]; !exists {
				merged[key] = entry
			}
		}
	}
	m.mu.Lock()
	m.prices = merged
	m.fetched = time.Now()
	m.sources = sources
	m.mu.Unlock()
	m.persist(merged)
	return len(merged), sources, nil
}

func (m *Manager) fetch(ctx context.Context, url string, parse func([]byte) (map[string]Entry, error)) (map[string]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aisense-tier-pricing/1.0")
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	entries, err := parse(body)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no usable entries")
	}
	return entries, nil
}

// ------------------------------------------------------------------ parsing

// flexNumber accepts JSON numbers or numeric strings ("0.000003").
type flexNumber float64

func (f *flexNumber) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexNumber(value)
	return nil
}

// models.dev catalogs models per provider with USD-per-1M cost fields.
func parseModelsDev(body []byte) (map[string]Entry, error) {
	var root map[string]struct {
		Models map[string]struct {
			Cost *struct {
				Input  flexNumber `json:"input"`
				Output flexNumber `json:"output"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	for _, provider := range root {
		for id, model := range provider.Models {
			if model.Cost == nil || model.Cost.Input <= 0 || model.Cost.Output <= 0 {
				continue
			}
			key := modelalias.FamilyKey(id)
			if key == "" {
				continue
			}
			entry := entryOf(float64(model.Cost.Input), float64(model.Cost.Output), "models.dev")
			entry.Sample = modelalias.BaseName(id)
			out[key] = entry
		}
	}
	return out, nil
}

// OpenRouter lists per-token USD prices; scale them to per-1M.
func parseOpenRouter(body []byte) (map[string]Entry, error) {
	var root struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     flexNumber `json:"prompt"`
				Completion flexNumber `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	for _, model := range root.Data {
		in := float64(model.Pricing.Prompt) * 1e6
		outv := float64(model.Pricing.Completion) * 1e6
		if in <= 0 || outv <= 0 {
			continue
		}
		key := modelalias.FamilyKey(model.ID)
		if key == "" {
			continue
		}
		entry := entryOf(in, outv, "openrouter")
		entry.Sample = modelalias.BaseName(model.ID)
		out[key] = entry
	}
	return out, nil
}

func entryOf(in, out float64, source string) Entry {
	return Entry{Input: in, Output: out, Blended: (in + out) / 2, Source: source}
}

// ------------------------------------------------------------------ lookup

func (m *Manager) snapshot() map[string]Entry {
	m.loadOnce.Do(m.loadCache)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Entry, len(m.prices))
	for key, entry := range m.prices {
		out[key] = entry
	}
	return out
}

// Lookup returns the cached online price of a model family. The exact family
// wins; otherwise the nearest version of the same vendor series is used.
func (m *Manager) Lookup(model string) (Entry, bool) {
	prices := m.snapshot()
	key := modelalias.FamilyKey(model)
	if key == "" {
		return Entry{}, false
	}
	if entry, ok := prices[key]; ok {
		return entry, true
	}
	return nearestSeries(prices, key, modelalias.BaseName(model))
}

// Status reports the cache state for logs and the admin panel.
func (m *Manager) Status() (count int, fetched time.Time, sources []string, lastErr string) {
	prices := m.snapshot()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(prices), m.fetched, append([]string(nil), m.sources...), m.lastErr
}

// seriesOf extracts the leading word run of a base name ("glm-5.3" → "glm").
func seriesOf(base string) string {
	base = strings.ToLower(strings.TrimSpace(base))
	var out strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' {
			out.WriteRune(r)
			continue
		}
		break
	}
	return out.String()
}

// nearestSeries finds the online family with the same leading series and the
// closest version number, so "glm-5.4" borrows the price of "glm-5.3" when
// the exact release is not catalogued anywhere yet.
func nearestSeries(prices map[string]Entry, key, base string) (Entry, bool) {
	series := seriesOf(base)
	if series == "" || len(series) < 2 {
		return Entry{}, false
	}
	major, minor, ok := leadingVersion(base)
	best := ""
	bestDist := 1 << 30
	for candidate, entry := range prices {
		if candidate == key {
			continue
		}
		// Compare versions on the catalogued spelling, not the FamilyKey:
		// keys drop the version dot, so "glm-5.3" would read as major 53.
		name := entry.Sample
		if name == "" {
			name = candidate
		}
		if !strings.HasPrefix(strings.ToLower(name), series) {
			continue
		}
		cMajor, cMinor, cOK := leadingVersion(name)
		if !ok || !cOK {
			// No version to compare: accept only when the series is unique.
			if best == "" {
				best = candidate
			}
			continue
		}
		dist := abs(cMajor-major)*10 + abs(cMinor-minor)
		if dist < bestDist {
			bestDist, best = dist, candidate
		}
	}
	if best == "" || (ok && bestDist > 12) {
		return Entry{}, false
	}
	return prices[best], true
}

func leadingVersion(s string) (int, int, bool) {
	var major, minor int
	var has bool
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			start := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			major, _ = strconv.Atoi(s[start:i])
			has = true
			if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
				major2, _ := strconv.Atoi(s[i+1 : i+2])
				minor = major2
			}
			break
		}
	}
	return major, minor, has
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ------------------------------------------------------------------ ranking

// TierCandidates ranks selectable chat models by effective price: the online
// blended price when the family is catalogued, the heuristic pseudo-price
// otherwise. "best" walks most expensive first, "shit" cheapest first.
func (m *Manager) TierCandidates(models []string, tier string) []string {
	desc := tier == modelalias.TierBest
	prices := m.snapshot()
	type entry struct {
		name   string
		price  float64
		online bool
	}
	seen := map[string]bool{}
	var entries []entry
	for _, model := range models {
		if !modelalias.SelectableModel(model) {
			continue
		}
		name := modelalias.DisplayName(model)
		if !modelalias.SelectableModel(name) {
			continue
		}
		key := modelalias.FamilyKey(name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if online, ok := prices[key]; ok {
			entries = append(entries, entry{name: name, price: online.Blended, online: true})
			continue
		}
		if online, ok := nearestSeries(prices, key, name); ok {
			entries = append(entries, entry{name: name, price: online.Blended, online: true})
			continue
		}
		entries = append(entries, entry{name: name, price: PseudoPrice(modelalias.TierRank(name))})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].price != entries[j].price {
			if desc {
				return entries[i].price > entries[j].price
			}
			return entries[i].price < entries[j].price
		}
		if entries[i].online != entries[j].online {
			return entries[i].online // at equal price prefer the priced, verified family
		}
		return entries[i].name < entries[j].name
	})
	if len(entries) > modelalias.MaxTierCandidates {
		entries = entries[:modelalias.MaxTierCandidates]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}

// ------------------------------------------------------------------ cache

type cacheFile struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Prices    map[string]Entry `json:"prices"`
}

func (m *Manager) persist(prices map[string]Entry) {
	if m.cachePath == "" {
		return
	}
	file := cacheFile{FetchedAt: time.Now(), Prices: prices}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(m.cachePath, data, 0o600); err != nil {
		log.Printf("[aisense] tier pricing cache write failed: %v", err)
	}
}

func (m *Manager) loadCache() {
	if m.cachePath == "" {
		return
	}
	data, err := os.ReadFile(m.cachePath)
	if err != nil {
		return
	}
	var file cacheFile
	if err := json.Unmarshal(data, &file); err != nil || len(file.Prices) == 0 {
		return
	}
	m.mu.Lock()
	if len(m.prices) == 0 {
		m.prices = file.Prices
		m.fetched = file.FetchedAt
	}
	m.mu.Unlock()
}
