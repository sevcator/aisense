package admin

import (
	"aisense/internal/config"
	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/routehealth"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

func (s *Server) hitchancePreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status     int             `json:"status"`
		Body       string          `json:"body"`
		RetryAfter string          `json:"retry_after"`
		UpstreamID string          `json:"upstream_id"`
		Model      string          `json:"model"`
		Policy     json.RawMessage `json:"policy"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Status < 100 || in.Status > 599 || len(in.Body) > 64<<10 {
		s.writeJSON(w, 400, map[string]string{"error": "invalid preview input (body limit 64 KiB)"})
		return
	}
	policy := s.Cfg.Get().Hitchance
	if len(in.Policy) > 0 {
		var err error
		policy, err = hitchance.Decode(policy, in.Policy)
		if err != nil {
			s.writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	d := hitchance.Classify(policy, hitchance.Input{Status: in.Status, Body: []byte(in.Body), Header: http.Header{"Retry-After": []string{in.RetryAfter}}, UpstreamID: in.UpstreamID, Model: in.Model})
	s.writeJSON(w, 200, map[string]any{"rule_id": d.RuleID, "category": d.Category, "action": d.Action, "scope": d.Scope, "cooldown_seconds": int(d.Cooldown.Seconds()), "mutated": false})
}
func (s *Server) hitchanceReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UpstreamID string `json:"upstream_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.UpstreamID == "" {
		s.writeJSON(w, 400, map[string]string{"error": "upstream_id required"})
		return
	}
	found := false
	var snapshot config.Upstream
	err := s.Cfg.Update(func(c *config.Config) error {
		for _, up := range c.Upstreams {
			if up.ID == in.UpstreamID {
				found = true
				snapshot = *up // reset exactly the identity whose state was persisted
				up.HitchanceState = nil
				return nil
			}
		}
		return fmt.Errorf("upstream not found")
	})
	if !found {
		s.writeJSON(w, 404, map[string]string{"error": "upstream not found"})
		return
	}
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": "could not persist reset"})
		return
	}
	s.healthManager().Reset(&snapshot)
	// Keep ProxyInfo backwards compatible; production wires this optional
	// synchronized key-pool reset through the main proxy adapter.
	if reset, ok := s.Proxy.(interface{ ResetHitchanceKeys(*config.Upstream) }); ok {
		reset.ResetHitchanceKeys(&snapshot)
	}
	s.writeJSON(w, 200, map[string]bool{"ok": true})
}

type hitchanceHealthRecord struct {
	routehealth.Record
	KeyCount       int                        `json:"key_count"`
	HitchanceState map[string]hitchance.State `json:"hitchance_state,omitempty"`
	// The endpoint (API base) and each API key are separate Hitchance scopes;
	// report them separately so the panel never attributes one to the other.
	EndpointState  *hitchance.State           `json:"endpoint_state,omitempty"`
	EndpointModels map[string]hitchance.State `json:"endpoint_models,omitempty"` // keyless upstreams only
	Keys           []hitchanceKeyHealth       `json:"keys,omitempty"`
	// Hit chance: the share of the recent attempts through the API base that succeeded.
	HitChance *float64 `json:"hit_chance,omitempty"`
	Attempts  int      `json:"attempts,omitempty"`
}

// hitchanceKeyHealth describes one API key by its position in the upstream's
// api_keys, so no credential or fingerprint leaves the server.
type hitchanceKeyHealth struct {
	Index         int                        `json:"index"`
	State         *hitchance.State           `json:"state,omitempty"`
	Models        map[string]hitchance.State `json:"models,omitempty"`
	BlockedModels int                        `json:"blocked_models,omitempty"`
	HitChance     *float64                   `json:"hit_chance,omitempty"`
	Attempts      int                        `json:"attempts,omitempty"`
}

// hitFields rounds a hit chance for the panel; no attempts means no figure.
func hitFields(rate routehealth.HitRate) (*float64, int) {
	if rate.Attempts == 0 {
		return nil, 0
	}
	chance := math.Round(rate.Chance*1000) / 1000
	return &chance, rate.Attempts
}

// Config.Manager already normalizes catalogs on publication. Index those raw
// routes once; do not rebuild the catalog or scan it for every credential/poll.
// Configured BlockedModels uses routing's family match, not exact raw matching.
func healthModelRoutes(up *config.Upstream) (map[string]bool, bool, bool) {
	routes := map[string]bool{}
	blocked := map[string]bool{}
	for _, model := range up.BlockedModels {
		if model == "*" {
			return routes, false, true
		}
		if family := modelalias.FamilyKey(model); family != "" {
			blocked[family] = true
		}
	}
	wildcard, restricted := false, false
	for _, model := range up.Models {
		if model == "*" {
			wildcard = true
			continue
		}
		if len(blocked) > 0 && blocked[modelalias.FamilyKey(model)] {
			restricted = true
			continue
		}
		rawRoutes := up.ModelAliases[model]
		if len(rawRoutes) == 0 {
			rawRoutes = []string{model}
		}
		for _, raw := range rawRoutes {
			if len(blocked) == 0 || !blocked[modelalias.FamilyKey(raw)] {
				routes[raw] = true
			} else {
				restricted = true
			}
		}
	}
	return routes, wildcard, restricted || wildcard && len(blocked) > 0
}

