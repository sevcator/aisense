package hitchance

import (
	"encoding/json"
	"regexp"
	"strings"
)

var requestDiagnostic = regexp.MustCompile(`(?i)((unknown|unrecognized|unrecognised|unsupported|unexpected|not supported|not allowed|extra|invalid)[^\n]{0,80}\b(parameter|argument|field|propert|keyword)|\b(parameter|argument|field|propert|keyword)[^\n]{0,256}(unknown|unrecognized|unrecognised|unsupported|unexpected|not supported|not allowed|isn't supported|isn't allowed|not permitted)|additional properties|extra inputs are not permitted)`)

// HasErrorEnvelope recognizes bounded JSON or complete SSE error events.
// It shares parsing with ErrorText, so forwarding cannot disagree with policy
// about multiline data, malformed JSON, or request/completion echoes.
func HasErrorEnvelope(body []byte) bool {
	if len(body) > 64<<10 {
		return false
	}
	if payloads, sse := sseErrorPayloads(body); sse {
		return len(payloads) > 0
	}
	return jsonErrorEnvelope(body)
}

func jsonErrorEnvelope(body []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return false
	}
	var typ string
	_ = json.Unmarshal(obj["type"], &typ)
	e := strings.TrimSpace(string(obj["error"]))
	return e != "" && e != "null" || strings.EqualFold(typ, "error")
}

// An event must have a blank-line terminator. Do not promote a valid JSON
// fragment in an unfinished SSE event into destructive credential evidence.
func sseErrorPayloads(body []byte) ([][]byte, bool) {
	if len(body) > 64<<10 {
		return nil, true
	}
	var data []string
	var out [][]byte
	sse := false
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if i < len(lines)-1 {
				payload := []byte(strings.Join(data, "\n"))
				if jsonErrorEnvelope(payload) {
					out = append(out, payload)
				}
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			sse = true
			data = append(data, strings.TrimPrefix(line[5:], " "))
		} else if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") || strings.HasPrefix(line, ":") {
			sse = true
		}
	}
	return out, sse
}

// Codes/types are structured signals; messages can contain client-controlled
// names. Validation types and diagnostic phrasing override credential-like text.
func requestValidationEvidence(body []byte, text string) bool {
	return validationEvidence(body, text, false)
}

// strictValidationEvidence is the same reading for statuses that speak about
// the account (401, 402, 403, 429). There a bare "invalid_request_error" is how
// several providers spell "this key is rejected", so only a real diagnostic —
// a named field in the text, or a validation-specific code — still counts.
func strictValidationEvidence(body []byte, text string) bool {
	return validationEvidence(body, text, true)
}

func validationEvidence(body []byte, text string, strict bool) bool {
	if text == "" {
		return false
	}
	if requestDiagnostic.MatchString(text) {
		return true
	}
	if len(body) > 64<<10 {
		return false
	}
	if payloads, sse := sseErrorPayloads(body); sse {
		for _, payload := range payloads {
			if validationEvidence(payload, ErrorText(payload), strict) {
				return true
			}
		}
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	if raw, ok := obj["error"]; ok {
		if json.Unmarshal(raw, &obj) != nil {
			return false
		}
	}
	var code string
	_ = json.Unmarshal(obj["code"], &code)
	// Generic request types are wrappers, not concrete validation evidence.
	// Explicit credential/billing codes still pass through ordered policy rules.
	explicitCode := false
	switch strings.ToLower(code) {
	case "invalid_api_key", "authentication_error", "insufficient_quota":
		explicitCode = true
	}
	for _, field := range []string{"code", "type"} {
		var tag string
		_ = json.Unmarshal(obj[field], &tag)
		switch strings.ToLower(tag) {
		case "invalid_request_error", "invalid_request":
			if strict {
				continue // a rejected key is often spelled this way
			}
			if field != "type" || !explicitCode {
				return true
			}
		case "validation_error", "request_validation_error", "invalid_argument", "unknown_parameter", "unsupported_parameter", "unknown_field", "unsupported_field", "extra_forbidden":
			return true
		}
	}
	return false
}
