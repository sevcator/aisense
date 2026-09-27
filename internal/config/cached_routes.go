package config

import (
	"errors"
	"time"

	"aisense/internal/modelalias"
)

// CachedRoute is a successful model/upstream pair shown in the admin panel.
// Identity prevents a route from surviving an endpoint or auth-context edit.
type CachedRoute struct {
	Type       string    `json:"type"`
	Model      string    `json:"model"`
	UpstreamID string    `json:"upstream_id"`
	Identity   string    `json:"identity"`
	CachedAt   time.Time `json:"cached_at"`
}

var errCachedRoutesUnchanged = errors.New("cached routes unchanged")

func normalizeCachedRoutes(c *Config) {
	if len(c.CachedRoutes) == 0 {
		return
	}
	live := make(map[string]*Upstream, len(c.Upstreams))
	for _, up := range c.Upstreams {
		if up != nil && up.Enabled {
			live[up.ID] = up
		}
	}
	ttl := time.Duration(c.Hitchance.UpstreamCacheTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := time.Now()
	out := make([]CachedRoute, 0, len(c.CachedRoutes))
	indices := map[string]int{}
	for _, route := range c.CachedRoutes {
		up := live[route.UpstreamID]
		if up == nil || route.Identity != up.HitchanceIdentity() || now.Sub(route.CachedAt) > ttl || !up.ModelVisible(route.Model) {
			continue
		}
		key := route.Type + ":" + modelalias.FamilyKey(route.Model) + ":" + route.UpstreamID
		if i, exists := indices[key]; exists {
			if route.CachedAt.After(out[i].CachedAt) {
				out[i] = route
			}
			continue
		}
		indices[key] = len(out)
		out = append(out, route)
	}
	c.CachedRoutes = out
}

// RecordCachedRoute persists distinct working upstreams for a model. Repeated
// successes refresh a route at most once a minute, avoiding a config write for
// every request while keeping the dashboard useful after a restart.
func (m *Manager) RecordCachedRoute(snapshot *Upstream, typ, model string) error {
	if snapshot == nil || typ == "" || model == "" {
		return nil
	}
	err := m.Update(func(c *Config) error {
		current := map[string]*Upstream{}
		for _, up := range c.Upstreams {
			if up != nil && up.Enabled {
				current[up.ID] = up
			}
		}
		up := current[snapshot.ID]
		if up == nil || up.HitchanceIdentity() != snapshot.HitchanceIdentity() || !up.ModelVisible(model) {
			return errCachedRoutesUnchanged
		}
		now := time.Now()
		ttl := time.Duration(c.Hitchance.UpstreamCacheTTLHours) * time.Hour
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		changed, found := false, false
		routes := make([]CachedRoute, 0, len(c.CachedRoutes)+1)
		for _, route := range c.CachedRoutes {
			owner := current[route.UpstreamID]
			if owner == nil || route.Identity != owner.HitchanceIdentity() || now.Sub(route.CachedAt) > ttl || !owner.ModelVisible(route.Model) {
				changed = true
				continue
			}
			if route.UpstreamID == up.ID && route.Type == typ && modelalias.FamilyKey(route.Model) == modelalias.FamilyKey(model) {
				found = true
				if now.Sub(route.CachedAt) >= time.Minute {
					route.CachedAt = now
					route.Model = model
					changed = true
				}
			}
			routes = append(routes, route)
		}
		if !found {
			routes = append(routes, CachedRoute{Type: typ, Model: model, UpstreamID: up.ID, Identity: up.HitchanceIdentity(), CachedAt: now})
			changed = true
		}
		if !changed {
			return errCachedRoutesUnchanged
		}
		c.CachedRoutes = routes
		return nil
	})
	if errors.Is(err, errCachedRoutesUnchanged) {
		return nil
	}
	return err
}
