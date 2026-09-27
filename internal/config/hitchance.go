package config

import (
	"aisense/internal/hitchance"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
)

var errHitchanceUnchanged = errors.New("hitchance unchanged")

func HitchanceTarget(scope, key, model string) string {
	if scope == "endpoint" {
		return "endpoint"
	}
	if scope == "model" {
		return "model:" + KeyFingerprint(key) + ":" + model
	}
	return "key:" + KeyFingerprint(key)
}
func (u *Upstream) HitchanceIdentity() string {
	mode := u.AuthMode
	if mode == "" {
		mode = "swap"
	}
	b, _ := json.Marshal([]any{NormalizeBaseURL(u.BaseURL), mode, u.OAuth, u.UseProxy})
	return KeyFingerprint(string(b))
}

// EscalatedCooldown doubles the cooldown for every repeated failure in a row of the
// same rule, up to the policy's longest cooldown. A success clears the streak.
func EscalatedCooldown(base time.Duration, failures, maxSeconds int) time.Duration {
	limit := time.Duration(maxSeconds) * time.Second
	d := base
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	if limit > 0 && d > limit {
		d = limit
	}
	return d
}

func HitchanceCooling(s hitchance.State, now time.Time) bool {
	return s.Action == "quarantine" || s.Until.After(now)
}

// HitchanceQuarantined reports the states a last-resort attempt must still
// respect: quarantine is an explicit "until someone resets health", not a
// cooldown that expires on its own.
func (u *Upstream) HitchanceQuarantined(key, model string) bool {
	for _, scope := range []string{"endpoint", "key", "model"} {
		if s := u.HitchanceState[HitchanceTarget(scope, key, model)]; (s.Identity == "" || s.Identity == u.HitchanceIdentity()) && s.Action == "quarantine" {
			return true
		}
	}
	return false
}

// HitchanceReadyAt reports when this credential may serve the model again: the
// zero time when it is ready now, and ok=false when only a health reset can
// bring it back (quarantine).
func (u *Upstream) HitchanceReadyAt(key, model string) (time.Time, bool) {
	var until time.Time
	for _, scope := range []string{"endpoint", "key", "model"} {
		s := u.HitchanceState[HitchanceTarget(scope, key, model)]
		if s.Identity != "" && s.Identity != u.HitchanceIdentity() {
			continue
		}
		if s.Action == "quarantine" {
			return time.Time{}, false
		}
		if s.Until.After(until) {
			until = s.Until
		}
	}
	return until, true
}

func (u *Upstream) HitchanceReady(key, model string, now time.Time) bool {
	for _, scope := range []string{"endpoint", "key", "model"} {
		if s := u.HitchanceState[HitchanceTarget(scope, key, model)]; (s.Identity == "" || s.Identity == u.HitchanceIdentity()) && HitchanceCooling(s, now) {
			return false
		}
	}
	return true
}

