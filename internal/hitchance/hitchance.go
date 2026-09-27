// Package hitchance classifies provider failures without storing response bodies.
package hitchance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RetryCycles                 int  `json:"retry_cycles"`
	ResponseStartTimeoutSeconds int  `json:"response_start_timeout_seconds"`
	UpstreamCacheTTLHours       int  `json:"upstream_cache_ttl_hours"`
	StickyUpstreamTTLHours      int  `json:"sticky_upstream_ttl_hours"`
	Enabled                     bool `json:"enabled"`
	InvalidConfirmations        int  `json:"invalid_confirmations"`
	ConfirmationWindowSeconds   int  `json:"confirmation_window_seconds"`
	MaxCooldownSeconds          int  `json:"max_cooldown_seconds"`
	// CoolingRetrySeconds is how long a request waits for a cooling upstream to
	// come back before it gives up — including a key that hit a rate limit
	// during this very request. When the wait is not enough it still tries
	// the cooling upstreams once, because a cooldown is a guess and an answer
	// beats "unavailable". Zero answers immediately, as before.
	CoolingRetrySeconds int    `json:"cooling_retry_seconds"`
	HonorRetryAfter     bool   `json:"honor_retry_after"`
	Rules               []Rule `json:"rules"`
}

// Filters are ANDed; alternatives inside a filter are ORed. First match wins.
// With MatchAny, any one of status codes, messages or pattern is enough;
// upstream and model filters still apply. Messages match case-insensitive
// substrings. Pattern uses Go's bounded RE2.
type Rule struct {
	ID              string   `json:"id"`
	StatusCodes     []int    `json:"status_codes,omitempty"`
	Messages        []string `json:"messages,omitempty"`
	Pattern         string   `json:"pattern,omitempty"`
	MatchAny        bool     `json:"match_any,omitempty"`
	UpstreamIDs     []string `json:"upstream_ids,omitempty"`
	Models          []string `json:"models,omitempty"`
	Category        string   `json:"category"`
	Action          string   `json:"action"`
	Scope           string   `json:"scope"`
	CooldownSeconds int      `json:"cooldown_seconds"`
}
type Input struct {
	Status            int
	Body              []byte
	Header            http.Header
	UpstreamID, Model string
}
type Decision struct {
	RuleID, Category, Action, Scope string
	Cooldown                        time.Duration
	// TextMatched reports that the rule matched the error's text (messages or
	// pattern), not only its HTTP status.
	TextMatched bool
}
type State struct {
	Identity  string    `json:"identity,omitempty"`
	Category  string    `json:"category"`
	RuleID    string    `json:"rule_id"`
	Action    string    `json:"action"`
	Until     time.Time `json:"until"`
	Failures  int       `json:"failures"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Default is four rules, one per outcome. Order matters: model and relay-provider
// errors never touch the key, quota text beats an auth error type, 5xx cools the
// API base, and only invalid-key text (not a bare 401/403) deletes a key.
// Repeated failures double each cooldown up to MaxCooldownSeconds.
func Default() Config {
	return Config{RetryCycles: 0, ResponseStartTimeoutSeconds: 20, UpstreamCacheTTLHours: 24, StickyUpstreamTTLHours: 24, Enabled: true, InvalidConfirmations: 1, ConfirmationWindowSeconds: 86400, MaxCooldownSeconds: 86400, CoolingRetrySeconds: 60, HonorRetryAfter: true, Rules: []Rule{
		{ID: "model-unavailable", MatchAny: true, Category: "model", Action: "demote", Scope: "model", CooldownSeconds: 300,
			Messages: []string{"model_not_found", "model not found", "unknown model", "unsupported model", "model_not_supported", "model retired", "model_retired", "model does not exist", "model unavailable", "model is not available", "no available channel"},
			Pattern:  `(?i)(model.*(not included|not allowed|access|permission|requires.*(credit|paid|subscription))|not included in your|do not have access|provider.*(credential|authentication|quota|unavailable|not configured)|no active credentials|upstream.*(invalid.*key|authentication failed))`},
		{ID: "key-limit", MatchAny: true, Category: "limit", Action: "demote", Scope: "key", CooldownSeconds: 60,
			StatusCodes: []int{402, 429},
			Messages:    []string{"not enough credits", "quota is exceeded", "insufficient_quota", "quota exceeded", "quota exhausted", "exceeded your current quota", "insufficient balance", "insufficient credits", "insufficient funds", "credit balance", "add credits", "billing hard limit", "payment required", "balance is too low", "额度不足", "余额不足", "rate limit", "rate_limit_exceeded", "tpm limit", "too many requests"},
			Pattern:     `(?i)((weekly|monthly).*(limit|quota)|每(周|月).*使用上限|每周/每月使用上限|限额将.*重置)`},
		{ID: "api-base-down", Category: "outage", Action: "demote", Scope: "endpoint", CooldownSeconds: 30,
			StatusCodes: []int{404, 405, 408, 425, 500, 501, 502, 503, 504, 507, 529}},
		{ID: "key-rejected", MatchAny: true, Category: "auth", Action: "delete", Scope: "key", CooldownSeconds: 300,
			StatusCodes: []int{401, 403},
			Messages:    []string{"your key is invalid", "invalid api key", "invalid_api_key", "incorrect api key", "api key not valid", "authentication failed", "authentication_error", "invalid authentication", "invalid token", "token is invalid", "api key has been revoked", "api key is expired", "invalid x-api-key"}},
	}}
}

// LegacyDefaultRules is the previous 13-rule default. Configs that still carry it
// unchanged are moved to the current Default rules on load.
func LegacyDefaultRules() []Rule {
	return []Rule{
		{ID: "model-entitlement", Pattern: `(?i)(model.*(not included|requires.*(credit|paid|subscription))|not included in your.*usage)`, Category: "permission", Action: "demote", Scope: "model", CooldownSeconds: 3600},
		{ID: "periodic-quota", Pattern: `(?i)((weekly|monthly).*(limit|quota)|每(周|月).*使用上限|每周/每月使用上限|限额将.*重置)`, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 86400},
		{ID: "billing", Messages: []string{"not enough credits", "quota is exceeded", "insufficient_quota", "quota exceeded", "quota exhausted", "exceeded your current quota", "insufficient balance", "insufficient credits", "insufficient funds", "credit balance", "add credits", "billing hard limit", "payment required", "balance is too low", "额度不足", "余额不足"}, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 3600},
		{ID: "payment-status", StatusCodes: []int{402}, Category: "quota", Action: "demote", Scope: "key", CooldownSeconds: 3600},
		{ID: "server-outage", StatusCodes: []int{500, 502, 503, 504, 529}, Category: "transient", Action: "demote", Scope: "endpoint", CooldownSeconds: 30},
		{ID: "provider-route", Pattern: `(?i)(provider.*(credential|authentication|quota|unavailable|not configured)|no active credentials|upstream.*(invalid.*key|authentication failed))`, Category: "model", Action: "demote", Scope: "model", CooldownSeconds: 300},
		{ID: "model-access", Pattern: `(?i)(model.*(access|permission|not included|not allowed)|not included in your|do not have access)`, Category: "permission", Action: "demote", Scope: "model", CooldownSeconds: 300},
		{ID: "invalid-credential", StatusCodes: []int{400, 401, 403}, Messages: []string{"your key is invalid", "invalid api key", "invalid_api_key", "incorrect api key", "api key not valid", "authentication failed", "authentication_error", "invalid authentication", "invalid token", "token is invalid", "api key has been revoked", "api key is expired", "invalid x-api-key"}, Category: "invalid_key", Action: "delete", Scope: "key", CooldownSeconds: 300},
		{ID: "model-route", Messages: []string{"model_not_found", "model not found", "unknown model", "unsupported model", "model_not_supported", "model retired", "model_retired", "model does not exist", "model unavailable", "model is not available", "no available channel"}, Category: "model", Action: "demote", Scope: "model", CooldownSeconds: 300},
		{ID: "rate-limit", StatusCodes: []int{429}, Category: "rate_limit", Action: "demote", Scope: "key", CooldownSeconds: 60},
		{ID: "rate-message", Messages: []string{"rate limit", "rate_limit_exceeded", "tpm limit", "too many requests"}, Category: "rate_limit", Action: "demote", Scope: "key", CooldownSeconds: 60},
		{ID: "unproven-auth", StatusCodes: []int{401, 403}, Category: "permission", Action: "demote", Scope: "key", CooldownSeconds: 300},
		{ID: "endpoint", StatusCodes: []int{404, 405, 408, 425, 500, 501, 502, 503, 504, 507, 529}, Category: "transient", Action: "demote", Scope: "endpoint", CooldownSeconds: 30},
	}
}

// NormalizeScheduler fills fields missing from older in-memory policies.
// A zero RetryCycles value means retries continue until the request is canceled.
func (c *Config) NormalizeScheduler() {
	if c.ResponseStartTimeoutSeconds == 0 {
		c.ResponseStartTimeoutSeconds = 20
	}
	if c.UpstreamCacheTTLHours == 0 {
		c.UpstreamCacheTTLHours = 24
	}
	if c.StickyUpstreamTTLHours == 0 {
		c.StickyUpstreamTTLHours = 24
	}
	if c.CoolingRetrySeconds == 0 {
		c.CoolingRetrySeconds = 60
	}
}

func (c Config) Validate() error {
	for _, f := range []struct {
		name       string
		value, max int
	}{
		{"response_start_timeout_seconds", c.ResponseStartTimeoutSeconds, 120},
		{"upstream_cache_ttl_hours", c.UpstreamCacheTTLHours, 720},
		{"sticky_upstream_ttl_hours", c.StickyUpstreamTTLHours, 720},
	} {
		if f.value < 1 || f.value > f.max {
			return fmt.Errorf("hitchance.%s must be 1..%d", f.name, f.max)
		}
	}
	if c.RetryCycles < 0 || c.RetryCycles > 10 {
		return fmt.Errorf("hitchance.retry_cycles must be 0..10")
	}
	if c.InvalidConfirmations < 1 || c.InvalidConfirmations > 20 {
		return fmt.Errorf("hitchance.invalid_confirmations must be 1..20")
	}
	if c.ConfirmationWindowSeconds < 1 || c.ConfirmationWindowSeconds > 2592000 {
		return fmt.Errorf("hitchance.confirmation_window_seconds must be 1..2592000")
	}
	if c.MaxCooldownSeconds < 1 || c.MaxCooldownSeconds > 2592000 {
		return fmt.Errorf("hitchance.max_cooldown_seconds must be 1..2592000")
	}
	if c.CoolingRetrySeconds < 0 || c.CoolingRetrySeconds > 120 {
		return fmt.Errorf("hitchance.cooling_retry_seconds must be 0..120")
	}
	if len(c.Rules) > 128 {
		return fmt.Errorf("hitchance allows at most 128 rules")
	}
	seen := map[string]bool{}
	for i, r := range c.Rules {
		// Name the rule and the broken field: the panel shows this text as is.
		fail := func(reason string) error {
			if id := strings.TrimSpace(r.ID); id != "" && len(r.ID) <= 80 {
				return fmt.Errorf("hitchance rule %d (%s): %s", i+1, id, reason)
			}
			return fmt.Errorf("hitchance rule %d: %s", i+1, reason)
		}
		if strings.TrimSpace(r.ID) == "" || len(r.ID) > 80 {
			return fail("id must be 1..80 characters")
		}
		if seen[r.ID] {
			return fail("id is already used by another rule")
		}
		seen[r.ID] = true
		if r.Action != "delete" && r.Action != "demote" && r.Action != "ignore" && r.Action != "quarantine" {
			return fail("action must be delete, demote, quarantine or ignore")
		}
		if r.Scope != "key" && r.Scope != "model" && r.Scope != "endpoint" {
			return fail("scope must be key, model or endpoint")
		}
		if len(r.Category) == 0 || len(r.Category) > 80 {
			return fail("category must be 1..80 characters")
		}
		if r.CooldownSeconds < 0 || r.CooldownSeconds > 2592000 {
			return fail("cooldown_seconds must be 0..2592000")
		}
		if r.Action != "ignore" && r.CooldownSeconds == 0 {
			return fail("cooldown_seconds must be at least 1 unless the action is ignore")
		}
		if len(r.StatusCodes) == 0 && len(r.Messages) == 0 && r.Pattern == "" {
			return fail("needs status_codes, messages or a pattern")
		}
		// Destructive rules require positive error content evidence, not just HTTP.
		if r.Action == "delete" && r.Scope != "key" {
			return fail("only key scope can delete")
		}
		if r.Action == "delete" && len(r.Messages) == 0 && r.Pattern == "" {
			return fail("delete needs messages or a pattern, not only status codes")
		}
		if len(r.Messages) > 128 || len(r.Models) > 128 || len(r.UpstreamIDs) > 128 || len(r.Pattern) > 2048 {
			return fail("at most 128 messages, models and upstream_ids, and a pattern of at most 2048 characters")
		}
		for _, s := range r.Messages {
			if strings.TrimSpace(s) == "" || len(s) > 2048 {
				return fail("messages must be non-blank and at most 2048 characters")
			}
		}
		for _, code := range r.StatusCodes {
			if code < 400 || code > 599 {
				return fail("status_codes must be 400..599")
			}
		}
		if r.Pattern != "" {
			if _, err := compilePattern(r.Pattern); err != nil {
				return fail("pattern is not valid RE2: " + err.Error())
			}
		}
	}
	return nil
}

// ErrorText extracts error fields only. Never classify echoed prompts, tool
// arguments, or successful completions as credential evidence. Input is bounded.
func ErrorText(body []byte) string {
	if len(body) > 64<<10 {
		return "" // Never turn a bounded/truncated structured response into text.
	}
	// Reuse the envelope parser used by forwarding. Incomplete or synthetic
	// SSE never falls back to plain text evidence.
	if payloads, sse := sseErrorPayloads(body); sse {
		var evidence []string
		for _, payload := range payloads {
			evidence = append(evidence, ErrorText(payload))
		}
		return strings.Join(evidence, " ")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil && obj != nil {
		var values []string
		var extract func(json.RawMessage, int)
		extract = func(b json.RawMessage, depth int) {
			if depth > 5 {
				return
			}
			var s string
			if json.Unmarshal(b, &s) == nil {
				values = append(values, s)
				return
			}
			var o map[string]json.RawMessage
			if json.Unmarshal(b, &o) != nil {
				return
			}
			for _, k := range []string{"error", "message", "detail", "title", "code", "type", "reason"} {
				if v, ok := o[k]; ok {
					extract(v, depth+1)
				}
			}
		}
		if e, ok := obj["error"]; ok {
			extract(e, 0)
		} else {
			for _, k := range []string{"message", "detail", "title", "code", "type"} {
				if v, ok := obj[k]; ok {
					extract(v, 0)
				}
			}
		}
		return strings.ToLower(strings.Join(values, " "))
	}
	text := strings.TrimSpace(string(body))
	// A truncated JSON document or an HTML login/WAF page is not proof of invalidity.
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "<") {
		return ""
	}
	return strings.ToLower(text)
}

// RateLimitCooldown is how long a key first rests after a rate limit that came
// without a Retry-After; repeats double it up to RateLimitMaxCooldown.
const (
	RateLimitCooldown    = 10 * time.Second
	RateLimitMaxCooldown = time.Minute
)

var (
	rateLimitTextRe = regexp.MustCompile(`(?i)(rate[ _-]?limit|too many requests|\b[rt]p[ms]\b|(tokens?|requests?) per (minute|second)|per[ _-]minute|throttl|slow down|too many concurrent|concurrency limit|请求过于频繁|频率|限流|速率)`)
	quotaTextRe     = regexp.MustCompile(`(?i)(quota|credit|balance|billing|payment|insufficient|funds|\b(daily|weekly|monthly)\b|per (day|week|month)|额度|余额|每(日|天|周|月)|使用上限|重置)`)
)

// RateLimitEvidence tells a rate limit (requests or tokens per minute, "too many
// requests") from a spent quota or balance: the first clears by itself within a
// minute, the second does not.
func RateLimitEvidence(status int, text string) bool {
	if status == http.StatusPaymentRequired || quotaTextRe.MatchString(text) {
		return false
	}
	return status == http.StatusTooManyRequests || rateLimitTextRe.MatchString(text)
}

// retryAfterSeconds reads a Retry-After header as whole seconds; 0 when absent.
func retryAfterSeconds(header http.Header) int {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if n, err := strconv.Atoi(value); err == nil && n > 0 {
		return n
	}
	if date, err := http.ParseTime(value); err == nil {
		if n := int(time.Until(date).Seconds()) + 1; n > 0 {
			return n
		}
	}
	return 0
}

// credentialOrLimitStatus reports the statuses that speak about the account,
// not about the shape of the request: authentication, permission and limits.
func credentialOrLimitStatus(status int) bool {
	switch status {
	case 401, 402, 403, 429:
		return true
	}
	return false
}

func contains[T comparable](list []T, value T) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
func Classify(c Config, in Input) Decision {
	if !c.Enabled || in.Status < 400 {
		return Decision{}
	}
	text := ErrorText(in.Body)
	// A provider's request diagnostic can echo arbitrary client field names.
	// This guard precedes even custom deletion rules, not just the defaults.
	// It never covers the credential and limit statuses: a provider that answers
	// 401 with "invalid_request_error" is rejecting the key, not the request, and
	// returning that to the client instead of trying the next upstream would
	// strand a model that other upstreams can still serve.
	if in.Status < 500 && requestValidationEvidence(in.Body, text) {
		// On a status that speaks about the account, a bare "invalid_request_error"
		// is how several providers spell "this key is rejected". The evidence is
		// too weak to delete the key, but handing the client that 401 while other
		// upstreams still serve the model is worse: cool this key down briefly and
		// let the request move on.
		if credentialOrLimitStatus(in.Status) && !strictValidationEvidence(in.Body, text) {
			return Decision{RuleID: "request-validation", Category: "request", Action: "demote", Scope: "key", Cooldown: time.Minute}
		}
		return Decision{RuleID: "request-validation", Category: "request", Action: "ignore", Scope: "key"}
	}
	for _, r := range c.Rules {
		if len(r.UpstreamIDs) > 0 && !contains(r.UpstreamIDs, in.UpstreamID) || len(r.Models) > 0 && !contains(r.Models, in.Model) {
			continue
		}
		statusHit := len(r.StatusCodes) > 0 && contains(r.StatusCodes, in.Status)
		messageHit := false
		for _, msg := range r.Messages {
			if strings.TrimSpace(msg) != "" && strings.Contains(text, strings.ToLower(msg)) {
				messageHit = true
				break
			}
		}
		patternHit := false
		if r.Pattern != "" {
			re, err := compilePattern(r.Pattern)
			patternHit = err == nil && re.MatchString(text)
		}
		if r.MatchAny {
			if !statusHit && !messageHit && !patternHit {
				continue
			}
		} else if len(r.StatusCodes) > 0 && !statusHit || len(r.Messages) > 0 && !messageHit || r.Pattern != "" && !patternHit {
			continue
		}
		action, scope := r.Action, r.Scope
		// Even operator rules cannot delete from a bare status or a server outage:
		// the rule's own error text must have matched.
		if action == "delete" && (scope != "key" || text == "" || !messageHit && !patternHit || in.Status >= 500) {
			action = "demote"
		}
		seconds, category := r.CooldownSeconds, r.Category
		retryAfter := 0
		if c.HonorRetryAfter {
			retryAfter = retryAfterSeconds(in.Header)
		}
		if retryAfter > seconds {
			seconds = retryAfter
		}
		// A per-minute limit clears by itself within the minute: it waits a short
		// while (or exactly as long as the provider asked), unlike a spent quota.
		if category == "limit" && RateLimitEvidence(in.Status, text) {
			category = "rate_limit"
			seconds = int(RateLimitCooldown / time.Second)
			if retryAfter > 0 {
				seconds = retryAfter
			}
		}
		if seconds > c.MaxCooldownSeconds {
			seconds = c.MaxCooldownSeconds
		}
		return Decision{RuleID: r.ID, Category: category, Action: action, Scope: scope, Cooldown: time.Duration(seconds) * time.Second, TextMatched: messageHit || patternHit}
	}
	return Decision{}
}
