package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCombosAreTrimmedDedupedAndKeptInOrder(t *testing.T) {
	combos := NormalizeCombos([]*ModelCombo{
		{Name: "  favourite ", Models: []string{" kimi-k3 ", "glm-5.3", "GLM-5.3", "", "glm-5.3-flash"}},
		{Name: "   ", Models: []string{"  "}},
	})
	if len(combos) != 1 {
		t.Fatalf("combos = %d", len(combos))
	}
	if combos[0].Name != "favourite" {
		t.Fatalf("name = %q", combos[0].Name)
	}
	if got := strings.Join(combos[0].Models, ","); got != "kimi-k3,glm-5.3,glm-5.3-flash" {
		t.Fatalf("models = %q", got)
	}
	if err := ValidateCombos(combos); err != nil {
		t.Fatal(err)
	}
	if models := (&Config{Combos: combos}).ComboModels("Favourite"); strings.Join(models, ",") != "kimi-k3,glm-5.3,glm-5.3-flash" {
		t.Fatalf("lookup ignoring case = %v", models)
	}
	if models := (&Config{Combos: combos}).ComboModels("kimi-k3"); models != nil {
		t.Fatalf("a plain model name is not a combo: %v", models)
	}
}

func TestCombosRejectUnusableNamesAndNesting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		combos []*ModelCombo
		want   string
	}{
		{"no name", []*ModelCombo{{Models: []string{"m"}}}, "name is required"},
		{"spaces", []*ModelCombo{{Name: "my combo", Models: []string{"m"}}}, "spaces or commas"},
		{"reserved", []*ModelCombo{{Name: "auto", Models: []string{"m"}}}, "reserved model name"},
		{"no models", []*ModelCombo{{Name: "favourite"}}, "at least one model"},
		{"duplicate", []*ModelCombo{{Name: "favourite", Models: []string{"m"}}, {Name: "FAVOURITE", Models: []string{"m"}}}, "already uses this name"},
		{"nested", []*ModelCombo{{Name: "a", Models: []string{"m"}}, {Name: "b", Models: []string{"a"}}}, "cannot contain another combo"},
	} {
		err := ValidateCombos(NormalizeCombos(tc.combos))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestCombosSurviveSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	manager, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(func(c *Config) error {
		c.Combos = []*ModelCombo{{Name: "favourite", Models: []string{"kimi-k3", "glm-5.3"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A combo that cannot be used is refused before it reaches the file.
	if err := manager.Update(func(c *Config) error {
		c.Combos = append(c.Combos, &ModelCombo{Name: "broken"})
		return nil
	}); err == nil || !strings.Contains(err.Error(), "at least one model") {
		t.Fatalf("invalid combo err = %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if models := reloaded.Get().ComboModels("favourite"); strings.Join(models, ",") != "kimi-k3,glm-5.3" {
		t.Fatalf("after reload = %v", models)
	}
	if len(reloaded.Get().Combos) != 1 {
		t.Fatalf("combos = %+v", reloaded.Get().Combos)
	}
}
