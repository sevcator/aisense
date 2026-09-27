package proxy

import (
	"aisense/internal/config"
	"aisense/internal/modelalias"
	"context"
	"net/http"
	"sort"
	"time"
)

// Persist observation with the exact per-attempt snapshot; the manager rejects
// edited identities and successes older than a concurrent failure.
func (p *Proxy) observeUpstreamHealth(up *config.Upstream, r *http.Request, success bool) {
	if err := p.Cfg.ObserveUpstreamHealth(up, success); err != nil {
		p.trace(r, "health.persist_error", map[string]any{"upstream_id": up.ID})
	}
}

// recordAttempt feeds the hit chance shown for each API base and API key.
// Client-caused and unclassified errors are not counted against either.
func (p *Proxy) recordAttempt(up *config.Upstream, key string, ok bool) {
	if p.Health != nil && up != nil {
		p.Health.RecordAttempt(up, key, ok)
	}
}

func (p *Proxy) hitchanceEnabled() bool { return p.Cfg != nil && p.Cfg.Get().Hitchance.Enabled }

func keyPoolIdentity(up *config.Upstream) string {
	return up.BaseURL + "|" + up.HitchanceIdentity()
}

// Call under upstreamKeyMu before mutating a pool. An old in-flight response
// must not overwrite the pool created for an edited endpoint/auth context.
func (p *Proxy) keyPoolSnapshotCurrent(up *config.Upstream) bool {
	if up == nil {
		return false
	}
	if p.Cfg == nil {
		return true
	} // standalone pool tests
	for _, current := range p.Cfg.Get().Upstreams {
		if current.ID == up.ID {
			return keyPoolIdentity(current) == keyPoolIdentity(up)
		}
	}
	return false
}

// ResetHitchanceKeys clears only the legacy cooldowns of the persisted reset's
// current identity. A delayed reset cannot clear an edited endpoint's state.
func (p *Proxy) ResetHitchanceKeys(snapshot *config.Upstream) {
	p.upstreamKeyMu.Lock()
	defer p.upstreamKeyMu.Unlock()
	if !p.keyPoolSnapshotCurrent(snapshot) {
		return
	}
	if state := p.upstreamKeys[snapshot.ID]; state != nil && state.identity == keyPoolIdentity(snapshot) {
		state.cooldown = map[string]time.Time{}
	}
}

// A queued attempt may outlive a key deletion, cooldown, or endpoint edit.
// Never move credentials from an old route onto an edited authentication context.
func (p *Proxy) currentHitchanceUpstream(snapshot *config.Upstream) *config.Upstream {
	for _, up := range p.Cfg.Get().Upstreams {
		if up.ID == snapshot.ID && up.Enabled && up.BaseURL == snapshot.BaseURL && up.HitchanceIdentity() == snapshot.HitchanceIdentity() {
			return up
		}
	}
	return nil
}

// probeInterval is how often one cooling upstream may be asked anyway. Without
// it, a provider that answered "slow down" would be hit by every request.
const probeInterval = 30 * time.Second

// mayProbe reports whether a last-resort attempt on this upstream and model is
// allowed now, and remembers that it happened.
func (p *Proxy) mayProbe(up *config.Upstream, model string) bool {
	key := keyPoolIdentity(up) + "|" + modelalias.FamilyKey(model)
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	if p.lastProbe == nil {
		p.lastProbe = map[string]time.Time{}
	}
	now := time.Now()
	if last, ok := p.lastProbe[key]; ok && now.Sub(last) < probeInterval {
		return false
	}
	p.lastProbe[key] = now
	return true
}

// askedToWait reports a cooldown the provider itself asked for: a rate limit or
// a spent quota. Those are the one kind of cooldown a last-resort attempt must
// respect, because asking again is exactly what the provider said not to do.
func (p *Proxy) askedToWait(up *config.Upstream, model string) bool {
	keys := config.NormalizeAPIKeys(up.APIKeys)
	if authMode(up) != "swap" || len(keys) == 0 {
		keys = []string{""}
	}
	now := time.Now()
	// An explicit runtime key cooldown carries a Retry-After or a provider
	// refusal; it is never probed either.
	p.upstreamKeyMu.Lock()
	pool := p.upstreamKeys[up.ID]
	if pool != nil && pool.identity == keyPoolIdentity(up) {
		for _, key := range keys {
			if pool.cooldown[key].After(now) {
				p.upstreamKeyMu.Unlock()
				return true
			}
		}
	}
	p.upstreamKeyMu.Unlock()
	for _, key := range keys {
		for _, scope := range []string{"endpoint", "key", "model"} {
			state := up.HitchanceState[config.HitchanceTarget(scope, key, model)]
			if !config.HitchanceCooling(state, now) {
				continue
			}
			switch state.Category {
			case "limit", "quota", "rate_limit":
				return true
			}
		}
	}
	return false
}

