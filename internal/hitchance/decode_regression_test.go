package hitchance

import (
	"encoding/json"
	"testing"
)

func TestHitchanceMixedCaseFieldsRejectedWithoutInheritance(t *testing.T) {
	rule := `[{"id":"replacement","status_codes":[402],"category":"invalid_key","action":"delete","scope":"key","cooldown_seconds":10}]`
	for _, body := range []string{
		`{"Rules":` + rule + `}`, `{"RULES":` + rule + `}`, `{"rUlEs":` + rule + `}`,
		`{"rules":[],"Rules":` + rule + `}`,
		`{"Enabled":false}`, `{"INVALID_CONFIRMATIONS":2}`,
		`{"rules":[{"id":"custom","status_codes":[402],"category":"quota","Action":"demote","scope":"key","cooldown_seconds":10}]}`,
		`{"rules":[{"id":"custom","status_codes":[402],"category":"quota","action":"demote","scope":"key","cooldown_seconds":10,"Pattern":"old"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			base := Default()
			before, _ := json.Marshal(base)
			if err := json.Unmarshal([]byte(body), &base); err == nil {
				t.Fatal("noncanonical field accepted; previous rule fields can be inherited")
			}
			after, _ := json.Marshal(base)
			if string(before) != string(after) {
				t.Fatal("rejected partial decode mutated policy")
			}
			if _, err := Decode(Default(), []byte(body)); err == nil {
				t.Fatal("mixed-case policy accepted by validated decoder")
			}
		})
	}
}
