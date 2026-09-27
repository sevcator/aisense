package config

import (
	"errors"
	"time"
)

// UpstreamHealth stores observed inference outcomes, not guessed availability.
// Current eligibility is derived from enabled state and live restrictions.
// Identity is an endpoint/auth-context fingerprint; no credentials are stored.
type UpstreamHealth struct {
	Identity      string     `json:"identity"`
	Status        string     `json:"status"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	// Hit windows: the recent attempts of the API base and of each API key, so
	// the panel still shows a hit chance right after a restart.
	Hits    *HitWindow            `json:"hits,omitempty"`
	KeyHits map[string]*HitWindow `json:"key_hits,omitempty"`
}

// HitWindow is one attempt history: one bit per attempt, 1 for success.
type HitWindow struct {
	Bits  uint64 `json:"bits"`
	Count int    `json:"count"`
}

// UpstreamHits is the saved hit history of one upstream. Identity pins it to
// the endpoint and auth context it was observed on.
type UpstreamHits struct {
	Identity string
	Endpoint HitWindow
	Keys     map[string]HitWindow
}

// SaveHitWindows stores the hit history of every upstream it still matches.
// Nothing is written when no window changed.
func (m *Manager) SaveHitWindows(windows map[string]UpstreamHits) error {
	if len(windows) == 0 {
		return nil
	}
	err := m.Update(func(c *Config) error {
		changed := false
		for _, up := range c.Upstreams {
			hits, ok := windows[up.ID]
			if !ok || hits.Identity != up.HitchanceIdentity() {
				continue
			}
			if up.Health == nil {
				up.Health = &UpstreamHealth{Identity: up.HitchanceIdentity()}
			}
			if up.Health.Identity != up.HitchanceIdentity() {
				continue // the endpoint or its auth was edited; that history is gone
			}
			endpoint := hits.Endpoint
			if up.Health.Hits == nil || *up.Health.Hits != endpoint {
				up.Health.Hits = &endpoint
				changed = true
			}
			keys := map[string]*HitWindow{}
			for fingerprint, window := range hits.Keys {
				saved := window
				keys[fingerprint] = &saved
				if previous, ok := up.Health.KeyHits[fingerprint]; !ok || previous == nil || *previous != saved {
					changed = true
				}
			}
			if len(keys) != len(up.Health.KeyHits) {
				changed = true
			}
			up.Health.KeyHits = keys
		}
		if !changed {
			return errUpstreamHealthUnchanged
		}
		return nil
	})
	if errors.Is(err, errUpstreamHealthUnchanged) {
		return nil
	}
	return err
}

var errUpstreamHealthUnchanged = errors.New("upstream health unchanged")

// ObserveUpstreamHealth persists real inference evidence independently of the
// failure policy. Late successes cannot override a concurrently recorded failure.
func (m *Manager) ObserveUpstreamHealth(snapshot *Upstream, success bool) error {
	if snapshot == nil {
		return nil
	}
	err := m.Update(func(c *Config) error {
		for _, up := range c.Upstreams {
			if up.ID != snapshot.ID || !up.Enabled || up.HitchanceIdentity() != snapshot.HitchanceIdentity() {
				continue
			}
			now := time.Now()
			var next UpstreamHealth
			if up.Health != nil && up.Health.Identity == up.HitchanceIdentity() {
				next = *up.Health
			}
			if success {
				if next.LastFailureAt != nil && (snapshot.Health == nil || snapshot.Health.Identity != next.Identity || snapshot.Health.LastFailureAt == nil || next.LastFailureAt.After(*snapshot.Health.LastFailureAt)) {
					return errUpstreamHealthUnchanged
				}
				// The failure policy and this observation may be separate transactions.
				// Catch the interval after a newer restriction but before its health write.
				for target, state := range up.HitchanceState {
					previous, existed := snapshot.HitchanceState[target]
					if (state.Identity == "" || state.Identity == up.HitchanceIdentity()) && (!existed || state.UpdatedAt.After(previous.UpdatedAt)) {
						return errUpstreamHealthUnchanged
					}
				}
				if next.Status == "available" && next.LastSuccessAt != nil && now.Sub(*next.LastSuccessAt) < time.Minute {
					return errUpstreamHealthUnchanged
				}
				next.Status = "available"
				next.LastSuccessAt = &now
			} else {
				next.Status = "unavailable"
				next.LastFailureAt = &now
			}
			next.Identity = up.HitchanceIdentity()
			up.Health = &next
			return nil
		}
		return errUpstreamHealthUnchanged
	})
	if errors.Is(err, errUpstreamHealthUnchanged) {
		return nil
	}
	return err
}
