package config

import (
	"fmt"
	"strings"

	"aisense/internal/modelalias"
)

// ModelCombo is a model name the user invents. A request for the combo walks
// its models in order and answers with the first one an upstream can serve, so
// the next model takes over as soon as one becomes unavailable.
type ModelCombo struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

const (
	maxCombos      = 64
	maxComboModels = 32
	maxComboName   = 80
)

func cloneCombos(combos []*ModelCombo) []*ModelCombo {
	if combos == nil {
		return nil
	}
	out := make([]*ModelCombo, 0, len(combos))
	for _, combo := range combos {
		if combo == nil {
			continue
		}
		copied := *combo
		copied.Models = append([]string(nil), combo.Models...)
		out = append(out, &copied)
	}
	return out
}

// NormalizeCombos trims every name and model list and drops empty entries and
// duplicate models, keeping the order the user gave them.
func NormalizeCombos(combos []*ModelCombo) []*ModelCombo {
	out := combos[:0]
	for _, combo := range combos {
		if combo == nil {
			continue
		}
		combo.Name = strings.TrimSpace(combo.Name)
		combo.Models = normalizeComboModels(combo.Models)
		if combo.Name == "" && len(combo.Models) == 0 {
			continue
		}
		out = append(out, combo)
	}
	return out
}

func normalizeComboModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[strings.ToLower(model)] {
			continue
		}
		seen[strings.ToLower(model)] = true
		out = append(out, model)
	}
	return out
}

// ValidateCombos keeps combo names usable as model names: present, unique,
// free of separators, and never standing for another combo.
func ValidateCombos(combos []*ModelCombo) error {
	if len(combos) > maxCombos {
		return fmt.Errorf("combos: at most %d combos", maxCombos)
	}
	names := map[string]bool{}
	for _, combo := range combos {
		names[strings.ToLower(strings.TrimSpace(combo.Name))] = true
	}
	for i, combo := range combos {
		name := strings.TrimSpace(combo.Name)
		where := fmt.Sprintf("combos[%d]", i+1)
		if name != "" {
			where = fmt.Sprintf("combos[%d] (%s)", i+1, name)
		}
		switch {
		case name == "":
			return fmt.Errorf("%s: name is required", where)
		case len(name) > maxComboName:
			return fmt.Errorf("%s: name must be at most %d characters", where, maxComboName)
		case strings.ContainsAny(name, " \t\r\n,"):
			return fmt.Errorf("%s: name must not contain spaces or commas", where)
		case modelalias.IsMetaName(name):
			return fmt.Errorf("%s: %s is a reserved model name", where, name)
		case modelalias.IsTierName(name):
			return fmt.Errorf("%s: %s is a reserved price-tier router name", where, name)
		case len(combo.Models) == 0:
			return fmt.Errorf("%s: add at least one model", where)
		case len(combo.Models) > maxComboModels:
			return fmt.Errorf("%s: at most %d models", where, maxComboModels)
		}
		for j, other := range combos {
			if j < i && strings.EqualFold(strings.TrimSpace(other.Name), name) {
				return fmt.Errorf("%s: another combo already uses this name", where)
			}
		}
		for _, model := range combo.Models {
			if names[strings.ToLower(model)] {
				return fmt.Errorf("%s: %s is a combo; a combo cannot contain another combo", where, model)
			}
		}
	}
	return nil
}

// ComboModels returns the models a combo name stands for, in the order they
// should be tried. It returns nil for every name that is not a combo.
func (c *Config) ComboModels(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" || c == nil {
		return nil
	}
	for _, combo := range c.Combos {
		if strings.EqualFold(strings.TrimSpace(combo.Name), name) {
			return append([]string(nil), combo.Models...)
		}
	}
	return nil
}

// ComboNames lists the combo names, for the model list clients are shown.
func (c *Config) ComboNames() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Combos))
	for _, combo := range c.Combos {
		if name := strings.TrimSpace(combo.Name); name != "" && len(combo.Models) > 0 {
			out = append(out, name)
		}
	}
	return out
}
