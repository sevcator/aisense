// Package protocol resolves wire operations independently of persisted upstream identity.
package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const Chat = "chat/completions"
const Messages = "messages"

func Format(op string) string {
	if op == Messages {
		return "anthropic"
	}
	return "openai"
}

// Endpoint replaces known full endpoint suffixes rather than appending to them.
// A bare origin uses /v1; explicit gateway prefixes are preserved.
func Endpoint(base, op string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return base
	}
	p := strings.TrimRight(u.Path, "/")
	full := false
	for _, suffix := range []string{Chat, Messages, "responses", "embeddings", "models"} {
		if strings.HasSuffix(p, "/"+suffix) {
			p = strings.TrimSuffix(p, "/"+suffix)
			full = true
			break
		}
	}
	if p == "" && !full {
		p = "/v1"
	}
	u.Path = p + "/" + strings.TrimLeft(op, "/")
	u.RawPath = ""
	return u.String()
}

func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type Evidence struct {
	Supported   bool   `json:"supported"`
	Unsupported bool   `json:"unsupported"`
	Status      int    `json:"status,omitempty"`
	Source      string `json:"source"`
}
type entry struct {
	evidence Evidence
	until    time.Time
}
type Resolver struct {
	mu      sync.Mutex
	cache   map[[32]byte]entry
	pending map[[32]byte]chan struct{}
}

var Shared Resolver
var probeSlots = make(chan struct{}, 8)

// Identity must include the effective credentials and transport configuration.
// Only hashes are retained. Type is deliberately not a persisted detection result.
func cacheKey(identity, op string) [32]byte { return sha256.Sum256([]byte(identity + "\x00" + op)) }
func (r *Resolver) put(identity, op string, e Evidence, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = make(map[[32]byte]entry)
	}
	key := cacheKey(identity, op)
	if current, ok := r.cache[key]; ok && current.evidence.Source == "success" && e.Source != "success" && time.Now().Before(current.until) {
		return
	}
	if len(r.cache) >= 2048 {
		for k, v := range r.cache {
			if time.Now().After(v.until) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= 2048 {
			for k := range r.cache {
				delete(r.cache, k)
				break
			}
		}
	}
	r.cache[key] = entry{e, time.Now().Add(ttl)}
}
func (r *Resolver) Remember(identity, op string) {
	r.put(identity, op, Evidence{Supported: true, Source: "success"}, 15*time.Minute)
}

// Successful checks operation-specific evidence, not just HTTP status. Model
// listings, HTML frontends and generic JSON success bodies prove no operation.
func Successful(op string, body []byte) bool {
	check := func(b []byte) bool {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil || m["error"] != nil {
			return false
		}
		switch op {
		case Chat:
			choices, _ := m["choices"].([]any)
			if len(choices) == 0 {
				return false
			}
			choice, _ := choices[0].(map[string]any)
			message, messageOK := choice["message"].(map[string]any)
			delta, deltaOK := choice["delta"].(map[string]any)
			return m["object"] == "chat.completion" && messageOK && len(message) > 0 || m["object"] == "chat.completion.chunk" && deltaOK && len(delta) > 0
		case Messages:
			content, _ := m["content"].([]any)
			message, _ := m["message"].(map[string]any)
			return m["type"] == "message" && len(content) > 0 || m["type"] == "message_start" && message["type"] == "message"
		case "responses":
			return m["object"] == "response" && m["output"] != nil || m["type"] == "response.created"
		case "embeddings":
			items, ok := m["data"].([]any)
			if !ok || len(items) == 0 {
				return false
			}
			item, _ := items[0].(map[string]any)
			return item["object"] == "embedding" && item["embedding"] != nil
		}
		return false
	}
	if check(body) {
		return true
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "data:") && check([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))) {
			return true
		}
	}
	return false
}

func (r *Resolver) Cached(identity, op string) (Evidence, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.cache[cacheKey(identity, op)]
	return v.evidence, ok && time.Now().Before(v.until)
}

type Send func(context.Context, string, []byte) (*http.Response, error)

