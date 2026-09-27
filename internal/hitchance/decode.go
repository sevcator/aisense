package hitchance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// UnmarshalJSON must replace rules, not reuse a prepopulated slice: otherwise
// omitted fields in rule N silently inherit the old rule N's match/action.
func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("hitchance must be an object")
	}
	for name := range fields {
		switch name {
		case "retry_cycles", "response_start_timeout_seconds", "upstream_cache_ttl_hours", "sticky_upstream_ttl_hours":
			if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
				return fmt.Errorf("hitchance.%s must be an integer", name)
			}
		case "enabled", "invalid_confirmations", "confirmation_window_seconds", "max_cooldown_seconds", "cooling_retry_seconds", "honor_retry_after", "rules":
		default:
			return fmt.Errorf("invalid hitchance field name")
		}
	}
	base := c.Clone()
	base.NormalizeScheduler()
	next := plain(base)
	if _, ok := fields["rules"]; ok {
		next.Rules = nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return fmt.Errorf("invalid hitchance configuration")
	}
	*c = Config(next)
	return nil
}

// encoding/json otherwise accepts case-insensitive field aliases. Require the
// documented names and decode each rule from scratch, even outside Config.
func (r *Rule) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("hitchance rule must be an object")
	}
	for name := range fields {
		switch name {
		case "id", "status_codes", "messages", "pattern", "match_any", "upstream_ids", "models", "category", "action", "scope", "cooldown_seconds":
		default:
			return fmt.Errorf("invalid hitchance rule field name")
		}
	}
	type plain Rule
	var next plain
	if err := json.Unmarshal(data, &next); err != nil {
		return fmt.Errorf("invalid hitchance rule")
	}
	*r = Rule(next)
	return nil
}

// Decode overlays a partial object on a deep copy and rejects misspelled fields.
func Decode(base Config, data []byte) (Config, error) {
	c := base.Clone()
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] != '{' {
		return c, fmt.Errorf("hitchance must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid hitchance configuration")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("invalid hitchance configuration")
	}
	return c, c.Validate()
}
