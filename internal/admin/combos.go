package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"aisense/internal/config"
)

// upsertCombo saves one combo. The optional original name renames an existing
// combo; without it the name decides whether this creates or replaces one.
func (s *Server) upsertCombo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string   `json:"name"`
		Models   []string `json:"models"`
		Original string   `json:"original_name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": "invalid combo"})
		return
	}
	combo := &config.ModelCombo{Name: strings.TrimSpace(in.Name), Models: in.Models}
	original := strings.TrimSpace(in.Original)
	if original == "" {
		original = combo.Name
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		for i, existing := range c.Combos {
			if strings.EqualFold(strings.TrimSpace(existing.Name), original) {
				c.Combos[i] = combo
				return nil
			}
		}
		c.Combos = append(c.Combos, combo)
		return nil
	})
	if err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "saved"})
}

func (s *Server) deleteCombo(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	removed := false
	err := s.Cfg.Update(func(c *config.Config) error {
		out := c.Combos[:0]
		for _, combo := range c.Combos {
			if strings.EqualFold(strings.TrimSpace(combo.Name), name) {
				removed = true
				continue
			}
			out = append(out, combo)
		}
		c.Combos = out
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "combo not found"})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "deleted"})
}
