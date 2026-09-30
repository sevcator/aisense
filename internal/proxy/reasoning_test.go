package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aisense/internal/config"
)

func TestReasoningBackfillNeeded(t *testing.T) {
	yes := []string{
		`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`,
		`{"error":{"message":"reasoning_content must be passed back"}}`,
	}
	for _, body := range yes {
		if !reasoningBackfillNeeded([]byte(body)) {
			t.Fatalf("missed: %s", body)
		}
	}
	no := []string{
		`{"error":{"message":"invalid request: messages must be an array","type":"invalid_request_error"}}`,
		`{"error":{"message":"model not found"}}`,
		``,
		`{"error":{"message":"your key is invalid"}}`,
	}
	for _, body := range no {
		if reasoningBackfillNeeded([]byte(body)) {
			t.Fatalf("false positive: %s", body)
		}
	}
}

func TestExtractReasoningContent(t *testing.T) {
	jsonBody := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"step one"}}]}`)
	if got := extractReasoningContent(jsonBody); got != "step one" {
		t.Fatalf("json reasoning = %q", got)
	}
	sse := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"alpha \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"beta\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n" +
		"data: [DONE]\n")
	if got := extractReasoningContent(sse); got != "alpha beta" {
		t.Fatalf("sse reasoning = %q", got)
	}
	if got := extractReasoningContent([]byte(`{"choices":[{"message":{"content":"plain"}}]}`)); got != "" {
		t.Fatalf("plain reasoning = %q", got)
	}
}

func TestInjectAndDetectReasoning(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ack"},{"role":"user","content":"more"}]}`)
	if lastAssistantHasReasoning(body) {
		t.Fatal("assistant history must count as missing reasoning only for the last assistant message")
	}
	// The last assistant message is the second message; the trailing user turn
	// does not change what the provider checks.
	fixed := injectReasoningContent(body, "RC")
	if fixed == nil {
		t.Fatal("injection failed")
	}
	if !lastAssistantHasReasoning(fixed) {
		t.Fatalf("reasoning not injected: %s", fixed)
	}
	var doc struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fixed, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Messages[1].ReasoningContent != "RC" || doc.Messages[1].Role != "assistant" {
		t.Fatalf("injected into the wrong message: %+v", doc.Messages)
	}
	// A body without any assistant message has nothing to inject.
	if got := injectReasoningContent([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "RC"); got != nil {
		t.Fatalf("a user-only body must be left alone, got %s", got)
	}
}

// Thinking-mode providers reject follow-up turns whose assistant history
// lacks the reasoning_content their answers carried. aisense must capture it,
// retry the rejected turn with it injected, and inject proactively afterwards.
func TestReasoningContentBackfillBridgesThinkingProviders(t *testing.T) {
	var mu sync.Mutex
	turns := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role             string `json:"role"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rc := ""
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "assistant" {
				rc = body.Messages[i].ReasoningContent
				break
			}
		}
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()
		if n == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ack","reasoning_content":"THINK-1"}}]}`))
			return
		}
		if rc != "THINK-1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer up.Close()

	p, _ := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{{ID: "think", Enabled: true, Type: "openai", BaseURL: up.URL, Models: []string{"glm-x"}, AuthMode: "none"}}
	})
	chat := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		p.ServeOpenAI(res, req)
		return res
	}

	res := chat(`{"model":"glm-x","messages":[{"role":"user","content":"hi"}]}`)
	if res.Code != 200 {
		t.Fatalf("turn 1: %d %s", res.Code, res.Body.String())
	}

	// Turn 2 without reasoning_content: the upstream 400s, aisense retries
	// with the captured reasoning and the client still sees success.
	res = chat(`{"model":"glm-x","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ack"},{"role":"user","content":"go on"}]}`)
	if res.Code != 200 {
		t.Fatalf("turn 2: %d %s", res.Code, res.Body.String())
	}

	// Turn 3 is injected proactively: the upstream never sees a missing field.
	res = chat(`{"model":"glm-x","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ack"},{"role":"user","content":"go on"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`)
	if res.Code != 200 {
		t.Fatalf("turn 3: %d %s", res.Code, res.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	// Turn 1 once, turn 2 twice (the rejected attempt plus the retried one),
	// turn 3 once thanks to proactive injection.
	if turns != 4 {
		t.Fatalf("upstream saw %d requests, want 4", turns)
	}
}

// The tier walk and health failover hop between upstreams mid-conversation.
// The reasoning_content captured from one upstream must be injected when a
// different upstream takes over the same conversation and demands the field.
func TestReasoningBackfillSurvivesUpstreamHop(t *testing.T) {
	var mu sync.Mutex
	turns := 0
	lenient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		turns++
		n := turns
		mu.Unlock()
		if n == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ack","reasoning_content":"THINK-A"}}]}`))
			return
		}
		// After serving the first turn, the lenient upstream goes down so the
		// walk fails over to the strict one for the next client turn.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer lenient.Close()
	strict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role             string `json:"role"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rc := ""
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "assistant" {
				rc = body.Messages[i].ReasoningContent
				break
			}
		}
		if rc != "THINK-A" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"served by the strict upstream"}}]}`))
	}))
	defer strict.Close()

	p, _ := newTestGateway(t, func(c *config.Config) {
		c.Upstreams = []*config.Upstream{
			{ID: "lenient", Enabled: true, Type: "openai", BaseURL: lenient.URL, Models: []string{"glm-x"}, AuthMode: "none"},
			{ID: "strict", Enabled: true, Type: "openai", BaseURL: strict.URL, Models: []string{"glm-x"}, AuthMode: "none"},
		}
	})
	chat := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer gateway-key")
		res := httptest.NewRecorder()
		p.ServeOpenAI(res, req)
		return res
	}

	res := chat(`{"model":"glm-x","messages":[{"role":"user","content":"hi"}]}`)
	if res.Code != 200 {
		t.Fatalf("turn 1: %d %s", res.Code, res.Body.String())
	}
	// Turn 2 lands on the strict upstream (the lenient one now fails): the
	// reasoning captured there must be injected, possibly after one 400.
	res = chat(`{"model":"glm-x","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ack"},{"role":"user","content":"go on"}]}`)
	if res.Code != 200 {
		t.Fatalf("turn 2 after the hop: %d %s", res.Code, res.Body.String())
	}
}
