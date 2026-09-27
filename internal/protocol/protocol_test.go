package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpoint(t *testing.T) {
	for _, base := range []string{"https://example.test/v1", "https://example.test/v1/messages", "https://example.test/v1/chat/completions", "https://example.test/v1/responses", "https://example.test/v1/embeddings", "https://example.test"} {
		if got := Endpoint(base, "models"); got != "https://example.test/v1/models" {
			t.Errorf("%s: %s", base, got)
		}
	}
	if got := Endpoint("https://example.test/gateway/v3/messages", Chat); got != "https://example.test/gateway/v3/chat/completions" {
		t.Fatal(got)
	}
	if got := Endpoint("https://example.test/messages", Chat); got != "https://example.test/chat/completions" {
		t.Fatal(got)
	}
}

func TestDetectionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status                 int
		body                   string
		supported, unsupported bool
	}{
		{"schema", 400, `{"error":{"message":"messages must be an array"}}`, true, false},
		{"schema422", 422, `{"error":{"message":"messages: field required"}}`, true, false},
		{"anthropicSchema", 400, `{"error":{"type":"invalid_request_error","message":"messages: Input should be a valid list"}}`, true, false},
		{"fastAPI", 422, `{"detail":[{"loc":["body","messages"],"type":"list_type","msg":"Input should be a valid list"}]}`, true, false},
		{"generic400", 400, `{"error":{"message":"bad request"}}`, false, false},
		{"auth", 401, `{"error":{"message":"messages required"}}`, false, false},
		{"rate", 429, `{"error":{"message":"messages required"}}`, false, false},
		{"missing", 404, `{"error":{"message":"route not found"}}`, false, true},
		{"missingModel", 404, `{"error":{"message":"model not found"}}`, false, false},
		{"html", 400, `<html>messages required</html>`, false, false},
		{"genericSuccess", 200, `{"ok":true}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != nil || body["messages"] != nil || body["max_tokens"] != float64(0) {
					t.Errorf("unsafe probe: %#v", body)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			send := func(ctx context.Context, op string, b []byte) (*http.Response, error) {
				req, _ := http.NewRequestWithContext(ctx, "POST", Endpoint(server.URL, op), bytes.NewReader(b))
				return server.Client().Do(req)
			}
			var r Resolver
			for i := 0; i < 2; i++ {
				e := r.Probe(context.Background(), "url/key", Chat, send)
				if e.Supported != tc.supported || e.Unsupported != tc.unsupported {
					t.Fatal(e)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("cache missed: %d", calls.Load())
			}
			r.Probe(context.Background(), "url/new-key", Chat, send)
			if calls.Load() != 2 {
				t.Fatal("changed credentials reused cache")
			}
		})
	}
}

func TestResolverNativePreferenceAndCancellation(t *testing.T) {
	for _, native := range []string{Chat, Messages} {
		var r Resolver
		r.Remember("dual", Chat)
		r.Remember("dual", Messages)
		got, err := r.Resolve(context.Background(), "dual", native, func(context.Context, string, []byte) (*http.Response, error) {
			t.Fatal("unexpected probe")
			return nil, nil
		})
		if err != nil || got != native {
			t.Fatalf("%s %v", got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var r Resolver
	_, err := r.Resolve(ctx, "cancel", Chat, func(ctx context.Context, _ string, _ []byte) (*http.Response, error) { return nil, ctx.Err() })
	if err != context.Canceled {
		t.Fatal(err)
	}
}

func TestProbeCoalescingAndBounds(t *testing.T) {
	var r Resolver
	var calls atomic.Int32
	send := func(context.Context, string, []byte) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"messages required"}}`))}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Probe(context.Background(), "same", Chat, send) }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	for i := 0; i < 3000; i++ {
		r.Remember(string(rune(i)), Chat)
	}
	if len(r.cache) > 2048 {
		t.Fatal(len(r.cache))
	}
}