// Probe never supplies a model or a valid messages/input array. Authentication,
// rate limits, generic errors and generic 2xx responses are not capability evidence.
func (r *Resolver) Probe(ctx context.Context, identity, op string, send Send) Evidence {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	k := cacheKey(identity, op)
	for {
		r.mu.Lock()
		if v, ok := r.cache[k]; ok && time.Now().Before(v.until) {
			r.mu.Unlock()
			return v.evidence
		}
		if wait := r.pending[k]; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return Evidence{Source: "cancelled"}
			}
		}
		if r.pending == nil {
			r.pending = map[[32]byte]chan struct{}{}
		}
		if len(r.pending) >= 2048 {
			r.mu.Unlock()
			return Evidence{Source: "busy"}
		}
		r.pending[k] = make(chan struct{})
		r.mu.Unlock()
		break
	}
	defer func() { r.mu.Lock(); close(r.pending[k]); delete(r.pending, k); r.mu.Unlock() }()
	select {
	case probeSlots <- struct{}{}:
		defer func() { <-probeSlots }()
	case <-ctx.Done():
		return Evidence{Source: "cancelled"}
	}
	resp, err := send(ctx, op, []byte(`{"model":null,"messages":null,"input":null,"max_tokens":0}`))
	e := Evidence{Source: "inconclusive"}
	if err != nil {
		e.Source = "transport_failure"
		if ctx.Err() == context.DeadlineExceeded {
			e.Source = "deadline_exceeded"
		} else if ctx.Err() == context.Canceled {
			e.Source = "cancelled"
		}
	}
	if err == nil {
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
		e.Status = resp.StatusCode
		switch {
		case e.Status == 401 || e.Status == 403:
			e.Source = "authentication_or_permission_denied"
		case e.Status == 429:
			e.Source = "rate_limited"
		case e.Status >= 500:
			e.Source = "server_error"
		default:
			e.Source = "unrecognized_response"
		}
		if readErr != nil || len(b) > 64<<10 {
			e.Source = "body_read_or_size_failure"
			if ctx.Err() == context.DeadlineExceeded {
				e.Source = "deadline_exceeded"
			}
		}
		if readErr == nil && len(b) <= 64<<10 {
			var obj map[string]any
			if json.Unmarshal(b, &obj) == nil {
				encoded, _ := json.Marshal(obj["error"])
				text := strings.ToLower(string(encoded))
				field := "messages"
				if op == "responses" || op == "embeddings" {
					field = "input"
				}
				validation := obj["error"] != nil && strings.Contains(text, field) && (strings.Contains(text, "required") || strings.Contains(text, "must be") || strings.Contains(text, "invalid type") || strings.Contains(text, "expected") || strings.Contains(text, "valid list") || strings.Contains(text, "valid array"))
				// FastAPI-compatible servers report typed validation locations.
				if details, ok := obj["detail"].([]any); ok {
					for _, v := range details {
						d, _ := v.(map[string]any)
						loc, _ := d["loc"].([]any)
						if len(loc) >= 2 && loc[0] == "body" && loc[1] == field && (d["type"] == "list_type" || d["type"] == "missing") {
							validation = true
						}
					}
				}
				if (e.Status == 400 || e.Status == 422) && validation {
					e.Supported = true
					e.Source = "schema"
				}
			}
			// Model/auth errors can use 404 to obscure resource existence. They
			// are not evidence that the operation itself is missing.
			lower := strings.ToLower(string(b))
			resourceError := strings.Contains(lower, "model") || strings.Contains(lower, "api key") || strings.Contains(lower, "auth") || strings.Contains(lower, "credential") || strings.Contains(lower, "rate limit")
			if (e.Status == 404 || e.Status == 405 || e.Status == 501) && !resourceError {
				e.Unsupported = true
				e.Source = "endpoint"
			}
		}
	}
	if ctx.Err() != nil && ctx.Err() == context.Canceled {
		return e
	}
	ttl := 30 * time.Second
	if e.Supported || e.Unsupported {
		ttl = 5 * time.Minute
	}
	r.put(identity, op, e, ttl)
	return e
}

// Resolve prefers the client's native operation, even for dual-stack servers
// carrying an old, misleading type hint. Inconclusive auth errors stay native.
func (r *Resolver) Resolve(ctx context.Context, identity, native string, send Send) (string, error) {
	e := r.Probe(ctx, identity, native, send)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e.Supported {
		return native, nil
	}
	other := Messages
	if native == Messages {
		other = Chat
	}
	if native != Chat && native != Messages {
		if e.Unsupported {
			return "", fmt.Errorf("unsupported native operation %s; Responses and embeddings are not translated", native)
		}
		return native, nil
	}
	a := r.Probe(ctx, identity, other, send)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if a.Supported {
		return other, nil
	}
	if e.Unsupported {
		return "", fmt.Errorf("unsupported operation %s: no compatible endpoint confirmed", native)
	}
	return native, nil
}
