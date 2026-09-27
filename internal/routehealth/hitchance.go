package routehealth

import (
	"aisense/internal/config"
	"aisense/internal/modelalias"
)

func (m *Manager) Reset(up *config.Upstream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[identity(up)]; e != nil {
		e.Global = failure{}
		e.Models = map[string]failure{}
		e.LastError = ""
	}
}

// FailureRank ignores success/unknown and round-robin so this can wrap sticky
// and variant ordering without disrupting fair rotation among healthy routes.
func (m *Manager) FailureRank(up *config.Upstream, model string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[identity(up)]
	if e == nil {
		return 0
	}
	now := m.now()
	if active(e.Global, now) {
		return 2
	}
	if active(e.Models[modelalias.FamilyKey(model)], now) {
		return 1
	}
	return 0
}
