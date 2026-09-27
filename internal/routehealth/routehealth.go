package routehealth

import (
	"sort"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

type Scope int

const (
	Global Scope = iota
	Model
)

type failure struct {
	Until time.Time
	Error string
	Model string
}

type entry struct {
	ID                string
	Identity          string
	InFlight          int
	LastSuccess       time.Time
	LastFailure       time.Time
	LastError         string
	NeedsRevalidation bool
	Global            failure
	Models            map[string]failure
	Hits              hitWindow             // every attempt through the endpoint
	KeyHits           map[string]*hitWindow // by key fingerprint
	hitIdentity       string                // endpoint/auth context the hits belong to
	hitsSeeded        bool                  // saved hits are restored once per process
}

type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry
	cursors map[string]uint64
	now     func() time.Time
}

type ModelRecord struct {
	Model         string    `json:"model"`
	CooldownUntil time.Time `json:"cooldown_until"`
	LastError     string    `json:"last_error,omitempty"`
}

type Record struct {
	ID                string        `json:"id"`
	Status            string        `json:"status"`
	StatusReason      string        `json:"status_reason,omitempty"`
	InFlight          int           `json:"in_flight"`
	LastSuccessAt     *time.Time    `json:"last_success_at,omitempty"`
	LastFailureAt     *time.Time    `json:"last_failure_at,omitempty"`
	CooldownUntil     *time.Time    `json:"cooldown_until,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	UnavailableModels []ModelRecord `json:"unavailable_models,omitempty"`
}

func New() *Manager {
	return &Manager{entries: map[string]*entry{}, cursors: map[string]uint64{}, now: time.Now}
}

func identity(up *config.Upstream) string {
	if up == nil {
		return ""
	}
	endpoint := config.ExactEndpointIdentity(up)
	if endpoint == "" {
		endpoint = strings.ToLower(strings.TrimSpace(up.Type)) + "|" + strings.TrimSpace(up.BaseURL)
	}
	return up.ID + "|" + endpoint + "|" + up.HitchanceIdentity()
}

func (m *Manager) ensure(up *config.Upstream) *entry {
	key := identity(up)
	e := m.entries[key]
	if e == nil {
		e = &entry{ID: up.ID, Identity: key, Models: map[string]failure{}}
		m.entries[key] = e
	}
	mergePersistedHealth(e, up)
	seedHitWindows(e, up)
	return e
}

// Restore observations after a restart without inventing a probe or reviving
// expired restrictions. Runtime observations newer than disk remain authoritative.
func mergePersistedHealth(e *entry, up *config.Upstream) {
	h := up.Health
	if h == nil || h.Identity != up.HitchanceIdentity() {
		return
	}
	latest := e.LastSuccess
	if e.LastFailure.After(latest) {
		latest = e.LastFailure
	}
	persisted := time.Time{}
	if h.LastSuccessAt != nil {
		persisted = *h.LastSuccessAt
	}
	if h.LastFailureAt != nil && h.LastFailureAt.After(persisted) {
		persisted = *h.LastFailureAt
	}
	if !persisted.After(latest) {
		return
	}
	if h.LastSuccessAt != nil && h.LastSuccessAt.After(e.LastSuccess) {
		e.LastSuccess = *h.LastSuccessAt
	}
	if h.LastFailureAt != nil && h.LastFailureAt.After(e.LastFailure) {
		e.LastFailure = *h.LastFailureAt
	}
	e.NeedsRevalidation = h.Status != "available" || e.LastSuccess.IsZero()
}

func active(f failure, now time.Time) bool { return f.Until.After(now) }

func rank(e *entry, family string, now time.Time) int {
	if e == nil {
		return 1 // unknown
	}
	if active(e.Global, now) {
		return 3 // unavailable
	}
	modelFailure := e.Models[family]
	if active(modelFailure, now) {
		return 2 // degraded for this model
	}
	if e.LastSuccess.IsZero() {
		return 1
	}
	return 0
}

type candidate struct {
	up       *config.Upstream
	health   int
	wildcard bool
	inFlight int
	tie      int
}

func wildcardOnly(up *config.Upstream, model string) bool {
	explicit := make([]string, 0, len(up.Models))
	hasWildcard := false
	for _, advertised := range up.Models {
		if strings.TrimSpace(advertised) == "*" {
			hasWildcard = true
			continue
		}
		explicit = append(explicit, advertised)
	}
	if !hasWildcard {
		return false
	}
	return len(modelalias.Candidates(explicit, up.ModelAliases, model, true)) == 0
}

// Order returns a fresh health/load-aware ordering. The cursor advances once
// per call so concurrent equal candidates do not all begin at the same row.
func (m *Manager) Order(ups []*config.Upstream, model string) []*config.Upstream {
	if len(ups) < 2 {
		return append([]*config.Upstream(nil), ups...)
	}
	family := modelalias.FamilyKey(model)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	ids := make([]string, 0, len(ups))
	for _, up := range ups {
		ids = append(ids, up.ID)
	}
	sort.Strings(ids)
	cursor := int(m.cursors[family] % uint64(len(ids)))
	m.cursors[family]++
	ties := make(map[string]int, len(ids))
	for i, id := range ids {
		ties[id] = (i - cursor + len(ids)) % len(ids)
	}
	items := make([]candidate, 0, len(ups))
	for _, up := range ups {
		e := m.entries[identity(up)]
		inFlight := 0
		if e != nil {
			inFlight = e.InFlight
		}
		items = append(items, candidate{
			up: up, health: rank(e, family, now), wildcard: wildcardOnly(up, model),
			inFlight: inFlight, tie: ties[up.ID],
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.health != b.health {
			return a.health < b.health
		}
		if a.up.Priority != b.up.Priority {
			return a.up.Priority < b.up.Priority
		}
		if a.wildcard != b.wildcard {
			return !a.wildcard
		}
		if a.inFlight != b.inFlight {
			return a.inFlight < b.inFlight
		}
		return a.tie < b.tie
	})
	out := make([]*config.Upstream, len(items))
	for i := range items {
		out[i] = items[i].up
	}
	return out
}

// Begin accounts for one complete upstream visit. The returned function is
// idempotent and must remain active for the lifetime of a streaming response.
func (m *Manager) Begin(up *config.Upstream) func() {
	m.mu.Lock()
	e := m.ensure(up)
	e.InFlight++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if current := m.entries[e.Identity]; current != nil && current.InFlight > 0 {
				current.InFlight--
			}
			m.mu.Unlock()
		})
	}
}

func (m *Manager) InFlight(up *config.Upstream) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[identity(up)]; e != nil {
		return e.InFlight
	}
	return 0
}

func safeError(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

func (m *Manager) MarkSuccess(up *config.Upstream, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e := m.ensure(up)
	e.LastSuccess = now
	e.NeedsRevalidation = false
	e.Global = failure{}
	delete(e.Models, modelalias.FamilyKey(model))
	if len(e.Models) == 0 {
		e.LastError = ""
	}
}

func (m *Manager) MarkFailure(up *config.Upstream, model string, scope Scope, duration time.Duration, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e := m.ensure(up)
	f := failure{Until: now.Add(duration), Error: safeError(message)}
	e.LastFailure = now
	e.NeedsRevalidation = true
	e.LastError = f.Error
	if scope == Model {
		f.Model = modelalias.DisplayName(model)
		e.Models[modelalias.FamilyKey(model)] = f
	} else {
		e.Global = f
	}
}

func status(e *entry, now time.Time) string {
	if e == nil {
		return "ready"
	}
	if active(e.Global, now) {
		return "unavailable"
	}
	for _, f := range e.Models {
		if active(f, now) {
			return "degraded"
		}
	}
	if e.LastSuccess.IsZero() || e.NeedsRevalidation {
		return "ready"
	}
	return "available"
}

func (m *Manager) Snapshot(ups []*config.Upstream) []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	live := map[string]bool{}
	out := make([]Record, 0, len(ups))
	for _, up := range ups {
		key := identity(up)
		live[key] = true
		e := m.entries[key]
		if e != nil || up.Health != nil && up.Health.Identity == up.HitchanceIdentity() {
			e = m.ensure(up)
		}
		record := Record{ID: up.ID, Status: "ready", StatusReason: "Eligible; inference availability has not been verified."}
		if !up.Enabled {
			record.Status = "disabled"
		} else if e != nil {
			if len(e.Models) > 0 {
				allowed := map[string]bool{}
				wildcard := false
				catalog := modelalias.Build(up.Models, up.ModelAliases)
				for _, model := range catalog.Models {
					if model == "*" {
						wildcard = true
					}
					allowed[modelalias.FamilyKey(model)] = true
					for _, raw := range catalog.Aliases[model] {
						allowed[modelalias.FamilyKey(raw)] = true
					}
				}
				for family, f := range e.Models {
					if !active(f, now) || !wildcard && !allowed[family] {
						delete(e.Models, family)
					}
				}
			}
			record.Status = status(e, now)
			record.InFlight = e.InFlight
			if !e.LastSuccess.IsZero() {
				value := e.LastSuccess
				record.LastSuccessAt = &value
			}
			if !e.LastFailure.IsZero() {
				value := e.LastFailure
				record.LastFailureAt = &value
			}
			if record.Status == "degraded" || record.Status == "unavailable" {
				record.LastError = e.LastError
			}
			if active(e.Global, now) {
				value := e.Global.Until
				record.CooldownUntil = &value
			}
			for family, f := range e.Models {
				if active(f, now) {
					model := f.Model
					if model == "" {
						model = family
					}
					record.UnavailableModels = append(record.UnavailableModels, ModelRecord{Model: model, CooldownUntil: f.Until, LastError: f.Error})
				}
			}
			sort.Slice(record.UnavailableModels, func(i, j int) bool { return record.UnavailableModels[i].Model < record.UnavailableModels[j].Model })
		}
		switch record.Status {
		case "disabled":
			record.StatusReason = "Disabled by configuration."
		case "available":
			record.StatusReason = "Successful inference observed."
		case "degraded":
			record.StatusReason = "Some models have active runtime failures."
		case "unavailable":
			record.StatusReason = "Endpoint failure cooldown is active."
		}
		out = append(out, record)
	}
	for key := range m.entries {
		if !live[key] {
			delete(m.entries, key)
		}
	}
	return out
}
