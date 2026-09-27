package routehealth

import (
	"math/bits"

	"aisense/internal/config"
)

// hitWindowSize is how many recent attempts a hit chance is computed from.
const hitWindowSize = 64

// hitWindow remembers the outcome of the most recent attempts, one bit each
// (1 = success). It is in-memory only and starts empty after a restart.
type hitWindow struct {
	bits uint64
	n    int
}

func (w *hitWindow) add(ok bool) {
	w.bits <<= 1
	if ok {
		w.bits |= 1
	}
	if w.n < hitWindowSize {
		w.n++
	}
}

// HitRate is the share of successful attempts among the most recent ones.
type HitRate struct {
	Chance   float64
	Attempts int
}

func (w *hitWindow) rate() HitRate {
	if w == nil || w.n == 0 {
		return HitRate{}
	}
	mask := ^uint64(0)
	if w.n < hitWindowSize {
		mask = uint64(1)<<w.n - 1
	}
	return HitRate{Chance: float64(bits.OnesCount64(w.bits&mask)) / float64(w.n), Attempts: w.n}
}

func (w hitWindow) saved() config.HitWindow { return config.HitWindow{Bits: w.bits, Count: w.n} }

func restoredWindow(saved *config.HitWindow) hitWindow {
	if saved == nil || saved.Count <= 0 {
		return hitWindow{}
	}
	count := saved.Count
	if count > hitWindowSize {
		count = hitWindowSize
	}
	return hitWindow{bits: saved.Bits, n: count}
}

// seedHitWindows restores the hits saved for this endpoint and auth context,
// once per entry: from then on the live attempts are the truth.
func seedHitWindows(e *entry, up *config.Upstream) {
	if e.hitsSeeded {
		return
	}
	e.hitsSeeded = true
	e.hitIdentity = up.HitchanceIdentity()
	h := up.Health
	if h == nil || h.Identity != e.hitIdentity {
		return
	}
	e.Hits = restoredWindow(h.Hits)
	for fingerprint, saved := range h.KeyHits {
		if window := restoredWindow(saved); window.n > 0 {
			if e.KeyHits == nil {
				e.KeyHits = map[string]*hitWindow{}
			}
			restored := window
			e.KeyHits[fingerprint] = &restored
		}
	}
}

// HitSnapshot is the hit history of every upstream seen in this process, ready
// to be saved so a restart does not start from "no requests yet".
func (m *Manager) HitSnapshot() map[string]config.UpstreamHits {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]config.UpstreamHits{}
	for _, e := range m.entries {
		if e.Hits.n == 0 && len(e.KeyHits) == 0 {
			continue
		}
		hits := config.UpstreamHits{Identity: e.hitIdentity, Endpoint: e.Hits.saved(), Keys: map[string]config.HitWindow{}}
		for fingerprint, window := range e.KeyHits {
			hits.Keys[fingerprint] = window.saved()
		}
		out[e.ID] = hits
	}
	return out
}

// RecordAttempt remembers whether one attempt through the endpoint succeeded,
// for the endpoint and, when key is not empty, for that API key.
func (m *Manager) RecordAttempt(up *config.Upstream, key string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.ensure(up)
	e.Hits.add(ok)
	if key == "" {
		return
	}
	if e.KeyHits == nil {
		e.KeyHits = map[string]*hitWindow{}
	}
	fingerprint := config.KeyFingerprint(key)
	w := e.KeyHits[fingerprint]
	if w == nil {
		w = &hitWindow{}
		e.KeyHits[fingerprint] = w
	}
	w.add(ok)
}

// HitRates reports the recent hit chance of the endpoint and of each key,
// keyed by key fingerprint. Editing the endpoint or its auth starts afresh.
func (m *Manager) HitRates(up *config.Upstream) (HitRate, map[string]HitRate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if up == nil {
		return HitRate{}, nil
	}
	e := m.ensure(up) // also restores the hits saved before the last restart
	keys := make(map[string]HitRate, len(e.KeyHits))
	for fingerprint, w := range e.KeyHits {
		keys[fingerprint] = w.rate()
	}
	return e.Hits.rate(), keys
}
