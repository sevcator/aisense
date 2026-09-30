package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/store"
)

func TestLastResponseLoggingWaitsForConversationIdle(t *testing.T) {
	oldIdle := lastResponseIdle
	lastResponseIdle = 80 * time.Millisecond
	defer func() { lastResponseIdle = oldIdle }()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"first-answer"}}]}`)
		} else {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"last-answer"}}]}`)
		}
	}))
	defer up.Close()
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "client", Key: "client-secret", Enabled: true}}
		c.Upstreams = []*config.Upstream{{ID: "up", BaseURL: up.URL, Models: []string{"model"}, Enabled: true, APIKeys: []string{"provider-secret"}}}
		c.Hitchance.Enabled = false
		c.LoggingEnabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := store.New(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	logger := debuglog.NewLastResponseDynamic(dir, "", func() bool { return cfg.Get().LoggingEnabled })
	defer logger.Close()
	p := New(cfg, usage, logger)
	request := func(answer string) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"private-prompt"}]}`))
		r.Header.Set("Authorization", "Bearer client-secret")
		w := httptest.NewRecorder()
		p.ServeOpenAI(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), answer) {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
	}
	request("first-answer")
	request("last-answer")
	deadline := time.Now().Add(time.Second)
	var logText string
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logger.Path())
		logText = string(data)
		if strings.Contains(logText, "last-answer") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logText, "last-answer") || strings.Contains(logText, "first-answer") || strings.Contains(logText, "private-prompt") || strings.Contains(logText, "client-secret") || strings.Contains(logText, "provider-secret") {
		t.Fatalf("unexpected last-response log: %s", logText)
	}
	if strings.Count(logText, `"event":"conversation.last_response"`) != 1 {
		t.Fatalf("expected one last response: %s", logText)
	}
}

func TestConversationIDUsesFirstUserMessage(t *testing.T) {
	a := conversationID([]byte(`{"model":"m","messages":[{"role":"system","content":"same"},{"role":"user","content":"one"}]}`), "client")
	b := conversationID([]byte(`{"model":"m","messages":[{"role":"system","content":"same"},{"role":"user","content":"two"}]}`), "client")
	if a == b {
		t.Fatal("separate conversations share an ID")
	}
	first := conversationID([]byte(`{"model":"m","input":"hello"}`), "client")
	followup := conversationID([]byte(`{"model":"m","input":"next","previous_response_id":"response-1"}`), "client")
	if first != followup {
		t.Fatal("follow-up without a message history was not grouped")
	}
}
