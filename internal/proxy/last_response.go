package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"aisense/internal/debuglog"
)

var lastResponseIdle = time.Minute

type pendingResponse struct {
	timer *time.Timer
	seq   uint64
}

// conversationID uses the first user message as a stable root across chat
// turns. For APIs that send only the latest input, the client and model form
// the idle group unless the request supplies a conversation identifier. No
// prompt or credential is retained in the pending-response map.
func conversationID(body []byte, clientKey string) string {
	var request struct {
		Model        string            `json:"model"`
		Messages     []json.RawMessage `json:"messages"`
		Conversation json.RawMessage   `json:"conversation"`
	}
	_ = json.Unmarshal(body, &request)
	root := []byte(request.Model)
	if len(request.Messages) > 0 {
		first := request.Messages[0]
		for _, message := range request.Messages {
			var role struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(message, &role) == nil && role.Role == "user" {
				first = message
				break
			}
		}
		root = append(root, first...)
	} else if len(request.Conversation) > 0 {
		root = append(root, request.Conversation...)
	}
	sum := sha256.Sum256(append([]byte(clientKey+"\x00"), root...))
	return hex.EncodeToString(sum[:])
}

func (p *Proxy) touchConversation(id string) uint64 {
	p.lastResponseMu.Lock()
	defer p.lastResponseMu.Unlock()
	if p.lastResponses == nil {
		p.lastResponses = make(map[string]pendingResponse)
	}
	entry := p.lastResponses[id]
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.seq++
	entry.timer = nil
	p.lastResponses[id] = entry
	return entry.seq
}

func (p *Proxy) queueLastResponse(id string, seq uint64, status int, contentType string, body []byte) {
	if p.Debug == nil || !p.Debug.LastResponseOnly() || !p.Debug.Enabled() || status < 200 || status >= 300 || len(body) == 0 {
		p.lastResponseMu.Lock()
		if current, ok := p.lastResponses[id]; ok && current.seq == seq {
			delete(p.lastResponses, id)
		}
		p.lastResponseMu.Unlock()
		return
	}
	p.lastResponseMu.Lock()
	entry, ok := p.lastResponses[id]
	if !ok || entry.seq != seq {
		p.lastResponseMu.Unlock()
		return
	}
	entry.timer = time.AfterFunc(lastResponseIdle, func() {
		p.lastResponseMu.Lock()
		defer p.lastResponseMu.Unlock()
		current, exists := p.lastResponses[id]
		if !exists || current.seq != seq {
			return
		}
		delete(p.lastResponses, id)
		p.Debug.Event("", "conversation.last_response", map[string]any{
			"status": status, "body": debuglog.Body(body, contentType),
		})
	})
	p.lastResponses[id] = entry
	p.lastResponseMu.Unlock()
}
