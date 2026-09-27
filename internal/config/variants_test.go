package config

import (
	"encoding/json"
	"testing"
)

func TestVariantDefaultsAndRoundTrip(t *testing.T) {
	for _, input := range []string{`{}`, `{"models":{}}`, `{"models":{"fast_mode":true}}`, `{"models":{"reasoning_variants":false,"prefer_other_variants":true}}`} {
		var c Config
		if err := json.Unmarshal([]byte(input), &c); err != nil {
			t.Fatal(err)
		}
		o := c.Models.VariantOptions()
		if o.Reasoning == o.Other {
			t.Fatalf("defaults: %s %+v", input, o)
		}
		data, _ := json.Marshal(c)
		var next Config
		if err := json.Unmarshal(data, &next); err != nil {
			t.Fatal(err)
		}
		if next.Models.VariantOptions() != o {
			t.Fatal("round trip lost options")
		}
	}
}
