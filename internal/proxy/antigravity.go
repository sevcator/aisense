package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aisense/internal/protocol"
)

// Antigravity-backed upstreams (raw routes prefixed ag/ or antigravity/)
// fingerprint unofficial clients from system prompt content, client headers,
// and tool names, then ban the account. Attempts routed to such routes are
// sanitized: system/developer instructions are stripped, tool names are
// replaced with neutral tool_1..tool_N placeholders (mapped back in the
// response), and client-identifying headers are removed. There is no config
// toggle: the behavior is tied strictly to the route prefix.

const officialUserAgent = "OpenAI/Python 1.99.0"

// isAntigravityRoute reports whether a per-attempt upstream model route
// targets an Antigravity-backed model. The check is case-insensitive on the
// raw route prefix.
func isAntigravityRoute(route string) bool {
	route = strings.ToLower(strings.TrimSpace(route))
	return strings.HasPrefix(route, "ag/") || strings.HasPrefix(route, "antigravity/")
}

// clientIdentifyingHeaders are stripped from Antigravity-bound attempts.
var clientIdentifyingHeaders = []string{
	"User-Agent", "X-Title", "Http-Referer", "Referer", "Origin",
	"X-Client", "X-Client-Name", "X-Client-Version", "X-Source",
	"X-Requested-With", "X-Package", "X-Request-Id", "X-Correlation-Id",
}

// scrubClientHeaders removes values upstreams use to fingerprint clients
// (including OpenAI SDK telemetry headers). Authentication headers are
// handled separately by the per-upstream auth mode and are never touched.
func scrubClientHeaders(h http.Header) {
	for _, name := range clientIdentifyingHeaders {
		h.Del(name)
	}
	for name := range h {
		if strings.HasPrefix(name, "X-Stainless-") {
			h.Del(name)
		}
	}
}

// antigravity carries the per-attempt tool-name mapping. The neutral mapping
// is order-stable: names are assigned tool_1..tool_N in the order first seen
// (tools array order, then tool_choice, then message history). Applying the
// sanitizer twice to the same payload is a no-op, so it can run both before
// protocol conversion and again on the converted payload.
type antigravity struct {
	neutral map[string]string // client tool name -> tool_N
	client  map[string]string // tool_N -> client tool name
	issued  int

	SystemStripped int
	ToolsRenamed   int
}

func newAntigravitySanitizer() *antigravity {
	return &antigravity{neutral: map[string]string{}, client: map[string]string{}}
}

// antigravitySanitizer decides whether one forwarding attempt is bound for an
// Antigravity-backed route. The route is the per-attempt upstream model (the
// request body model already rewritten by rewriteModelInBody, falling back to
// the query parameter). Only chat/messages operations are sanitized;
// embeddings and other operations are forwarded untouched.
func (p *Proxy) antigravitySanitizer(op string, r *http.Request, body []byte) (*antigravity, string) {
	if op != protocol.Chat && op != protocol.Messages {
		return nil, ""
	}
	route := modelFromBody(body)
	if route == "" {
		route = r.URL.Query().Get("model")
	}
	if !isAntigravityRoute(route) {
		return nil, ""
	}
	return newAntigravitySanitizer(), route
}

// neutralName returns the placeholder for a client tool name. Names that are
// already placeholders issued by this sanitizer are returned unchanged so
// repeated passes are idempotent even when a client tool is literally named
// like a placeholder.
func (a *antigravity) neutralName(name string) string {
	if name == "" {
		return name
	}
	if _, ok := a.client[name]; ok {
		return name
	}
	if n, ok := a.neutral[name]; ok {
		return n
	}
	a.issued++
	n := fmt.Sprintf("tool_%d", a.issued)
	a.neutral[name] = n
	a.client[n] = name
	a.ToolsRenamed++
	return n
}

func decodeJSONValue(b []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, false
	}
	return v, true
}

func marshalJSON(v any) ([]byte, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// sanitizeRequest strips system/developer instructions and neutralizes tool
// names in an outgoing chat payload. Both OpenAI chat shapes (messages roles
// system|developer, top-level system, function tools, tool_choice) and
// Anthropic Messages shapes (top-level system, tools, tool_choice, tool_use)
// are handled, so it is correct before and after protocol conversion.
// Payloads that are not JSON objects are returned unchanged.
func (a *antigravity) sanitizeRequest(body []byte) []byte {
	if a == nil {
		return body
	}
	root, ok := decodeJSONValue(body)
	if !ok {
		return body
	}
	m, ok := root.(map[string]any)
	if !ok {
		return body
	}
	changed := false
	if _, ok := m["system"]; ok {
		delete(m, "system")
		a.SystemStripped++
		changed = true
	}
	messages, _ := m["messages"].([]any)
	if len(messages) > 0 {
		kept := messages[:0]
		for _, v := range messages {
			msg, isMsg := v.(map[string]any)
			if !isMsg {
				kept = append(kept, v)
				continue
			}
			if role, _ := msg["role"].(string); role == "system" || role == "developer" {
				a.SystemStripped++
				changed = true
				continue
			}
			kept = append(kept, v)
		}
		m["messages"] = kept
	}
	if tools, ok := m["tools"].([]any); ok {
		for _, v := range tools {
			tool, _ := v.(map[string]any)
			if tool == nil {
				continue
			}
			if fn, ok := tool["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					fn["name"] = a.neutralName(name)
					changed = true
				}
				continue
			}
			if name, _ := tool["name"].(string); name != "" {
				tool["name"] = a.neutralName(name)
				changed = true
			}
		}
	}
	if choice, ok := m["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				fn["name"] = a.neutralName(name)
				changed = true
			}
		}
		if name, _ := choice["name"].(string); name != "" {
			choice["name"] = a.neutralName(name)
			changed = true
		}
	}
	for _, v := range messages {
		msg, _ := v.(map[string]any)
		if msg == nil {
			continue
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, c := range calls {
				call, _ := c.(map[string]any)
				if call == nil {
					continue
				}
				if fn, ok := call["function"].(map[string]any); ok {
					if name, _ := fn["name"].(string); name != "" {
						fn["name"] = a.neutralName(name)
						changed = true
					}
				}
			}
		}
		if blocks, ok := msg["content"].([]any); ok {
			for _, b := range blocks {
				block, _ := b.(map[string]any)
				if block == nil || block["type"] != "tool_use" {
					continue
				}
				if name, _ := block["name"].(string); name != "" {
					block["name"] = a.neutralName(name)
					changed = true
				}
			}
		}
	}
	if !changed {
		return body
	}
	next, ok := marshalJSON(m)
	if !ok {
		return body
	}
	return next
}

