package modelalias

import (
	"sync"
	"sync/atomic"
)

// Model names are turned into family keys and display names constantly: once
// per upstream per request, and many times per save. Both are pure functions
// of the name, so the answers are remembered instead of recomputed.
const maxMemoEntries = 8192

type nameMemo struct {
	values sync.Map // name -> string
	count  atomic.Int64
}

// get returns the remembered value, computing it on first sight. The cap keeps
// a client asking for endless made-up model names from growing the table.
func (m *nameMemo) get(name string, build func(string) string) string {
	if cached, ok := m.values.Load(name); ok {
		return cached.(string)
	}
	value := build(name)
	if m.count.Load() < maxMemoEntries {
		if _, loaded := m.values.LoadOrStore(name, value); !loaded {
			m.count.Add(1)
		}
	}
	return value
}

var (
	familyKeyMemo   nameMemo
	displayNameMemo nameMemo
)
