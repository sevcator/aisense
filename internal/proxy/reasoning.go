package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"aisense/internal/config"
)

// Thinking-mode providers (MiniMax M2/M3 and similar relays) reject multi-turn
// requests whose assistant history lacks the reasoning_content their earlier
// answers carried: "The reasoning_content in the thinking mode must be passed
// back to the API." Chat clients routinely strip that field from stored
// history, so a conversation that worked once starts failing on the next
// turn. aisense bridges the gap: it remembers the reasoning_content of each
// answer per upstream/model/key, and once a provider has demanded the field
// it is injected back into follow-up requests before they are forwarded. A
// request rejected with that error is retried once with the cached value (or
// an empty string) before the 400 reaches the client.

const (
	reasoningCacheTTL    = 2 * time.Hour
	reasoningCacheMaxLen = 64 << 10
	// When no captured reasoning exists (proxy restart mid-conversation), a
	// presence-checking provider still needs a non-empty field.
	reasoningPlaceholder = "..."
)

type reasoningCacheEntry struct {
	value string
	at    time.Time
}

// reasoningBackfillNeeded reports whether an upstream error asks for the
// reasoning_content of earlier assistant messages to be passed back.
func reasoningBackfillNeeded(body []byte) bool {
	if len(body) == 0 || len(body) > 64<<10 {
		return false
	}
	if !bytes.Contains(bytes.ToLower(body), []byte("reasoning_content")) {
		return false
	}
	return bytes.Contains(bytes.ToLower(body), []byte("passed back"))
}

// extractReasoningContent pulls the assistant reasoning text out of an
// upstream response — a plain JSON completion or an SSE stream of deltas.
func extractReasoningContent(buf []byte) string {
	if len(buf) == 0 || len(buf) > 8<<20 {
		return ""
	}
	if buf[0] == '{' {
		return reasoningFromJSON(buf)
	}
	var out strings.Builder
	lines := bytes.Split(buf, []byte("\n"))
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
		if len(payload) == 0 || payload[0] == '[' {
			continue
		}
		out.WriteString(reasoningFromJSON(payload))
	}
	return out.String()
}

func reasoningFromJSON(b []byte) string {
	var envelope struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &envelope) != nil {
		return ""
	}
	for _, choice := range envelope.Choices {
		if choice.Delta.ReasoningContent != "" {
			return choice.Delta.ReasoningContent
		}
		if choice.Message.ReasoningContent != "" {
			return choice.Message.ReasoningContent
		}
	}
	return ""
}

// lastAssistantHasReasoning reports whether the request body's final
// assistant message already carries a non-empty reasoning_content.
func lastAssistantHasReasoning(body []byte) bool {
	messages, ok := decodeMessages(body)
	if !ok {
		return true // unfamiliar shape: never rewrite it
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "assistant") {
			rc, _ := msg["reasoning_content"].(string)
			return strings.TrimSpace(rc) != ""
		}
	}
	return true // no assistant message to backfill
}

// injectReasoningContent returns a copy of the request body with
// reasoning_content set on its last assistant message, or nil when the body
// is not a chat-completions payload.
func injectReasoningContent(body []byte, rc string) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	messages, ok := doc["messages"].([]any)
	if !ok {
		return nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "assistant") {
			msg["reasoning_content"] = rc
			out, err := json.Marshal(doc)
			if err != nil {
				return nil
			}
			return out
		}
	}
	return nil
}

func decodeMessages(body []byte) ([]any, bool) {
	var doc struct {
		Messages []any `json:"messages"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Messages == nil {
		return nil, false
	}
	return doc.Messages, true
}

// reasoningSlot identifies one conversation stream. The reasoning_content
// belongs to the conversation, not to the upstream that happened to serve the
// previous turn: the tier walk and health failover hop between upstreams, and
// every one of them must see the same reasoning their predecessor produced.
func (p *Proxy) reasoningSlot(model string, key *config.APIKey) string {
	keyID := ""
	if key != nil {
		keyID = key.ID
	}
	return model + "|" + keyID
}

func (p *Proxy) rememberReasoning(model string, key *config.APIKey, rc string) {
	if rc == "" {
		return
	}
	if len(rc) > reasoningCacheMaxLen {
		rc = rc[:reasoningCacheMaxLen]
	}
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	if p.reasoningCache == nil {
		p.reasoningCache = map[string]reasoningCacheEntry{}
	}
	p.reasoningCache[p.reasoningSlot(model, key)] = reasoningCacheEntry{value: rc, at: time.Now()}
}

func (p *Proxy) cachedReasoning(model string, key *config.APIKey) (string, bool) {
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	entry, ok := p.reasoningCache[p.reasoningSlot(model, key)]
	if !ok || time.Since(entry.at) > reasoningCacheTTL {
		return "", false
	}
	return entry.value, true
}

func (p *Proxy) markReasoningDemand(model string, key *config.APIKey) {
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	if p.reasoningDemand == nil {
		p.reasoningDemand = map[string]time.Time{}
	}
	p.reasoningDemand[p.reasoningSlot(model, key)] = time.Now()
}

// reasoningDemanded reports whether some provider recently rejected a request
// for missing reasoning_content. The demand flag never expires: a provider
// that enforces the field enforces it for every turn of every conversation.
func (p *Proxy) reasoningDemanded(model string, key *config.APIKey) bool {
	p.reasoningMu.Lock()
	defer p.reasoningMu.Unlock()
	_, ok := p.reasoningDemand[p.reasoningSlot(model, key)]
	return ok
}

// prepareReasoningBody returns the request body to forward: once a provider
// has demanded reasoning_content, follow-up requests missing it get the
// cached reasoning injected before they are sent.
func (p *Proxy) prepareReasoningBody(model string, key *config.APIKey, body []byte) []byte {
	if !p.reasoningDemanded(model, key) || lastAssistantHasReasoning(body) {
		return body
	}
	rc, ok := p.cachedReasoning(model, key)
	if !ok {
		return body
	}
	if fixed := injectReasoningContent(body, rc); fixed != nil {
		return fixed
	}
	return body
}

// retryWithReasoningBackfill re-sends a request rejected for missing
// reasoning_content with the captured reasoning injected, or a placeholder
// when nothing was captured (right after a proxy restart). The forward
// closure re-applies the model rewrite and resets the capture buffer. It
// reports the replacement result, or the originals when no retry applies;
// the caller captures the reasoning_content of a successful retry.
func (p *Proxy) retryWithReasoningBackfill(forward func([]byte) (*forwardResult, error), model string, key *config.APIKey, body []byte, fr *forwardResult, err error) (*forwardResult, error, bool) {
	if err != nil || fr == nil || fr.committed || fr.status != http.StatusBadRequest || !reasoningBackfillNeeded(fr.body) {
		return fr, err, false
	}
	rc, ok := p.cachedReasoning(model, key)
	if !ok {
		rc = reasoningPlaceholder
	}
	fixed := injectReasoningContent(body, rc)
	if fixed == nil {
		return fr, err, false
	}
	p.markReasoningDemand(model, key)
	retryFr, retryErr := forward(fixed)
	if retryErr != nil || retryFr == nil || !(retryFr.committed || (retryFr.status >= 200 && retryFr.status < 300)) {
		return fr, err, true // keep the original 400 for the client
	}
	return retryFr, retryErr, true
}