// upstreamRateLimited reports that every credential on this upstream is
// currently resting after a rate limit. Such an upstream belongs at the end
// of a retry cycle while other routes can still be tried.
func (p *Proxy) upstreamRateLimited(up *config.Upstream) bool {
	if !p.hitchanceEnabled() || up == nil {
		return false
	}
	keys := config.NormalizeAPIKeys(up.APIKeys)
	if authMode(up) != "swap" || len(keys) == 0 {
		keys = []string{""}
	}
	now := time.Now()
	for _, key := range keys {
		limited := false
		for _, scope := range []string{"endpoint", "key"} {
			state := up.HitchanceState[config.HitchanceTarget(scope, key, "")]
			if state.Category == "rate_limit" && (state.Identity == "" || state.Identity == up.HitchanceIdentity()) && config.HitchanceCooling(state, now) {
				limited = true
				break
			}
		}
		if !limited {
			return false
		}
	}
	return true
}

// coolingUntil reports when any credential of this upstream could serve the
// model again: the zero time when one is ready now, and ok=false when nothing
// short of a health reset or a config change would help. With onlyRateLimits
// it counts only credentials whose rest is a rate limit.
func (p *Proxy) coolingUntil(up *config.Upstream, model string, onlyRateLimits bool) (time.Time, bool) {
	keys := config.NormalizeAPIKeys(up.APIKeys)
	if authMode(up) != "swap" || len(keys) == 0 {
		keys = []string{""}
	}
	var soonest time.Time
	found := false
	p.upstreamKeyMu.Lock()
	pool := p.upstreamKeys[up.ID]
	p.upstreamKeyMu.Unlock()
	for _, key := range keys {
		if up.KeyModelBlocked(key, model) {
			continue
		}
		until, ok := up.HitchanceReadyAt(key, model)
		if !ok {
			continue // quarantined: waiting cannot help
		}
		if pool != nil && pool.identity == keyPoolIdentity(up) {
			if poolUntil := pool.cooldown[key]; poolUntil.After(until) {
				until = poolUntil
			}
		}
		if onlyRateLimits && !restingAfterRateLimit(up, key, model, until) {
			continue
		}
		if !found || until.Before(soonest) {
			soonest, found = until, true
		}
	}
	return soonest, found
}

// restingAfterRateLimit reports that the rest ending at until is a rate limit,
// so the credential comes back by itself once it is over.
func restingAfterRateLimit(up *config.Upstream, key, model string, until time.Time) bool {
	for _, scope := range []string{"endpoint", "key", "model"} {
		s := up.HitchanceState[config.HitchanceTarget(scope, key, model)]
		if s.Category == "rate_limit" && s.Until.Equal(until) && (s.Identity == "" || s.Identity == up.HitchanceIdentity()) {
			return true
		}
	}
	return false
}

// lastResortKey marks a request that has nothing left to try: every upstream
// for the model is cooling down. Such an attempt looks past the cooldowns,
// because a cooldown is a guess about the future and the client would rather
// have a real attempt than an instant "unavailable".
type lastResortKey struct{}

func lastResort(r *http.Request) bool {
	if r == nil {
		return false
	}
	value, _ := r.Context().Value(lastResortKey{}).(bool)
	return value
}

func withLastResort(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), lastResortKey{}, true))
}

func (p *Proxy) hitchanceKeyReady(up *config.Upstream, key, model string, ignoreCooldown bool) bool {
	if upstreamModelBlocked(up, model) || up.KeyModelBlocked(key, model) {
		return false
	}
	if ignoreCooldown {
		return !up.HitchanceQuarantined(key, model)
	}
	// Disabling Hitchance ignores cooldowns, not deleted keys or disabled/
	// edited endpoints. Identity and membership checks still apply.
	if p.hitchanceEnabled() {
		now := time.Now()
		if !up.HitchanceReady(key, model, now) {
			return false
		}
		p.upstreamKeyMu.Lock()
		state := p.upstreamKeys[up.ID]
		cooling := state != nil && state.identity == keyPoolIdentity(up) && state.cooldown[key].After(now)
		p.upstreamKeyMu.Unlock()
		if cooling {
			return false
		}
	}
	if authMode(up) != "swap" || key == "" && len(up.APIKeys) == 0 {
		return key == ""
	}
	for _, current := range up.APIKeys {
		if current == key {
			return true
		}
	}
	return false
}

// Health is the outermost ordering tier. Stickiness and variant preference
// remain tie-breakers only among routes that can actually serve this request.
func (p *Proxy) hitchanceOrder(ups []*config.Upstream, model string, boost []string) []*config.Upstream {
	if !p.hitchanceEnabled() {
		return ups
	}
	ranks := map[string]int{}
	for _, up := range ups {
		if p.Health != nil {
			ranks[up.ID] = p.Health.FailureRank(up, model)
		}
		usable := false
		for _, raw := range p.combinedModelRoutes(up, model, boost) {
			keys := up.APIKeys
			if authMode(up) != "swap" || len(keys) == 0 {
				keys = []string{""}
			}
			for _, key := range keys {
				if !up.KeyModelBlocked(key, raw) && up.HitchanceReady(key, raw, time.Now()) {
					usable = true
					break
				}
			}
			if usable {
				break
			}
		}
		if !usable {
			ranks[up.ID] = 3
		}
	}
	sort.SliceStable(ups, func(i, j int) bool { return ranks[ups[i].ID] < ranks[ups[j].ID] })
	return ups
}
