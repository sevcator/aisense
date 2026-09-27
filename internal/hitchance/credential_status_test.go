package hitchance

import "testing"

// A provider that rejects the key with a 401 but tags it "invalid_request_error"
// (DeepSeek does exactly this) must not end the request: the model may still be
// served by another upstream. The evidence is too weak to delete the key, so it
// only cools down.
func TestRejectedKeyWithAGenericRequestCodeFailsOverInsteadOfAnswering(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Authentication Fails, Your api key: ****b345 is invalid","type":"authentication_error","code":"invalid_request_error","param":null}}`,
		`{"error":{"type":"invalid_request_error","message":"invalid api key"}}`,
		`{"error":{"type":"invalid_request","message":"api key not valid"}}`,
	} {
		d := Classify(Default(), Input{Status: 401, Body: []byte(body)})
		switch {
		case d.Action == "" || d.Action == "ignore":
			t.Errorf("the client would receive the upstream 401: %+v for %s", d, body)
		case d.Action == "delete":
			t.Errorf("this evidence must not delete a key: %+v for %s", d, body)
		case d.Scope != "key" || d.Cooldown == 0:
			t.Errorf("expected a short key cooldown: %+v for %s", d, body)
		}
	}
}

// A real request diagnostic is still the client's fault, whatever status it
// arrives with: the request ends instead of burning every upstream.
func TestFieldDiagnosticsStillEndTheRequest(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":{"type":"invalid_request_error","message":"Unknown parameter: 'reasoning_effort'"}}`},
		{401, `{"error":{"type":"invalid_request_error","message":"Unknown field: invalid_api_key"}}`},
		{422, `{"error":{"type":"validation_error","message":"extra inputs are not permitted"}}`},
		{429, `{"error":{"code":"unknown_parameter","message":"parameter top_k is not supported"}}`},
	} {
		d := Classify(Default(), Input{Status: tc.status, Body: []byte(tc.body)})
		if d.RuleID != "request-validation" || d.Action != "ignore" {
			t.Errorf("status %d: %+v for %s", tc.status, d, tc.body)
		}
	}
}

// Plain credential and limit answers keep their own rules.
func TestPlainCredentialAnswersKeepTheirRules(t *testing.T) {
	for _, tc := range []struct {
		status         int
		body           string
		rule, category string
	}{
		{401, `{"error":{"message":"invalid api key"}}`, "key-rejected", "auth"},
		{429, `{"error":{"message":"inference exceeds tpm/rpm limit","type":"rate_limit_error"}}`, "key-limit", "rate_limit"},
		{402, `{"error":{"message":"not enough credits"}}`, "key-limit", "limit"},
	} {
		d := Classify(Default(), Input{Status: tc.status, Body: []byte(tc.body)})
		if d.RuleID != tc.rule || d.Category != tc.category {
			t.Errorf("status %d: %+v", tc.status, d)
		}
	}
}