func TestRequestTextAndToolsRoundTrip(t *testing.T) {
	b := []byte(`{"model":"m","max_tokens":99,"messages":[{"role":"system","content":"be helpful"},{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"parallel_tool_calls":false}`)
	a, err := Request(b, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(a, []byte(`"tool_use_id":"call_1"`)) || !bytes.Contains(a, []byte(`"system"`)) {
		t.Fatal(string(a))
	}
	o, err := Request(a, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	var m object
	_ = json.Unmarshal(o, &m)
	msgs := arr(m["messages"])
	if len(msgs) != 4 || obj(msgs[0])["role"] != "system" || obj(msgs[3])["tool_call_id"] != "call_1" || m["parallel_tool_calls"] != false {
		t.Fatal(string(o))
	}
	if obj(obj(arr(obj(msgs[2])["tool_calls"])[0])["function"])["arguments"] != `{"city":"Paris"}` {
		t.Fatal(string(o))
	}
}

func TestUnsupportedFeaturesExplicit(t *testing.T) {
	for _, b := range []string{
		`{"messages":[],"reasoning_effort":"high"}`,
		`{"messages":[],"response_format":{"type":"json_object"}}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"x","arguments":"bad"}}]}]}`,
		`{"messages":[],"n":2}`,
	} {
		if _, err := Request([]byte(b), "openai"); err == nil {
			t.Fatal(b)
		}
	}
	for _, b := range []string{`{"messages":[],"thinking":{"type":"enabled"}}`, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret"}]}]}`} {
		if _, err := Request([]byte(b), "anthropic"); err == nil {
			t.Fatal(b)
		}
	}
}

func TestResponseConversion(t *testing.T) {
	a := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call_1","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`)
	o, err := Response(a, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(o, []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(o, []byte(`"total_tokens":12`)) {
		t.Fatal(string(o))
	}
	a, err = Response(o, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(a, []byte(`"input_tokens":5`)) || !bytes.Contains(a, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatal(string(a))
	}
}

func TestToolArgumentIntegerPrecision(t *testing.T) {
	b := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"id\":9007199254740993}"}}]}]}`)
	a, err := Request(b, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(a, []byte("9007199254740993")) {
		t.Fatal(string(a))
	}
	o, err := Request(a, "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(o, []byte("9007199254740993")) {
		t.Fatal(string(o))
	}
}

func TestIncrementalStreamsBothDirections(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}`,
		`[DONE]`,
	}
	s := &Stream{From: "openai"}
	var anthropic bytes.Buffer
	for i, f := range frames {
		out, err := s.Feed([]byte("data: " + f + "\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !bytes.Contains(out, []byte("Hello")) {
			t.Fatal("text was not incremental")
		}
		if i == 1 && !bytes.Contains(out, []byte("partial_json")) {
			t.Fatal("tool args were buffered")
		}
		anthropic.Write(out)
	}
	if !s.Complete() {
		t.Fatal("not complete")
	}
	r := bufio.NewReader(bytes.NewReader(anthropic.Bytes()))
	s = &Stream{From: "anthropic"}
	var openai bytes.Buffer
	for {
		f, err := ReadFrame(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := s.Feed(f)
		if err != nil {
			t.Fatal(err)
		}
		openai.Write(out)
	}
	for _, want := range []string{`"content":"Hello"`, `"id":"call_1"`, `"arguments":"{\"city\":"`, `"arguments":"\"Paris\"}"`, `"prompt_tokens":5`, `"completion_tokens":7`, `"finish_reason":"tool_calls"`, `[DONE]`} {
		if !strings.Contains(openai.String(), want) {
			t.Errorf("missing %s in %s", want, openai.String())
		}
	}
}

func TestStreamErrorsAndFrameBound(t *testing.T) {
	s := &Stream{From: "openai"}
	if _, err := s.Feed([]byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("accepted premature done")
	}
	if _, err := s.Feed([]byte("data: {\"error\":{\"message\":\"overloaded\"}}\n\n")); err == nil {
		t.Fatal("accepted upstream error")
	}
	if _, err := ReadFrame(bufio.NewReader(strings.NewReader("data: " + strings.Repeat("x", (1<<20)+1) + "\n\n"))); err == nil {
		t.Fatal("accepted oversized SSE event")
	}
}

func TestRedirectNoCredentialLeak(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := origin.Client()
	c.CheckRedirect = NoRedirect
	req, _ := http.NewRequest("POST", origin.URL, strings.NewReader("{}"))
	req.Header.Set("X-Api-Key", "secret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 307 || leaked.Load() {
		t.Fatal("redirect followed")
	}
}