// ObserveHitchance atomically changes only the matching endpoint/credential.
// A zero decision means proven inference success; it clears temporary failures.
// No raw credential or provider text is copied into health records.
func (m *Manager) ObserveHitchance(snapshot *Upstream, key, model string, d hitchance.Decision) error {
	if snapshot == nil || !m.Get().Hitchance.Enabled || d.Action == "ignore" {
		return nil
	}
	if d.Action == "" {
		hasState := false
		for _, up := range m.Get().Upstreams {
			if up.ID == snapshot.ID && len(up.HitchanceState) > 0 {
				hasState = true
				break
			}
		}
		if !hasState {
			return nil
		}
	}
	err := m.Update(func(c *Config) error {
		if !c.Hitchance.Enabled {
			return errHitchanceUnchanged
		}
		for _, up := range c.Upstreams {
			if up.ID != snapshot.ID || up.BaseURL != snapshot.BaseURL || up.AuthMode != snapshot.AuthMode || up.UseProxy != snapshot.UseProxy || !reflect.DeepEqual(up.OAuth, snapshot.OAuth) {
				continue
			}
			swapped := up.AuthMode == "swap" || up.AuthMode == ""
			if swapped {
				present := false
				for _, k := range up.APIKeys {
					if k == key {
						present = true
					}
				}
				if !present && key != "" {
					return errHitchanceUnchanged
				}
			}
			if d.Action == "" {
				changed := false
				for _, scope := range []string{"key", "model", "endpoint"} {
					target := HitchanceTarget(scope, key, model)
					if s, ok := up.HitchanceState[target]; ok && s == snapshot.HitchanceState[target] && s.Action != "quarantine" && s.Action != "delete" {
						delete(up.HitchanceState, target)
						changed = true
					}
				}
				if !changed {
					return errHitchanceUnchanged
				}
				return nil
			}
			if up.HitchanceState == nil {
				up.HitchanceState = map[string]hitchance.State{}
			}
			now := time.Now()
			// Bound historical model errors. Active quarantine records are never evicted.
			for k, s := range up.HitchanceState {
				if !HitchanceCooling(s, now) && now.Sub(s.UpdatedAt) > time.Duration(c.Hitchance.ConfirmationWindowSeconds)*time.Second {
					delete(up.HitchanceState, k)
				}
			}
			scope, action := d.Scope, d.Action
			if !swapped || key == "" {
				if scope == "key" {
					scope = "endpoint"
				}
				if action == "delete" {
					action = "demote"
				}
			}
			target := HitchanceTarget(scope, key, model)
			s := up.HitchanceState[target]
			previous := s
			if s.RuleID != d.RuleID || now.Sub(s.UpdatedAt) > time.Duration(c.Hitchance.ConfirmationWindowSeconds)*time.Second {
				s.Failures = 0
			}
			s.Identity = up.HitchanceIdentity()
			// Parallel requests that hit the same rate limit are one event, not a
			// streak: only a limit hit again after the rest ended counts as a repeat.
			if d.Category != "rate_limit" || previous.Category != "rate_limit" || !HitchanceCooling(previous, now) {
				s.Failures++
			}
			s.RuleID = d.RuleID
			s.Category = d.Category
			s.UpdatedAt = now
			maxSeconds := c.Hitchance.MaxCooldownSeconds
			if d.Category == "rate_limit" {
				// A rate limit counts per minute, so it never rests a key for longer
				// than that — or than the Retry-After the provider sent.
				maxSeconds = int(max(hitchance.RateLimitMaxCooldown, d.Cooldown) / time.Second)
				if limit := c.Hitchance.MaxCooldownSeconds; limit > 0 && maxSeconds > limit {
					maxSeconds = limit
				}
			}
			s.Until = now.Add(EscalatedCooldown(d.Cooldown, s.Failures, maxSeconds))
			s.Action = action
			if action == "delete" && s.Failures < c.Hitchance.InvalidConfirmations {
				s.Action = "demote"
			}
			// In-flight failures may arrive out of order. They can strengthen a
			// restriction, never release quarantine or shorten a Retry-After.
			if previous.Action == "quarantine" {
				s.Action = "quarantine"
			}
			if previous.Until.After(s.Until) {
				s.Until = previous.Until
			}
			if _, exists := up.HitchanceState[target]; !exists && len(up.HitchanceState) >= 512 {
				// Expired model histories cannot be key-deletion confirmations.
				// Reclaim them before widening enforcement at capacity.
				for k, old := range up.HitchanceState {
					if strings.HasPrefix(k, "model:") && !HitchanceCooling(old, now) {
						delete(up.HitchanceState, k)
					}
				}
				if len(up.HitchanceState) >= 512 {
					// No safe eviction remains. Fold all restrictions into an
					// explicit endpoint quarantine (manual reset), rather than
					// silently dropping this decision or releasing an active route.
					barrier := hitchance.State{Identity: up.HitchanceIdentity(), Category: "capacity_overflow", RuleID: "capacity-overflow", Action: "quarantine", Until: s.Until, UpdatedAt: now, Failures: 1}
					for _, old := range up.HitchanceState {
						if old.Until.After(barrier.Until) {
							barrier.Until = old.Until
						}
					}
					up.HitchanceState = map[string]hitchance.State{"endpoint": barrier}
					if target == "endpoint" {
						s = barrier
					}
				}
			}
			up.HitchanceState[target] = s
			if scope == "key" && swapped && key != "" {
				keys := make([]string, 0, len(up.APIKeys))
				for _, k := range up.APIKeys {
					if k != key {
						keys = append(keys, k)
					}
				}
				if s.Action != "delete" {
					keys = append(keys, key)
				} else {
					delete(up.KeyBlockedModels, KeyFingerprint(key))
				}
				up.APIKeys = keys
				if len(keys) == 0 && s.Action == "delete" {
					// The last stored credential was proven invalid. Keeping a
					// keyless swap upstream would turn it into a passthrough route
					// on the next request, so remove the exhausted upstream.
					for i, candidate := range c.Upstreams {
						if candidate == up {
							c.Upstreams = append(c.Upstreams[:i], c.Upstreams[i+1:]...)
							break
						}
					}
				}
			}
			return nil
		}
		return errHitchanceUnchanged
	})
	if errors.Is(err, errHitchanceUnchanged) {
		return nil
	}
	return err
}
