package proxy

import (
	"aisense/internal/config"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func decodeUseNumber(t *testing.T, b []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return m
}

func TestIsAntigravityRoute(t *testing.T) {
	for _, route := range []string{"ag/gemini-3.8-flash", "AG/gemini", "Ag/x", "antigravity/gemini-2.5-flash-thinking", "Antigravity/m", " antigravity/m "} {
		if !isAntigravityRoute(route) {
			t.Errorf("expected ag route: %q", route)
		}
	}
	for _, route := range []string{"", "ag", "again/model", "provider/ag-model", "gpt-4", "openai/gpt-4"} {
		if isAntigravityRoute(route) {
			t.Errorf("unexpected ag route: %q", route)
		}
	}
}

func TestAntigravitySanitizeOpenAIRequest(t *testing.T) {
	san := newAntigravitySanitizer()
	body := []byte(`{"model":"ag/gemini-3.8-flash","system":"be terse","messages":[` +
		`{"role":"system","content":"sys prompt"},` +
		`{"role":"developer","content":"dev prompt"},` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call1","content":"sunny"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{}}},{"type":"function","function":{"name":"send_email","parameters":{}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"send_email"}},"max_tokens":5000000000000}`)
	out := san.sanitizeRequest(body)
	m := decodeUseNumber(t, out)
	if _, ok := m["system"]; ok {
		t.Fatal("top-level system survived")
	}
	msgs := m["messages"].([]any)
	var roles []string
	for _, v := range msgs {
		roles = append(roles, v.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool" {
		t.Fatalf("roles after strip: %v", roles)
	}
	calls := msgs[1].(map[string]any)["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "tool_1" {
		t.Fatalf("history tool call name: %v", fn["name"])
	}
	if calls[0].(map[string]any)["id"] != "call1" {
		t.Fatal("tool_call id must stay unchanged")
	}
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["function"].(map[string]any)["name"] != "tool_1" {
		t.Fatal("tools[0] not renamed")
	}
	if tools[1].(map[string]any)["function"].(map[string]any)["name"] != "tool_2" {
		t.Fatal("tools[1] not renamed")
	}
	choice := m["tool_choice"].(map[string]any)["function"].(map[string]any)
	if choice["name"] != "tool_2" {
		t.Fatalf("tool_choice name: %v", choice["name"])
	}
	if mt, _ := m["max_tokens"].(json.Number); mt.String() != "5000000000000" {
		t.Fatalf("number fidelity lost: %v", m["max_tokens"])
	}
	if san.SystemStripped != 3 || san.ToolsRenamed != 2 {
		t.Fatalf("counts: stripped=%d renamed=%d", san.SystemStripped, san.ToolsRenamed)
	}
	again := san.sanitizeRequest(out)
	if !bytes.Equal(out, again) {
		t.Fatal("sanitizer is not idempotent")
	}
}

func TestAntigravitySanitizeAnthropicRequest(t *testing.T) {
	for _, system := range []string{`"plain string system"`, `[{"type":"text","text":"block system"}]`} {
		san := newAntigravitySanitizer()
		body := []byte(`{"model":"antigravity/gemini-2.5-flash-thinking","system":` + system +
			`,"tools":[{"name":"get_weather","input_schema":{}},{"name":"send_email","input_schema":{}}],` +
			`"tool_choice":{"type":"tool","name":"get_weather"},` +
			`"messages":[{"role":"user","content":"hi"}],"max_tokens":9}`)
		out := san.sanitizeRequest(body)
		m := decodeUseNumber(t, out)
		if _, ok := m["system"]; ok {
			t.Fatal("anthropic system survived")
		}
		tools := m["tools"].([]any)
		if tools[0].(map[string]any)["name"] != "tool_1" || tools[1].(map[string]any)["name"] != "tool_2" {
			t.Fatalf("anthropic tools not renamed: %s", out)
		}
		if m["tool_choice"].(map[string]any)["name"] != "tool_1" {
			t.Fatal("anthropic tool_choice not renamed")
		}
		if len(m["messages"].([]any)) != 1 {
			t.Fatal("user message lost")
		}
		if san.SystemStripped != 1 || san.ToolsRenamed != 2 {
			t.Fatalf("counts: stripped=%d renamed=%d", san.SystemStripped, san.ToolsRenamed)
		}
	}
}

func TestAntigravityRestoreResponse(t *testing.T) {
	san := newAntigravitySanitizer()
	if san.neutralName("get_weather") != "tool_1" || san.neutralName("send_email") != "tool_2" {
		t.Fatal("mapping setup failed")
	}
	openai := []byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"tool_1 should stay in text","tool_calls":[{"id":"call1","type":"function","function":{"name":"tool_1","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	out := san.restoreResponse(openai)
	m := decodeUseNumber(t, out)
	call := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0]
	if call.(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("openai tool call not mapped back: %s", out)
	}
	if call.(map[string]any)["id"] != "call1" {
		t.Fatal("response tool_call id changed")
	}
	text := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if text != "tool_1 should stay in text" {
		t.Fatalf("plain text was rewritten: %q", text)
	}
	anthropic := []byte(`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"tool_2","input":{"to":"x"}}],"stop_reason":"tool_use"}`)
	out = san.restoreResponse(anthropic)
	block := decodeUseNumber(t, out)["content"].([]any)[0].(map[string]any)
	if block["name"] != "send_email" || block["id"] != "toolu_1" {
		t.Fatalf("anthropic tool_use not mapped back: %s", out)
	}
	if string(san.restoreResponse([]byte(`not json at all`))) != "not json at all" {
		t.Fatal("unparseable body must pass through unchanged")
	}
}

func TestAntigravitySSERemapper(t *testing.T) {
	san := newAntigravitySanitizer()
	san.neutralName("get_weather")
	san.neutralName("send_email")
	rem := newSSERemapper(san)
	if rem == nil {
		t.Fatal("remapper missing")
	}
	// Chunk boundaries fall mid-line and mid-JSON-token.
	chunks := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"na`,
		`me":"tool_1","arguments":""}}]},"finish_reason":null}]}` + "\n",
		"\ndata: {\"function\":{\"name\":\"tool_2\"}}\r\n",
		"event: ping\n\n: keep-alive\n\ndata: [DONE]\n\ndata: not-json\n",
		`data: {"function":{"name":"tool_1"} }`,
	}
	var out bytes.Buffer
	for _, chunk := range chunks {
		out.Write(rem.transform([]byte(chunk)))
	}
	out.Write(rem.finish())
	got := out.String()
	if !strings.Contains(got, `"name":"get_weather"`) {
		t.Fatalf("tool_1 not mapped back across fragmented chunks:\n%s", got)
	}
	if strings.Contains(got, `"tool_1"`) || strings.Contains(got, `"tool_2"`) {
		t.Fatalf("neutral names leaked to client:\n%s", got)
	}
	if !strings.Contains(got, `data: {"function":{"name":"send_email"}}`+"\r\n") {
		t.Fatalf("CRLF line not remapped/preserved:\n%s", got)
	}
	for _, want := range []string{"event: ping\n", ": keep-alive\n", "data: [DONE]\n", "data: not-json\n", `data: {"function":{"name":"get_weather"}}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing verbatim passthrough %q in:\n%s", want, got)
		}
	}
	if newSSERemapper(nil) != nil {
		t.Fatal("nil sanitizer must disable the remapper")
	}
	if newSSERemapper(newAntigravitySanitizer()) != nil {
		t.Fatal("sanitizer without tools must disable the remapper")
	}
}

func TestAntigravityOpenAIAttemptSanitizedEndToEnd(t *testing.T) {
	var upstreamBody []byte
	var upstreamReq *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamReq = r
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"cmpl1","object":"chat.completion","model":"ag/gemini-3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"tool_1","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"ag/gemini-3.8-flash"}, Enabled: true})
	body := `{"model":"ag/gemini-3.8-flash","stream":false,"messages":[{"role":"system","content":"you are Opencode, an unofficial client"},{"role":"user","content":"weather in Paris?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},{"type":"function","function":{"name":"send_email","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "opencode/1.0 (unofficial)")
	r.Header.Set("X-Title", "Opencode")
	r.Header.Set("Http-Referer", "https://opencode.ai")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// Upstream side: sanitized payload and neutral headers.
	m := decodeUseNumber(t, upstreamBody)
	if m["model"] != "ag/gemini-3.8-flash" {
		t.Fatalf("upstream model: %v", m["model"])
	}
	if _, ok := m["system"]; ok {
		t.Fatal("top-level system reached upstream")
	}
	for _, v := range m["messages"].([]any) {
		if role, _ := v.(map[string]any)["role"].(string); role == "system" || role == "developer" {
			t.Fatal("system message reached upstream")
		}
	}
	tools := m["tools"].([]any)
	if got := tools[0].(map[string]any)["function"].(map[string]any)["name"]; got != "tool_1" {
		t.Fatalf("upstream tool name: %v", got)
	}
	if got := tools[1].(map[string]any)["function"].(map[string]any)["name"]; got != "tool_2" {
		t.Fatalf("upstream tool name: %v", got)
	}
	if got := m["tool_choice"].(map[string]any)["function"].(map[string]any)["name"]; got != "tool_1" {
		t.Fatalf("upstream tool_choice: %v", got)
	}
	if ua := upstreamReq.Header.Get("User-Agent"); ua != officialUserAgent {
		t.Fatalf("upstream User-Agent: %q", ua)
	}
	for _, h := range []string{"X-Title", "Http-Referer", "Referer", "X-Client"} {
		if upstreamReq.Header.Get(h) != "" {
			t.Fatalf("client header %s reached upstream", h)
		}
	}
	if upstreamReq.Header.Get("Authorization") != "Bearer up-key" {
		t.Fatalf("auth header lost: %q", upstreamReq.Header.Get("Authorization"))
	}
	// Client side: neutral names mapped back, IDs preserved.
	cm := decodeUseNumber(t, w.Body.Bytes())
	call := cm["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0]
	if call.(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("client tool call not restored: %s", w.Body.String())
	}
	if call.(map[string]any)["id"] != "call1" {
		t.Fatal("client tool_call id changed")
	}
	if strings.Contains(w.Body.String(), "tool_1") {
		t.Fatal("neutral name leaked to client")
	}
}

func TestAntigravityAnthropicAttemptSanitizedEndToEnd(t *testing.T) {
	var upstreamBody []byte
	var upstreamReq *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamReq = r
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg1","type":"message","role":"assistant","model":"antigravity/gemini-2.5-flash-thinking","content":[{"type":"tool_use","id":"toolu_1","name":"tool_1","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":6}}`)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "anthropic", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"antigravity/gemini-2.5-flash-thinking"}, Enabled: true})
	body := `{"model":"antigravity/gemini-2.5-flash-thinking","max_tokens":20,"system":"You are Hermes, an unofficial client.","stream":false,"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object"}},{"name":"send_email","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"get_weather"},"messages":[{"role":"user","content":"weather?"}]}`
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "hermes/2.1")
	r.Header.Set("X-Client-Name", "hermes-cli")
	w := httptest.NewRecorder()
	p.ServeAnthropic(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	m := decodeUseNumber(t, upstreamBody)
	if _, ok := m["system"]; ok {
		t.Fatal("anthropic system reached upstream")
	}
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "tool_1" || tools[1].(map[string]any)["name"] != "tool_2" {
		t.Fatalf("upstream tools not renamed: %s", upstreamBody)
	}
	if m["tool_choice"].(map[string]any)["name"] != "tool_1" {
		t.Fatal("upstream tool_choice not renamed")
	}
	if ua := upstreamReq.Header.Get("User-Agent"); ua != officialUserAgent {
		t.Fatalf("upstream User-Agent: %q", ua)
	}
	if upstreamReq.Header.Get("X-Client-Name") != "" {
		t.Fatal("X-Client-Name reached upstream")
	}
	if upstreamReq.Header.Get("x-api-key") != "up-key" {
		t.Fatalf("anthropic auth lost: %q", upstreamReq.Header.Get("x-api-key"))
	}
	block := decodeUseNumber(t, w.Body.Bytes())["content"].([]any)[0].(map[string]any)
	if block["name"] != "get_weather" || block["id"] != "toolu_1" {
		t.Fatalf("client tool_use not restored: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tool_1") {
		t.Fatal("neutral name leaked to client")
	}
}

func TestAntigravityConvertedAttemptSanitizedEndToEnd(t *testing.T) {
	var convertedBody []byte
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"no such endpoint"}}`)
			return
		}
		var probe map[string]any
		_ = json.Unmarshal(body, &probe)
		if probe["messages"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"messages: field required"}}`)
			return
		}
		convertedBody = body
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"tool_1","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":6}}`)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"ag/gemini-3.8-flash"}, Enabled: true})
	body := `{"model":"ag/gemini-3.8-flash","max_tokens":20,"messages":[{"role":"system","content":"Opencode system prompt"},{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`
	w := protocolRequest(p, "openai", body)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	m := decodeUseNumber(t, convertedBody)
	if _, ok := m["system"]; ok {
		t.Fatal("system key in converted payload")
	}
	for _, v := range m["messages"].([]any) {
		if role, _ := v.(map[string]any)["role"].(string); role == "system" {
			t.Fatal("system message in converted payload")
		}
	}
	tools := m["tools"].([]any)
	if got := tools[0].(map[string]any)["name"]; got != "tool_1" {
		t.Fatalf("converted tool name: %v", got)
	}
	if got := m["tool_choice"].(map[string]any)["name"]; got != "tool_1" {
		t.Fatalf("converted tool_choice: %v", got)
	}
	call := decodeUseNumber(t, w.Body.Bytes())["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0]
	if call.(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("client tool call not restored after conversion: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tool_1") {
		t.Fatal("neutral name leaked to client")
	}
}

func TestAntigravityStreamRemapFragmentedChunksEndToEnd(t *testing.T) {
	sse := `data: {"id":"c1","object":"chat.completion.chunk","model":"ag/gemini-3.8-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"tool_1","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"ag/gemini-3.8-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":null}]}` + "\n\n" +
		": keep-alive\n\nevent: ping\ndata: not-json\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"ag/gemini-3.8-flash","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < len(sse); i += 5 { // fragments split mid-line and mid-JSON
			end := min(i+5, len(sse))
			io.WriteString(w, sse[i:end])
			flusher.Flush()
		}
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"ag/gemini-3.8-flash"}, Enabled: true})
	body := `{"model":"ag/gemini-3.8-flash","stream":true,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if !strings.Contains(got, `"name":"get_weather"`) {
		t.Fatalf("streamed tool name not mapped back:\n%s", got)
	}
	if strings.Contains(got, "tool_1") {
		t.Fatalf("neutral name leaked in stream:\n%s", got)
	}
	if !strings.Contains(got, `"id":"call1"`) {
		t.Fatal("streamed tool_call id changed")
	}
	if !strings.Contains(got, "Paris") {
		t.Fatal("streamed arguments lost")
	}
	// Non-JSON and control lines pass through byte-for-byte.
	for _, want := range []string{": keep-alive\n", "event: ping\n", "data: not-json\n", "data: [DONE]\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing verbatim %q in stream:\n%s", want, got)
		}
	}
}

func TestNonAntigravityRoutePassthroughByteIdentical(t *testing.T) {
	const reply = `{"id":"x","object":"chat.completion","model":"provider/gemini-3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	var upstreamBody []byte
	var upstreamReq *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamReq = r
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"provider/gemini-3.8-flash"}, Enabled: true})
	clientBody := `{"model":"gemini-3.8-flash","stream":false,"messages":[{"role":"system","content":"keep me"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(clientBody))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "test-agent/1.0")
	r.Header.Set("X-Title", "ClientApp")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(clientBody), &expected); err != nil {
		t.Fatal(err)
	}
	expected["model"] = "provider/gemini-3.8-flash"
	wantBody, _ := json.Marshal(expected)
	if string(upstreamBody) != string(wantBody) {
		t.Fatalf("non-ag body mutated:\n got: %s\nwant: %s", upstreamBody, wantBody)
	}
	m := decodeUseNumber(t, upstreamBody)
	if _, ok := m["system"]; !ok && m["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Fatal("non-ag system message was stripped")
	}
	if upstreamReq.Header.Get("User-Agent") != "test-agent/1.0" || upstreamReq.Header.Get("X-Title") != "ClientApp" {
		t.Fatal("non-ag client headers were scrubbed")
	}
	if w.Body.String() != reply {
		t.Fatalf("non-ag response not byte-identical:\n got: %s\nwant: %s", w.Body.String(), reply)
	}
}

func TestAntigravityEmbeddingsUntouched(t *testing.T) {
	const reply = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"ag/text-embed"}`
	var upstreamBody []byte
	var upstreamReq *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamReq = r
		var m map[string]any
		if err := json.Unmarshal(upstreamBody, &m); err != nil || m["input"] == nil {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"input: field required"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	defer api.Close()
	p := protocolGateway(t, &config.Upstream{ID: "up", Type: "openai", BaseURL: api.URL, APIKeys: []string{"up-key"}, Models: []string{"ag/text-embed"}, Enabled: true})
	clientBody := `{"model":"ag/text-embed","input":["hello"]}`
	r := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(clientBody))
	r.Header.Set("Authorization", "Bearer client-key")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "test-agent/9")
	r.Header.Set("X-Title", "EmbedClient")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(clientBody), &expected); err != nil {
		t.Fatal(err)
	}
	expected["model"] = "ag/text-embed" // pre-existing model rewrite only
	var gotBody map[string]any
	if err := json.Unmarshal(upstreamBody, &gotBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotBody, expected) {
		t.Fatalf("embeddings body mutated:\n got: %s\nwant: %s", upstreamBody, expected)
	}
	if upstreamReq.Header.Get("User-Agent") != "test-agent/9" || upstreamReq.Header.Get("X-Title") != "EmbedClient" {
		t.Fatal("embeddings headers were scrubbed")
	}
	if w.Body.String() != reply {
		t.Fatalf("embeddings response not byte-identical: %s", w.Body.String())
	}
}
