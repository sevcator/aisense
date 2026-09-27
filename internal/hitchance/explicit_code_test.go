package hitchance

import "testing"

func TestExplicitCodeOverridesGenericRequestType(t *testing.T) {
	for _, status := range []int{400, 401} {
		for _, tc := range []struct{ body, category, action string }{
			{`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided"}}`, "auth", "delete"},
			{`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Unknown field: invalid_api_key"}}`, "request", "ignore"},
			{`{"error":{"type":"invalid_request_error","code":"insufficient_quota","message":"authentication failed"}}`, "limit", "demote"},
		} {
			d := Classify(Default(), Input{Status: status, Body: []byte(tc.body)})
			if d.Category != tc.category || d.Action != tc.action {
				t.Errorf("status=%d body=%s decision=%+v", status, tc.body, d)
			}
		}
	}
}
