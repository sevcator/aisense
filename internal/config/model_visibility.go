package config

import (
	"crypto/sha256"
	"fmt"

	"aisense/internal/modelalias"
)

func KeyFingerprint(key string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(key))) }

func (u *Upstream) KeyModelBlocked(key, raw string) bool {
	for _, blocked := range u.KeyBlockedModels[KeyFingerprint(key)] {
		if blocked == raw {
			return true
		}
	}
	return false
}

func (u *Upstream) ModelKeyAvailable(raw string) bool {
	keys := u.APIKeys
	if u.AuthMode == "none" || u.AuthMode == "oauth" || u.AuthMode == "passthrough" || len(keys) == 0 {
		keys = []string{""}
	}
	for _, key := range keys {
		if !u.KeyModelBlocked(key, raw) {
			return true
		}
	}
	return false
}

func (u *Upstream) ModelsVisible() bool { return u != nil && u.Enabled && !u.HiddenInvalid }

func (u *Upstream) ModelVisible(model string) bool {
	if u == nil {
		return false
	}
	for _, blocked := range u.BlockedModels {
		if blocked == "*" || modelalias.Matches(blocked, model) {
			return false
		}
	}
	for _, raw := range u.VisibleModelNames() {
		if raw == "*" || modelalias.Matches(raw, model) {
			return true
		}
	}
	return false
}

// Compute presentation candidates once per catalog, rather than rescanning the
// entire catalog for each listed model. Never mutate the operator's raw aliases.
func (u *Upstream) VisibleModelNames() []string {
	if !u.ModelsVisible() {
		return nil
	}
	keys := NormalizeAPIKeys(u.APIKeys)
	if u.AuthMode == "none" || u.AuthMode == "oauth" || u.AuthMode == "passthrough" || len(keys) == 0 {
		keys = []string{""}
	}
	blocks := make([]map[string]bool, len(keys))
	for i, key := range keys {
		blocks[i] = map[string]bool{}
		for _, raw := range u.KeyBlockedModels[KeyFingerprint(key)] {
			blocks[i][raw] = true
		}
	}
	blocked := func(model string) bool {
		for _, b := range u.BlockedModels {
			if b == "*" || modelalias.Matches(b, model) {
				return true
			}
		}
		return false
	}
	var models []string
	aliases := map[string][]string{}
	for _, model := range u.Models {
		if blocked(model) {
			continue
		}
		routes := u.ModelAliases[model]
		if len(routes) == 0 {
			routes = []string{model}
		}
		for _, raw := range routes {
			if blocked(raw) {
				continue
			}
			for _, keyBlocks := range blocks {
				if !keyBlocks[raw] {
					aliases[model] = append(aliases[model], raw)
					break
				}
			}
		}
		if len(aliases[model]) > 0 {
			models = append(models, model)
		}
	}
	return modelalias.ListingNames(models, aliases)
}
