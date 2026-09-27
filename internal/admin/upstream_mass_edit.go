package admin

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aisense/internal/config"
	"aisense/internal/modelalias"
)

type stringListEdit struct {
	Mode   string   `json:"mode"`
	Values []string `json:"values"`
}

type upstreamMassEditPatch struct {
	Models        *stringListEdit `json:"models,omitempty"`
	BlockedModels *stringListEdit `json:"blocked_models,omitempty"`
	Type          *string         `json:"type,omitempty"`
	BaseURL       *string         `json:"base_url,omitempty"`
	APIKeys       *[]string       `json:"api_keys,omitempty"`
	LegacyAPIKey  *string         `json:"api_key,omitempty"`
	Priority      *int            `json:"priority,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	FailCodes     *[]int          `json:"fail_codes,omitempty"`
	AuthMode      *string         `json:"auth_mode,omitempty"`
	UseProxy      *bool           `json:"use_proxy,omitempty"`
}

type upstreamMassEditRequest struct {
	IDs   []string              `json:"ids"`
	All   bool                  `json:"all"`
	Patch upstreamMassEditPatch `json:"patch"`
}

func (p upstreamMassEditPatch) empty() bool {
	return p.Models == nil && p.BlockedModels == nil && p.Type == nil && p.BaseURL == nil &&
		p.APIKeys == nil && p.LegacyAPIKey == nil && p.Priority == nil && p.Enabled == nil && p.FailCodes == nil &&
		p.AuthMode == nil && p.UseProxy == nil
}

func applyUpstreamMassEdit(up *config.Upstream, patch upstreamMassEditPatch) error {
	var err error
	if patch.Models != nil {
		up.Models, up.ModelAliases, err = applyModelListEdit(up.Models, up.ModelAliases, patch.Models)
		if err != nil {
			return fmt.Errorf("models: %w", err)
		}
		if len(up.Models) == 0 {
			up.Models = []string{"*"}
		}
	}
	if patch.BlockedModels != nil {
		up.BlockedModels, err = applyStringListEdit(up.BlockedModels, patch.BlockedModels)
		if err != nil {
			return fmt.Errorf("blocked_models: %w", err)
		}
	}
	if patch.Type != nil {
		return errors.New("type is a read-only legacy hint; protocol detection is automatic")
	}
	if patch.BaseURL != nil {
		baseURL := strings.TrimRight(strings.TrimSpace(*patch.BaseURL), "/")
		parsed, parseErr := url.Parse(baseURL)
		if baseURL == "" || parseErr != nil || parsed.Scheme == "" || parsed.Host == "" || config.ValidateBaseURL(baseURL) != nil {
			return errors.New("base_url must be a valid absolute URL")
		}
		up.BaseURL = baseURL
	}
	if patch.APIKeys != nil {
		up.APIKeys = config.NormalizeAPIKeys(*patch.APIKeys)
	} else if patch.LegacyAPIKey != nil {
		up.APIKeys = config.NormalizeAPIKeys([]string{*patch.LegacyAPIKey})
	}
	if patch.Priority != nil {
		if *patch.Priority < 0 {
			return errors.New("priority must be zero or greater")
		}
		up.Priority = *patch.Priority
	}
	if patch.Enabled != nil {
		up.Enabled = *patch.Enabled
	}
	if patch.FailCodes != nil {
		codes := make([]int, 0, len(*patch.FailCodes))
		seen := map[int]bool{}
		for _, code := range *patch.FailCodes {
			if code < 100 || code > 599 {
				return fmt.Errorf("invalid fail code %d", code)
			}
			if !seen[code] {
				seen[code] = true
				codes = append(codes, code)
			}
		}
		up.FailCodes = codes
	}
	if patch.AuthMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*patch.AuthMode))
		switch mode {
		case "swap", "passthrough", "none", "oauth":
			up.AuthMode = mode
		default:
			return errors.New("auth_mode must be swap, passthrough, none or oauth")
		}
	}
	if patch.UseProxy != nil {
		up.UseProxy = *patch.UseProxy
	}
	return nil
}

func applyModelListEdit(current []string, aliases map[string][]string, edit *stringListEdit) ([]string, map[string][]string, error) {
	if edit == nil {
		return append([]string(nil), current...), config.CloneModelAliases(aliases), nil
	}
	mode := strings.ToLower(strings.TrimSpace(edit.Mode))
	if mode == "" {
		mode = "replace"
	}
	values := normalizeStringList(edit.Values)
	switch mode {
	case "replace":
		catalog := modelalias.Build(values, nil)
		return catalog.Models, catalog.Aliases, nil
	case "add":
		combinedAliases := config.CloneModelAliases(aliases)
		if combinedAliases == nil {
			combinedAliases = map[string][]string{}
		}
		newModels := append([]string(nil), current...)
		for _, value := range values {
			matched := false
			for _, canonical := range current {
				if canonical != "*" && modelalias.Matches(canonical, value) {
					combinedAliases[canonical] = append(combinedAliases[canonical], value)
					matched = true
					break
				}
			}
			if !matched {
				newModels = append(newModels, value)
			}
		}
		catalog := modelalias.Build(newModels, combinedAliases)
		return catalog.Models, catalog.Aliases, nil
	case "remove":
		remove := map[string]bool{}
		for _, value := range values {
			remove[modelalias.FamilyKey(value)] = true
		}
		kept := make([]string, 0, len(current))
		keptAliases := map[string][]string{}
		for _, model := range current {
			if model == "*" || !remove[modelalias.FamilyKey(model)] {
				kept = append(kept, model)
				if raw := aliases[model]; len(raw) > 0 {
					keptAliases[model] = append([]string(nil), raw...)
				}
			}
		}
		catalog := modelalias.Build(kept, keptAliases)
		return catalog.Models, catalog.Aliases, nil
	default:
		return nil, nil, errors.New("mode must be replace, add or remove")
	}
}

func applyStringListEdit(current []string, edit *stringListEdit) ([]string, error) {
	if edit == nil {
		return append([]string(nil), current...), nil
	}
	mode := strings.ToLower(strings.TrimSpace(edit.Mode))
	if mode == "" {
		mode = "replace"
	}
	values := normalizeStringList(edit.Values)
	switch mode {
	case "replace":
		return values, nil
	case "add":
		return normalizeStringList(append(append([]string(nil), current...), values...)), nil
	case "remove":
		remove := map[string]bool{}
		for _, value := range values {
			remove[value] = true
		}
		out := make([]string, 0, len(current))
		for _, value := range normalizeStringList(current) {
			if !remove[value] {
				out = append(out, value)
			}
		}
		return out, nil
	default:
		return nil, errors.New("mode must be replace, add or remove")
	}
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
