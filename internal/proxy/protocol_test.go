package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/store"
)

func protocolGateway(t *testing.T, upstreams ...*config.Upstream) *Proxy {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "client-key", Enabled: true}}
		c.Upstreams = upstreams
		c.Hitchance.RetryCycles = 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return New(cfg, st)
}
func protocolRequest(p *Proxy, typ, body string) *httptest.ResponseRecorder {
	path := "/v1/chat/completions"
	if typ == "anthropic" {
		path = "/v1/messages"
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if typ == "anthropic" {
		p.ServeAnthropic(w, r)
	} else {
		p.ServeOpenAI(w, r)
	}
	return w
}

const anthropicReply = `{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call1","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`
const openaiReply = `{"id":"msg1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`
const anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const openaiStream = "data: {\"id\":\"msg1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"msg1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"msg1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"

func TestAutomaticProtocolTranslationEndToEnd(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", typ, stream), func(t *testing.T) {
				destPath := "/v1/messages"
				reply := anthropicReply
				sse := anthropicStream
				if typ == "anthropic" {
					destPath = "/v1/chat/completions"
					reply = openaiReply
					sse = openaiStream
				}
				var probes, real atomic.Int32
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != destPath {
						w.WriteHeader(404)
						return
					}
					if typ == "openai" {
						if r.Header.Get("X-Api-Key") != "up-key" || r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Version") == "" {
							t.Errorf("wrong Anthropic auth: %v", r.Header)
						}
					} else if r.Header.Get("Authorization") != "Bearer up-key" || r.Header.Get("X-Api-Key") != "" {
						t.Errorf("wrong OpenAI auth: %v", r.Header)
					}
					var b map[string]any
					_ = json.NewDecoder(r.Body).Decode(&b)
					if b["model"] == nil {
						probes.Add(1)
						w.WriteHeader(400)
						io.WriteString(w, `{"error":{"message":"messages must be an array"}}`)
						return
					}
					real.Add(1)
					if b["model"] != "m" {
						t.Errorf("model: %v", b["model"])
					}
					if b["stream"] != stream {
						t.Errorf("stream: %v", b["stream"])
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, sse)
					} else {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("ETag", "old")
						io.WriteString(w, reply)
					}
				}))
				defer api.Close()
				// The legacy hint is deliberately the listener, not the destination.
				p := protocolGateway(t, &config.Upstream{ID: "up", Type: typ, BaseURL: api.URL + destPath, APIKeys: []string{"up-key"}, Models: []string{"m"}, Enabled: true})
				for i := 0; i < 2; i++ {
					w := protocolRequest(p, typ, fmt.Sprintf(`{"model":"m","max_tokens":20,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream))
					if w.Code != 200 {
						t.Fatalf("%d %s", w.Code, w.Body.String())
					}
					if !strings.Contains(w.Body.String(), "hello") {
						t.Fatal(w.Body.String())
					}
					if !stream && !strings.Contains(w.Body.String(), "call1") {
						t.Fatal("tool ID lost")
					}
					if w.Header().Get("ETag") != "" {
						t.Fatal("stale content header")
					}
					u := extractUsage(typ, w.Body.Bytes())
					if u.In != 5 || u.Out != 7 {
						t.Fatalf("usage=%+v, body=%s", u, w.Body.String())
					}
				}
				if real.Load() != 2 || probes.Load() != 1 {
					t.Fatalf("real=%d probes=%d", real.Load(), probes.Load())
				}
				if p.Cfg.Get().Upstreams[0].Type != typ {
					t.Fatal("detection mutated config identity")
				}
			})
		}
	}
}

func TestNativeDualStackIgnoresLegacyHint(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic"} {
		t.Run(typ, func(t *testing.T) {
			var calls atomic.Int32
			body := openaiReply
			path := "/v1/chat/completions"
			hint := "anthropic"
			if typ == "anthropic" {
				body = anthropicReply
				path = "/v1/messages"
				hint = "openai"
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != path {
					t.Errorf("not native: %s", r.URL.Path)
				}
				io.WriteString(w, body)
			}))
			defer api.Close()
			p := protocolGateway(t, &config.Upstream{ID: "up", Type: hint, BaseURL: api.URL + "/v1", Models: []string{"m"}, Enabled: true})
			w := protocolRequest(p, typ, `{"model":"m","messages":[],"provider_extension":{"preserved":true}}`)
			if w.Code != 200 || w.Body.String() != body || calls.Load() != 1 {
				t.Fatalf("native passthrough: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
			}
		})
	}
}

func TestCrossFormatErrorsNativeOnlyAndUnsupportedFeatures(t *testing.T) {
	var real atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["model"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"messages required"}}`)
			return
		}
		real.Add(1)
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad tools"}}`)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", BaseURL: api.URL + "/v1/messages", Models: []string{"m"}, Enabled: true})
	w := protocolRequest(p, "openai", `{"model":"m","messages":[]}`)
	if w.Code != 400 || strings.Contains(w.Body.String(), `"type":"error"`) || !strings.Contains(w.Body.String(), "bad tools") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = protocolRequest(p, "openai", `{"model":"m","messages":[],"reasoning_effort":"high"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unsupported cross-format") || real.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, op := range []string{"responses", "embeddings"} {
		r := httptest.NewRequest("POST", "/v1/"+op, strings.NewReader(`{"model":"m","input":"hello"}`))
		r.Header.Set("Authorization", "Bearer client-key")
		w = httptest.NewRecorder()
		p.ServeOpenAI(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "not translated") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestConvertedStreamFailureNeverRetriesAfterCommit(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["model"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"messages required"}}`)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Split(anthropicStream, "event: content_block_start")[0])
		w.(http.Flusher).Flush()
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"broken\"}}\n\n")
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", BaseURL: api.URL + "/v1", Models: []string{"m"}, Enabled: true})
	w := protocolRequest(p, "openai", `{"model":"m","stream":true,"messages":[]}`)
	if calls.Load() != 1 || !strings.Contains(w.Body.String(), "broken") || strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal(calls.Load(), w.Body.String())
	}
	if len(p.CachedUpstreams()) != 0 {
		t.Fatal("failed stream was pinned")
	}
}