func (s *Server) hitchanceHealth() []hitchanceHealthRecord {
	cfg := s.Cfg.Get()
	records := s.healthManager().Snapshot(cfg.Upstreams)
	out := make([]hitchanceHealthRecord, 0, len(records))
	now := time.Now()
	for i, record := range records {
		up := cfg.Upstreams[i]
		normalizedKeys := config.NormalizeAPIKeys(up.APIKeys)
		rec := hitchanceHealthRecord{Record: record, KeyCount: len(normalizedKeys)}
		endpointRate, keyRates := s.healthManager().HitRates(up)
		rec.HitChance, rec.Attempts = hitFields(endpointRate)
		routes, wildcard, configuredRestriction := healthModelRoutes(up)
		swap := up.AuthMode == "" || up.AuthMode == "swap"
		if up.Enabled && swap && len(normalizedKeys) == 0 {
			rec.Status = "unavailable"
			rec.StatusReason = "No usable API keys are configured for swap authentication."
			out = append(out, rec)
			continue
		}
		if up.Enabled && len(routes) == 0 && !wildcard {
			rec.Status = "unavailable"
			rec.StatusReason = "No usable model routes are configured."
			out = append(out, rec)
			continue
		}
		if !up.Enabled {
			out = append(out, rec)
			continue
		}
		keys := normalizedKeys
		if !swap {
			keys = []string{""}
		}
		// Count sparse restrictions per effective credential. This is linear in
		// keys + catalog + recorded blocks, never the key×catalog product.
		keyBlocks := map[string]bool{}
		modelBlocks := map[string]map[string]bool{}
		keyIndex := map[string]int{}
		for i, key := range keys {
			fingerprint := config.KeyFingerprint(key)
			modelBlocks[fingerprint] = map[string]bool{}
			keyIndex[fingerprint] = i
		}
		for fingerprint, models := range up.KeyBlockedModels {
			if blocks, ok := modelBlocks[fingerprint]; ok {
				for _, raw := range models {
					if routes[raw] || wildcard {
						blocks[raw] = true
					}
				}
			}
		}
		if swap {
			rec.Keys = make([]hitchanceKeyHealth, len(keys))
			for fingerprint, i := range keyIndex {
				rec.Keys[i] = hitchanceKeyHealth{Index: i, BlockedModels: len(modelBlocks[fingerprint])}
				rec.Keys[i].HitChance, rec.Keys[i].Attempts = hitFields(keyRates[fingerprint])
			}
		}
		endpointRestricted, endpointQuarantined := false, false
		identity := up.HitchanceIdentity()
		for target, state := range up.HitchanceState {
			if !cfg.Hitchance.Enabled || (state.Identity != "" && state.Identity != identity) || !config.HitchanceCooling(state, now) {
				continue
			}
			parts := strings.SplitN(target, ":", 3)
			switch {
			case target == "endpoint":
				endpointRestricted = true
				endpointQuarantined = state.Action == "quarantine"
				if !endpointQuarantined && (rec.CooldownUntil == nil || state.Until.After(*rec.CooldownUntil)) {
					until := state.Until
					rec.CooldownUntil = &until
				}
				endpoint := state
				rec.EndpointState = &endpoint
			case len(parts) == 2 && parts[0] == "key":
				if _, ok := modelBlocks[parts[1]]; !ok {
					continue
				}
				keyBlocks[parts[1]] = true
				if swap {
					key := state
					rec.Keys[keyIndex[parts[1]]].State = &key
				}
			case len(parts) == 3 && parts[0] == "model":
				blocks, ok := modelBlocks[parts[1]]
				if !ok || parts[2] == "" || (!routes[parts[2]] && !wildcard) {
					continue
				}
				blocks[parts[2]] = true
				models := &rec.EndpointModels
				if swap {
					models = &rec.Keys[keyIndex[parts[1]]].Models
				}
				if *models == nil {
					*models = map[string]hitchance.State{}
				}
				(*models)[parts[2]] = state
			default:
				continue
			}
			if rec.HitchanceState == nil {
				rec.HitchanceState = map[string]hitchance.State{}
			}
			rec.HitchanceState[target] = state
			if !state.UpdatedAt.IsZero() && (rec.LastFailureAt == nil || state.UpdatedAt.After(*rec.LastFailureAt)) {
				at := state.UpdatedAt
				rec.LastFailureAt = &at
				rec.LastError = "hitchance: " + state.Category
			}
		}
		usable, restricted := 0, configuredRestriction
		for fingerprint, blocks := range modelBlocks {
			if keyBlocks[fingerprint] || len(blocks) > 0 {
				restricted = true
			}
			if !keyBlocks[fingerprint] && (wildcard || len(blocks) < len(routes)) {
				usable++
			}
		}
		switch {
		case endpointQuarantined:
			rec.Status, rec.StatusReason = "unavailable", "Endpoint is quarantined until health reset."
			rec.CooldownUntil = nil // A timestamp cannot promise release from quarantine.
		case endpointRestricted:
			rec.Status, rec.StatusReason = "unavailable", "Endpoint failure cooldown is active."
		case rec.Status == "unavailable":
			// A runtime endpoint failure is not downgraded by a partial key block.
		case len(keyBlocks) == len(modelBlocks):
			rec.Status, rec.StatusReason = "unavailable", "All effective credentials are restricted."
		case usable == 0:
			rec.Status, rec.StatusReason = "unavailable", "All configured model routes are restricted."
		case restricted:
			rec.Status, rec.StatusReason = "degraded", "Some credentials or model routes are restricted."
		}
		out = append(out, rec)
	}
	return out
}