// restoreResponse maps neutral tool names in an upstream response body back
// to the client's names. Bodies that fail to parse (unknown response shapes)
// are returned byte-for-byte.
func (a *antigravity) restoreResponse(body []byte) []byte {
	if a == nil || len(a.client) == 0 {
		return body
	}
	root, ok := decodeJSONValue(body)
	if !ok {
		return body
	}
	if !a.restoreWalk(root) {
		return body
	}
	next, ok := marshalJSON(root)
	if !ok {
		return body
	}
	return next
}

// restoreWalk rewrites tool names in place. It recognizes every placement
// names take in both wire formats: any object carrying a function.name
// (chat tool_calls and tool_choice, including streaming deltas) and any
// tool_use block (Anthropic content and content_block_start events).
func (a *antigravity) restoreWalk(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				if orig, ok := a.client[name]; ok {
					fn["name"] = orig
					changed = true
				}
			}
		}
		if typ, _ := t["type"].(string); typ == "tool_use" {
			if name, _ := t["name"].(string); name != "" {
				if orig, ok := a.client[name]; ok {
					t["name"] = orig
					changed = true
				}
			}
		}
		for _, child := range t {
			if a.restoreWalk(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range t {
			if a.restoreWalk(child) {
				changed = true
			}
		}
	}
	return changed
}

// sseRemapper is a line-buffered, JSON-aware transformer for SSE streams. It
// accumulates bytes until line boundaries, parses each complete `data:` line
// carrying a JSON object, maps neutral tool names back to client names, and
// re-serializes the line. Anything unparseable (comments, event fields,
// [DONE], malformed payloads, unknown shapes) passes through byte-for-byte.
// Chunk boundaries may fall anywhere, including mid-line.
type sseRemapper struct {
	san     *antigravity
	pending []byte
}

// newSSERemapper returns nil when there is nothing to remap, disabling the
// transformer entirely for requests without tools.
func newSSERemapper(san *antigravity) *sseRemapper {
	if san == nil || len(san.client) == 0 {
		return nil
	}
	return &sseRemapper{san: san}
}

func (m *sseRemapper) transform(chunk []byte) []byte {
	if m == nil || len(chunk) == 0 {
		return chunk
	}
	m.pending = append(m.pending, chunk...)
	var out bytes.Buffer
	for {
		idx := bytes.IndexByte(m.pending, '\n')
		if idx < 0 {
			break
		}
		line := m.pending[:idx+1]
		m.pending = m.pending[idx+1:]
		out.Write(m.remapLine(line))
	}
	return out.Bytes()
}

// finish flushes a trailing partial line at end of stream.
func (m *sseRemapper) finish() []byte {
	if m == nil || len(m.pending) == 0 {
		return nil
	}
	rest := m.remapLine(m.pending)
	m.pending = nil
	return rest
}

func (m *sseRemapper) remapLine(line []byte) []byte {
	ending := []byte(nil)
	body := line
	if bytes.HasSuffix(body, []byte("\n")) {
		ending = []byte("\n")
		body = body[:len(body)-1]
		if bytes.HasSuffix(body, []byte("\r")) {
			ending = []byte("\r\n")
			body = body[:len(body)-1]
		}
	}
	start, payload := sseDataPayload(body)
	if payload == nil {
		return line
	}
	value, ok := decodeJSONValue(payload)
	if !ok {
		return line
	}
	if !m.san.restoreWalk(value) {
		return line
	}
	next, ok := marshalJSON(value)
	if !ok {
		return line
	}
	out := make([]byte, 0, start+len(next)+len(ending))
	out = append(out, body[:start]...)
	out = append(out, next...)
	return append(out, ending...)
}

// sseDataPayload recognizes `data:` fields per the SSE grammar: the field
// name, colon, then one optional leading space. Only lines whose payload
// begins with a JSON object are eligible for remapping; it returns the
// payload offset within line and the payload, or (0, nil) for every other
// line (comments, event/id fields, [DONE], arrays, scalars).
func sseDataPayload(line []byte) (int, []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return 0, nil
	}
	start := len("data:")
	payload := line[start:]
	if len(payload) > 0 && payload[0] == ' ' {
		start++
		payload = payload[1:]
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, nil
	}
	return start, payload
}
