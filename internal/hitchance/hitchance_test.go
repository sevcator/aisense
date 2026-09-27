package hitchance

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDefaultClassification(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		status                        int
		body, category, action, scope string
	}{
		{"invalid", 401, `{"error":{"message":"Your key is invalid"}}`, "auth", "delete", "key"},
		{"auth", 403, `{"error":{"message":"Authentication failed"}}`, "auth", "delete", "key"},
		{"billing wins over auth", 401, `{"error":{"type":"authentication_error","message":"quota is exceeded"}}`, "limit", "demote", "key"},
		{"paid model entitlement", 402, `{"error":{"type":"api_error","message":"this model is not included in your free usage, add usage credits to pay as you go or upgrade for included usage"}}`, "model", "demote", "model"},
		{"periodic quota", 429, `{"error":{"message":"您已达到每周/每月使用上限，限额将在稍后重置"}}`, "limit", "demote", "key"},
		{"credits", 400, `{"error":{"message":"not enough credits"}}`, "limit", "demote", "key"},
		{"payment", 402, `{}`, "limit", "demote", "key"},
		{"bare unauthorized", 401, `Unauthorized`, "auth", "demote", "key"},
		{"bare forbidden", 403, `Forbidden`, "auth", "demote", "key"},
		{"rate", 429, `{}`, "rate_limit", "demote", "key"},
		{"tpm text", 400, `{"error":{"message":"TPM limit reached, please slow down"}}`, "rate_limit", "demote", "key"},
		{"rpm text", 429, `{"error":{"message":"Rate limit reached for requests per minute (RPM)"}}`, "rate_limit", "demote", "key"},
		{"too many requests", 200, `{"error":{"message":"Too Many Requests"}}`, "", "", ""},
		{"quota on 429", 429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details"}}`, "limit", "demote", "key"},
		{"daily limit on 429", 429, `{"error":{"message":"daily request limit reached"}}`, "limit", "demote", "key"},
		{"provider credentials", 400, `{"error":{"message":"No active credentials for provider: forge"}}`, "model", "demote", "model"},
		// A relay reporting its own provider's failure cools only that model on our key.
		{"nested provider key", 502, `{"error":{"message":"upstream provider authentication failed: invalid api key"}}`, "model", "demote", "model"},
		{"model", 404, `{"error":{"code":"model_not_found"}}`, "model", "demote", "model"},
		{"model access", 403, `{"error":{"message":"model does not exist or you do not have access"}}`, "model", "demote", "model"},
		{"outage", 503, `{"error":{"message":"authentication failed"}}`, "outage", "demote", "endpoint"},
		{"bare not found", 404, `{}`, "outage", "demote", "endpoint"},
		{"quota text beats outage", 500, `{"error":{"message":"quota exceeded"}}`, "limit", "demote", "key"},
		{"client", 400, `{"error":{"message":"invalid request: messages must be an array"}}`, "", "", ""},
		{"ignore completions", 200, `{"choices":[{"message":{"content":"Your key is invalid"}}]}`, "", "", ""},
		{"ignore echoed content", 400, `{"error":{"message":"bad request"},"request":{"prompt":"Your key is invalid"}}`, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Classify(Default(), Input{Status: tc.status, Body: []byte(tc.body)})
			if d.Category != tc.category || d.Action != tc.action || d.Scope != tc.scope {
				t.Fatalf("decision=%+v want %s/%s/%s", d, tc.category, tc.action, tc.scope)
			}
		})
	}
}

func TestRetryAfterAndDisabled(t *testing.T) {
	d := Classify(Default(), Input{Status: 429, Header: http.Header{"Retry-After": []string{"120"}}})
	if d.Cooldown != 120*time.Second {
		t.Fatalf("retry-after=%v", d.Cooldown)
	}
	c := Default()
	c.Enabled = false
	if d := Classify(c, Input{Status: 401, Body: []byte("Your key is invalid")}); d.Action != "" {
		t.Fatal("disabled policy ran")
	}
}

func TestDefaultPolicyIsFourRules(t *testing.T) {
	c := Default()
	var ids []string
	for _, r := range c.Rules {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "model-unavailable,key-limit,api-base-down,key-rejected" {
		t.Fatalf("default rules = %s", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Rules = LegacyDefaultRules()
	if len(c.Rules) != 13 || c.Validate() != nil {
		t.Fatal("legacy default rules must stay valid for migration")
	}
}

func TestMatchAnyNeedsOneFilterAndDeleteNeedsText(t *testing.T) {
	c := Default()
	c.Rules = []Rule{
		{ID: "any", MatchAny: true, StatusCodes: []int{429}, Messages: []string{"slow down"}, Category: "limit", Action: "demote", Scope: "key", CooldownSeconds: 5},
		{ID: "wipe", MatchAny: true, StatusCodes: []int{401}, Messages: []string{"bad key"}, Category: "auth", Action: "delete", Scope: "key", CooldownSeconds: 5},
	}
	if d := Classify(c, Input{Status: 429}); d.RuleID != "any" || d.TextMatched {
		t.Fatalf("status alone: %+v", d)
	}
	if d := Classify(c, Input{Status: 400, Body: []byte(`{"error":{"message":"please slow down"}}`)}); d.RuleID != "any" || !d.TextMatched {
		t.Fatalf("text alone: %+v", d)
	}
	if d := Classify(c, Input{Status: 401, Body: []byte("Unauthorized")}); d.RuleID != "wipe" || d.Action != "demote" {
		t.Fatalf("a bare status must never delete a key: %+v", d)
	}
	if d := Classify(c, Input{Status: 400, Body: []byte(`{"error":{"message":"bad key"}}`)}); d.Action != "delete" {
		t.Fatalf("matching text deletes: %+v", d)
	}
	c.Rules[0].MatchAny = false
	if d := Classify(c, Input{Status: 429}); d.RuleID == "any" {
		t.Fatal("without match_any every filled filter must match")
	}
}
