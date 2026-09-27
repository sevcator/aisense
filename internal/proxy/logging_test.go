package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/store"
)

func TestDynamicLoggingFullPromptsResponsesAndSSE(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic"} {
		for _, mode := range []string{"json", "sse", "failure"} {
			t.Run(typ+"/"+mode, func(t *testing.T) {
				prompt := strings.Repeat("full-prompt-", 30000) + "prompt-end"
				answer := strings.Repeat("full-response-", 220000) + "response-end"
				payload, _ := json.Marshal(map[string]any{"content": answer, "api_key": "response-key-secret"})
				wire := string(payload)
				if mode == "sse" {
					wire = "data: " + wire + "\n\ndata: [DONE]\n\n"
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					input, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(input), prompt) {
						t.Error("upstream prompt truncated")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Set-Cookie", "response-cookie-secret")
					if mode == "sse" {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					if mode == "failure" {
						w.WriteHeader(503)
					}
					for start := 0; start < len(wire); start += 17003 {
						end := min(start+17003, len(wire))
						_, _ = io.WriteString(w, wire[start:end])
						w.(http.Flusher).Flush()
					}
				}))
				defer up.Close()
				dir := t.TempDir()
				cfg, err := config.Load(filepath.Join(dir, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := cfg.Update(func(c *config.Config) error {
					c.APIKeys = []*config.APIKey{{ID: "client", Key: "client-key-secret", Enabled: true}}
					c.Upstreams = []*config.Upstream{{ID: "upstream", Type: typ, BaseURL: up.URL, Models: []string{"model"}, Enabled: true, APIKeys: []string{"upstream-key-secret"}}}
					c.Hitchance.Enabled = false
					c.Hitchance.Enabled = false // Isolate logging toggles from persistent circuit-breaker state.
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				usage, err := store.New(filepath.Join(dir, "usage.json"))
				if err != nil {
					t.Fatal(err)
				}
				defer usage.Close()
				trace := debuglog.NewDynamic(dir, "", func() bool { return cfg.Get().LoggingEnabled })
				defer trace.Close()
				if err := cfg.SetLoggingValidator(trace.Validate); err != nil {
					t.Fatal(err)
				}
				p := New(cfg, usage, trace)
				run := func() {
					body, _ := json.Marshal(map[string]any{"model": "model", "stream": mode != "json", "messages": []any{map[string]any{"role": "user", "content": prompt}}, "api_key": "input-key-secret"})
					req := httptest.NewRequest("POST", "/v1/chat/completions?api_key=query-key-secret", strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer client-key-secret")
					req.Header.Set("Cookie", "request-cookie-secret")
					res := httptest.NewRecorder()
					if typ == "openai" {
						p.ServeOpenAI(res, req)
					} else {
						p.ServeAnthropic(res, req)
					}
					if mode != "failure" && (res.Code != 200 || !strings.Contains(res.Body.String(), answer)) {
						t.Fatalf("response incomplete: status %d", res.Code)
					}
				}
				run()
				if _, err := os.Stat(trace.Path()); !os.IsNotExist(err) {
					t.Fatal("disabled proxy created log")
				}
				if err := cfg.Update(func(c *config.Config) error { c.LoggingEnabled = true; return nil }); err != nil {
					t.Fatal(err)
				}
				run()
				b, err := os.ReadFile(trace.Path())
				if err != nil {
					t.Fatal(err)
				}
				text := string(b)
				if !strings.Contains(text, prompt) || !strings.Contains(text, answer) {
					t.Fatal("full prompt/response missing from log")
				}
				if strings.Count(text, `"event":"request.received"`) != 1 {
					t.Fatal("duplicate or missing request trace")
				}
				for _, secret := range []string{"client-key-secret", "upstream-key-secret", "response-key-secret", "input-key-secret", "query-key-secret", "response-cookie-secret", "request-cookie-secret"} {
					if strings.Contains(text, secret) {
						t.Fatalf("leaked %s", secret)
					}
				}
				if err := cfg.Update(func(c *config.Config) error { c.LoggingEnabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
				run()
				after, _ := os.ReadFile(trace.Path())
				if string(after) != text {
					t.Fatal("disabled proxy still logged")
				}
			})
		}
	}
}
