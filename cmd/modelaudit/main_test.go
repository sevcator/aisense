package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		body            string
		status          int
		outcome, reason string
	}{
		{`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`, 200, "succeeded", "generated"},
		{`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`, 200, "succeeded", "accepted_token_cap_empty"},
		{`{"error":{"message":"invalid api key"}}`, 200, "failed", "key_auth"},
		{`{"error":{"message":"unknown model"}}`, 404, "failed", "unsupported_or_unavailable_model"},
		{`{"code":500,"message":"oops"}`, 200, "failed", "unrecognized_success_envelope"},
		{`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, 200, "failed", "accepted_but_empty_response"},
		{`data: {"error":"bad"}`, 200, "failed", "unexpected_sse"},
		{`<html>Bad gateway</html>`, 200, "failed", "non_json_response"},
		{`{"choices":"unexpected"}`, 200, "failed", "unexpected_json_shape"},
		{`{"error":{"type":"invalid_request_error","message":"Unable to determine provider for model test"}}`, 200, "failed", "provider_resolution_failed"},
		{`{"error":{"message":"Model test is not available in provider catalog"}}`, 200, "failed", "unsupported_or_unavailable_model"},
		{`{"error":{"message":"Invalid temperature for model test"}}`, 400, "failed", "logical_error_envelope"},
		{`{"error":{"message":"Unable to determine provider for model test: invalid api key"}}`, 200, "failed", "key_auth"},
		{`{"choices":[{"message":{"content":"Unable to determine provider for model test"}}]}`, 200, "succeeded", "generated"},
	} {
		o, r := classify(tc.status, []byte(tc.body), "chat/completions")
		if o != tc.outcome || r != tc.reason {
			t.Fatalf("got %s/%s, want %s/%s", o, r, tc.outcome, tc.reason)
		}
	}
}

func TestProbeResponseSizeLimit(t *testing.T) {
	for _, size := range []int{2 << 20, (2 << 20) + 1} {
		body := `{"choices":[{"message":{"content":"OK"}}]}`
		body += strings.Repeat(" ", size-len(body))
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		u := &config.Upstream{Type: "openai", BaseURL: s.URL}
		a := probe(s.Client(), u, "test-model", "", 0)
		s.Close()
		want := "generated"
		if size > 2<<20 {
			want = "response_size_limit_exceeded"
		}
		if a.Reason != want {
			t.Fatalf("size %d: got %s, want %s", size, a.Reason, want)
		}
	}
}

func TestAnalyzeLogDoesNotEchoPrivateFields(t *testing.T) {
	var out bytes.Buffer
	if err := analyzeLog("testdata/private-log.jsonl", &out); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE", "upstream_id", "trace_id", "prompt", "message"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("analysis leaked %s", secret)
		}
	}
	if !strings.Contains(out.String(), `"provider_resolution_failed":1`) {
		t.Fatal(out.String())
	}
}

func TestPlanRetainsAdvertisedAliasesAndEnabledOnly(t *testing.T) {
	c := config.Config{Upstreams: []*config.Upstream{
		{ID: "on", Enabled: true, Models: []string{"canonical", "variant-fast", "*"}, ModelAliases: map[string][]string{"canonical": {"provider/raw-thinking", "provider/raw"}}},
		{ID: "off", Enabled: false, Models: []string{"disabled-model"}},
	}}
	p := plan(&c)
	if len(p) != 3 {
		t.Fatal(len(p))
	}
	for _, x := range p {
		if x.Model == "canonical" && (len(x.Routes) != 2 || x.Routes[0] != "provider/raw-thinking") {
			t.Fatal("invented or lost aliases")
		}
		if x.Model == "*" && x.Skip != "wildcard_unenumerable" {
			t.Fatal("wildcard must skip")
		}
	}
}

func TestProbeTypes(t *testing.T) {
	for _, m := range []string{"nano-banana-2", "seedance-2.0", "qwen3-asr", "eleven-v3", "flux.2-max", "aura-2-en", "rerank-4-pro"} {
		if _, s := probeType(m); s == "" {
			t.Fatalf("unsafe chat probe: %s", m)
		}
	}
	for _, m := range []string{"mistral-embed", "nv-embedqa-e5-v5", "text-embedding-3-small"} {
		if e, s := probeType(m); e != "embeddings" || s != "" {
			t.Fatal(m)
		}
	}
}

func TestProbeAnthropicAndNoEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://user:secret@127.0.0.1:1")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
			t.Error("wrong anthropic request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))
	}))
	defer s.Close()
	u := &config.Upstream{Type: "anthropic", BaseURL: s.URL + "/v1"}
	client, skip := clientFor(&config.Config{}, u)
	if skip != "" {
		t.Fatal(skip)
	}
	defer client.CloseIdleConnections()
	a := probe(client, u, "claude-sonnet-4", "test-key", 1)
	if a.Outcome != "succeeded" {
		t.Fatal(a)
	}
}
