package debuglog

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventRedactsSecretsAndKeepsPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aisense-debug.jsonl")
	l, err := New(path, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{
		"Authorization": {"Bearer top-secret"},
		"Cookie":        {"session=cookie-secret"},
		"Content-Type":  {"application/json"},
		"X-Request-ID":  {"visible-request"},
	}
	l.Event("trace-1", "request.received", map[string]any{
		"headers": RedactHeaders(headers),
		"url":     RedactURL("https://name:password@example.test/v1?code=secret-code&model=visible"),
		"body":    Body([]byte(`{"model":"visible-model","api_key":"body-secret","messages":[{"content":"hello"}]}`), "application/json"),
	})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, secret := range []string{"top-secret", "cookie-secret", "password", "secret-code", "body-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaked %q: %s", secret, text)
		}
	}
	for _, visible := range []string{"trace-1", "request.received", "visible-request", "visible-model", "hello"} {
		if !strings.Contains(text, visible) {
			t.Fatalf("log missing %q: %s", visible, text)
		}
	}
	var line map[string]any
	if err := json.Unmarshal(b, &line); err != nil {
		t.Fatalf("invalid JSONL event: %v", err)
	}
}

func TestDynamicLifecycleAndDateRotation(t *testing.T) {
	dir := t.TempDir()
	var enabled atomic.Bool
	l := NewDynamic(dir, "", enabled.Load)
	date := time.Date(2026, 9, 11, 23, 59, 0, 0, time.Local)
	l.now = func() time.Time { return date }
	l.Event("", "disabled", nil)
	if _, err := os.Stat(l.Path()); !os.IsNotExist(err) {
		t.Fatalf("disabled logger created file: %v", err)
	}
	if err := l.Validate(true); err != nil {
		t.Fatal(err)
	}
	enabled.Store(true)
	l.Event("", "first-day", nil)
	first := l.Path()
	date = date.Add(2 * time.Minute)
	l.Event("", "second-day", nil)
	second := l.Path()
	if filepath.Base(first) != "2026-09-11.log" || filepath.Base(second) != "2026-09-12.log" {
		t.Fatalf("paths: %s, %s", first, second)
	}
	before, _ := os.ReadFile(second)
	enabled.Store(false)
	l.Event("", "disabled", nil)
	after, _ := os.ReadFile(second)
	if string(before) != string(after) {
		t.Fatal("disabled logger wrote data")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				enabled.Store(j%2 == 0)
				l.Event("", "concurrent", nil)
				_ = l.Path()
			}
		}()
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := l.Validate(true); err == nil {
		t.Fatal("closed logger accepted enable")
	}
	b, _ := os.ReadFile(first)
	if !strings.Contains(string(b), "first-day") || strings.Contains(string(b), "second-day") {
		t.Fatal("incorrect rotation")
	}
}

func TestRawCompatibilityStillRedactsConfigAndSplitSSE(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.log")
	l, err := NewRaw(path, -1, 1)
	if err != nil {
		t.Fatal(err)
	}
	type credentials struct {
		APIKeys  []string `json:"api_keys"`
		Password string   `json:"password"`
		URL      string   `json:"base_url"`
	}
	l.Event("", "config", map[string]any{"config": credentials{[]string{"config-key-secret"}, "password-secret", "https://user:url-secret@example.test/?api_key=query-secret"}, "headers": http.Header{"Authorization": {"Bearer header-secret"}}})
	long := strings.Repeat("complete-prompt-", 10000)
	s := &Stream{Logger: l, Emit: func(body any) { l.Event("", "sse", map[string]any{"body": body}) }}
	s.Write([]byte(`data: {"api_key":"split-`))
	s.Write([]byte(`secret","delta":"` + long + "\"}\n\n"))
	s.Write([]byte("data: {\"api_key\":\n"))
	s.Write([]byte("data: \"multiline-secret\",\"delta\":\"visible\"}\r\n\r\n"))
	s.Close()
	l.Close()
	b, _ := os.ReadFile(path)
	for _, secret := range []string{"config-key-secret", "password-secret", "url-secret", "query-secret", "header-secret", "split-secret", "multiline-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	if !strings.Contains(string(b), long) {
		t.Fatal("long stream payload truncated")
	}
}

func TestLoggerRotatesWithoutDroppingCurrentEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aisense-debug.jsonl")
	l, err := New(path, 300, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		l.Event("trace", "chunk", map[string]any{"body": strings.Repeat("x", 80), "index": i})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	matches, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 2 {
		t.Fatalf("too many backup logs: %v", matches)
	}
}

func TestNegativeMaxSizeKeepsCompleteUnboundedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	l, err := NewRaw(path, -1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		l.Event("trace", "chunk", map[string]any{"body": strings.Repeat("x", 80), "index": i})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("unbounded logger unexpectedly rotated: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(body), "\n"); lines != 20 {
		t.Fatalf("unbounded logger retained %d events, want 20", lines)
	}
}
