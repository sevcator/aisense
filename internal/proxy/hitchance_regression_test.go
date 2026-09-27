package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aisense/internal/protocol"
)

func TestHitchanceSSEEchoCannotRejectCredential(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"bad request\"},\"request\":{\"prompt\":\"invalid api key\"}}\n\n"
	for _, translated := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "translated"}[translated], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte(body))
			}))
			defer server.Close()
			p := hitchanceProxy(t, server.URL, []string{"valid"})
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if translated {
				rememberHitchanceMessages(p, r, "valid")
			}
			fr, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", r, []byte(`{"model":"model","messages":[],"max_tokens":10,"stream":true}`), &bytes.Buffer{}, httptest.NewRecorder())
			if err != nil {
				t.Fatal(err)
			}
			up := p.Cfg.Get().Upstreams[0]
			if fr.status == 401 || len(up.APIKeys) != 1 {
				t.Fatalf("echo rejected key: status=%d keys=%d", fr.status, len(up.APIKeys))
			}
			for target, s := range up.HitchanceState {
				if s.Category == "invalid_key" || strings.HasPrefix(target, "key:") {
					t.Fatalf("echo cooled key as invalid: %s %+v", target, s)
				}
			}
		})
	}
	if logicalErrorStatus([]byte(body)) == 401 {
		t.Fatal("logical status trusts echoed prompt")
	}
}

// Prime only capability discovery; requests still cross the real HTTP seam.
func rememberHitchanceMessages(p *Proxy, r *http.Request, key string) {
	up := p.Cfg.Get().Upstreams[0]
	b, _ := json.Marshal([]any{up.BaseURL, up.APIKeys, up.AuthMode, up.OAuth, up.UseProxy, p.Cfg.Get().Proxies, p.Cfg.Get().ModelDiscovery.IgnoreCertErrors, key, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")})
	protocol.Shared.Remember(string(b), protocol.Messages)
	protocol.Shared.Probe(context.Background(), string(b), protocol.Chat, func(context.Context, string, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})
}

func TestHitchanceLocalConversionCannotRejectCredential(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	p := hitchanceProxy(t, server.URL, []string{"valid"})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[],"invalid_api_key":true}`))
	r.Header.Set("Authorization", "Bearer gateway-test-key")
	rememberHitchanceMessages(p, r, "valid")
	w := httptest.NewRecorder()
	p.ServeOpenAI(w, r)
	up := p.Cfg.Get().Upstreams[0]
	if len(up.APIKeys) != 1 || !up.Enabled || len(up.HitchanceState) != 0 {
		t.Fatalf("local validation changed credential: keys=%d enabled=%v states=%v", len(up.APIKeys), up.Enabled, up.HitchanceState)
	}
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_api_key") {
		t.Fatalf("conversion not exercised: %d %s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("local validation made %d provider calls", calls.Load())
	}
}

func TestHitchanceConvertedUpstreamErrorsKeepProvenance(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid api key\"}}\n\n"))
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(401)
					w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				}
			}))
			defer server.Close()
			p := hitchanceProxy(t, server.URL, []string{"rejected"})
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			rememberHitchanceMessages(p, r, "rejected")
			fr, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", r, []byte(`{"model":"model","messages":[],"max_tokens":10,"stream":true}`), &bytes.Buffer{}, httptest.NewRecorder())
			if err != nil {
				t.Fatal(err)
			}
			if fr.committed || fr.status != 401 || len(p.Cfg.Get().Upstreams) != 0 {
				t.Fatalf("upstream rejection lost: result=%+v upstreams=%d", fr, len(p.Cfg.Get().Upstreams))
			}
		})
	}
}
