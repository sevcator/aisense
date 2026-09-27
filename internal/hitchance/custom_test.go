package hitchance

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCustomPolicyReplaceFiltersQuarantineAndLimits(t *testing.T) {
	c, err := Decode(Default(), []byte(`{"rules":[{"id":"local","upstream_ids":["u"],"models":["raw/m"],"status_codes":[403],"pattern":"(?i)subscription.*expired","category":"billing","action":"quarantine","scope":"key","cooldown_seconds":8}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 1 || len(c.Rules[0].Messages) != 0 {
		t.Fatal("replacement rule inherited old matchers")
	}
	in := Input{Status: 403, Body: []byte(`{"error":{"message":"Subscription has expired"}}`), UpstreamID: "u", Model: "raw/m"}
	if d := Classify(c, in); d.Action != "quarantine" {
		t.Fatal(d)
	}
	in.Model = "other"
	if d := Classify(c, in); d.Action != "" {
		t.Fatal("model filter ignored")
	}
	in.Model = "raw/m"
	in.UpstreamID = "other"
	if d := Classify(c, in); d.Action != "" {
		t.Fatal("upstream filter ignored")
	}
	c = Default()
	c.MaxCooldownSeconds = 100
	for _, value := range []string{"999999", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		d := Classify(c, Input{Status: 429, Header: http.Header{"Retry-After": []string{value}}})
		if d.Cooldown != 100*time.Second {
			t.Fatalf("unbounded Retry-After: %v", d)
		}
	}
	c.HonorRetryAfter = false
	if d := Classify(c, Input{Status: 402, Header: http.Header{"Retry-After": []string{"99"}}}); d.Cooldown != 60*time.Second {
		t.Fatal("ignore Retry-After not honored")
	}
	for _, bad := range []string{`{"rules":[{"id":"bad","action":"delete","scope":"key","category":"bad","status_codes":[403],"cooldown_seconds":1}]}`, `{"invalid_confirmations":0}`, `{"rules":[{"id":"bad","pattern":"[","action":"demote","scope":"key","category":"x","cooldown_seconds":1}]}`, `{"typo":true}`, `null`, `{} {}`} {
		if _, err := Decode(Default(), []byte(bad)); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestValidateNamesTheRuleAndField(t *testing.T) {
	for _, tc := range []struct{ rules, want string }{
		{`[{"id":"bad-re","pattern":"(unclosed","category":"x","action":"demote","scope":"key","cooldown_seconds":5}]`, "hitchance rule 1 (bad-re): pattern is not valid RE2: "},
		{`[{"id":"wipe","messages":["x"],"category":"x","action":"delete","scope":"endpoint","cooldown_seconds":5}]`, "hitchance rule 1 (wipe): only key scope can delete"},
		{`[{"id":"a","status_codes":[429],"category":"x","action":"demote","scope":"key","cooldown_seconds":5},{"id":"a","status_codes":[429],"category":"x","action":"demote","scope":"key","cooldown_seconds":5}]`, "hitchance rule 2 (a): id is already used by another rule"},
		{`[{"id":" ","status_codes":[429],"category":"x","action":"demote","scope":"key","cooldown_seconds":5}]`, "hitchance rule 1: id must be 1..80 characters"},
	} {
		_, err := Decode(Default(), []byte(`{"rules":`+tc.rules+`}`))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
	}
}