func TestConvertedStreamCancellation(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["model"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"messages required"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Split(anthropicStream, "event: content_block_start")[0])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", BaseURL: api.URL + "/v1", Models: []string{"m"}, Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer client-key")
	done := make(chan struct{})
	go func() { p.ServeOpenAI(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream did not cancel")
	}
}

func TestConvertedStreamFailoverBeforeCommit(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["model"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"messages required"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	defer first.Close()
	var fallback atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallback.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, openaiStream)
	}))
	defer second.Close()
	p := protocolGateway(t, &config.Upstream{ID: "first", Priority: 1, BaseURL: first.URL + "/v1", Models: []string{"m"}, Enabled: true}, &config.Upstream{ID: "second", Priority: 2, BaseURL: second.URL + "/v1", Models: []string{"m"}, Enabled: true})
	w := protocolRequest(p, "openai", `{"model":"m","stream":true,"messages":[]}`)
	if w.Code != 200 || w.Body.String() != openaiStream || fallback.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), fallback.Load())
	}
}

func TestNativeStreamResponseStartTimeoutFailsOver(t *testing.T) {
	var timedOutCalls, fallbackCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timedOutCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer second.Close()
	p := protocolGateway(t,
		&config.Upstream{ID: "first", Priority: 1, BaseURL: first.URL + "/v1", Models: []string{"m"}, Enabled: true},
		&config.Upstream{ID: "second", Priority: 2, BaseURL: second.URL + "/v1", Models: []string{"m"}, Enabled: true},
	)
	if err := p.Cfg.Update(func(c *config.Config) error {
		c.Hitchance.RetryCycles = 0
		c.Hitchance.ResponseStartTimeoutSeconds = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := protocolRequest(p, "openai", `{"model":"m","stream":true,"messages":[]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if timedOutCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("timed out calls=%d fallback calls=%d", timedOutCalls.Load(), fallbackCalls.Load())
	}
}

func TestCapabilityCacheInvalidatesCredentialChanges(t *testing.T) {
	var old, newCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path == "/v1/messages" && r.Header.Get("X-Api-Key") == "old" {
			if b["model"] == nil {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":{"message":"messages required"}}`)
				return
			}
			old.Add(1)
			io.WriteString(w, anthropicReply)
			return
		}
		if r.URL.Path == "/v1/chat/completions" && r.Header.Get("Authorization") == "Bearer new" {
			newCalls.Add(1)
			io.WriteString(w, openaiReply)
			return
		}
		w.WriteHeader(404)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", BaseURL: api.URL + "/v1", APIKeys: []string{"old"}, Models: []string{"m"}, Enabled: true})
	if w := protocolRequest(p, "openai", `{"model":"m","messages":[]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := p.Cfg.Update(func(c *config.Config) error { c.Upstreams[0].APIKeys = []string{"new"}; return nil }); err != nil {
		t.Fatal(err)
	}
	w := protocolRequest(p, "openai", `{"model":"m","messages":[]}`)
	if w.Code != 200 || w.Body.String() != openaiReply || old.Load() != 1 || newCalls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), old.Load(), newCalls.Load())
	}
}

func TestNativeAnthropicStreamUsage(t *testing.T) {
	u := extractUsage("anthropic", []byte(anthropicStream))
	if u.In != 5 || u.Out != 7 {
		t.Fatalf("%+v", u)
	}
}
