package hitchance

import "testing"

func TestHitchanceR1ValidationEvidence(t *testing.T) {
	for _, body := range []string{
		`{"error":{"type":"invalid_request_error","message":"invalid api key"}}`,
		`{"error":{"code":"validation_error","message":"authentication failed"}}`,
		`{"error":{"message":"Unknown parameter: invalid_api_key"}}`,
		`{"error":{"message":"Unsupported top-level field 'invalid_api_key'"}}`,
		`{"error":{"message":"Got an unexpected keyword argument 'invalid_api_key'"}}`,
		`{"error":{"message":"Parameter 'invalid_api_key' isn't supported"}}`,
		`{"error":{"message":"Unsupported field 'authentication failed'"}}`,
		`{"error":{"message":"Unrecognized request argument supplied: invalid_api_key"}}`,
		`{"error":{"message":"Additional properties are not allowed ('invalid_api_key' was unexpected)"}}`,
		`{"error":{"message":"Field invalid_api_key is not supported"}}`,
		`{"error":{"message":"Unknown request parameter","code":"invalid_api_key"}}`,
	} {
		for _, custom := range []bool{false, true} {
			c := Default()
			if custom {
				c.Rules = []Rule{{ID: "custom", Pattern: "(?i)(invalid.api.key|authentication failed)", Category: "invalid_key", Action: "delete", Scope: "key", CooldownSeconds: 60}}
			}
			d := Classify(c, Input{Status: 400, Body: []byte(body)})
			if d.Action != "ignore" {
				t.Errorf("custom=%v body=%s decision=%+v; want non-destructive terminal", custom, body, d)
			}
		}
	}
	for _, body := range []string{`{"error":{"code":"invalid_api_key","message":"Rejected"}}`, `{"error":{"type":"authentication_error","message":"Rejected"}}`, `{"error":{"message":"Your key is invalid"}}`, `{"error":{"message":"Authentication failed"}}`} {
		if d := Classify(Default(), Input{Status: 400, Body: []byte(body)}); d.Action != "delete" {
			t.Errorf("real auth lost: %s %+v", body, d)
		}
	}
	for _, status := range []int{401, 403} {
		if d := Classify(Default(), Input{Status: status}); d.Action == "delete" {
			t.Fatal("bare auth deleted")
		}
	}
}
