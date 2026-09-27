package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExplicitCodeGenericTypeForwarding(t *testing.T) {
	for _, status := range []int{400, 401} {
		for _, translated := range []bool{false, true} {
			for _, tc := range []struct {
				code, message, category string
				deleted                 bool
			}{
				{"invalid_api_key", "Incorrect API key provided", "auth", true},
				{"invalid_api_key", "Unknown field: invalid_api_key", "request", false},
				{"insufficient_quota", "authentication failed", "limit", false},
			} {
				t.Run(fmt.Sprintf("%d/%v/%s/%s", status, translated, tc.code, tc.category), func(t *testing.T) {
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":%q,"message":%q}}`, tc.code, tc.message)
					}))
					defer server.Close()
					p := hitchanceProxy(t, server.URL, []string{"test-key"})
					r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
					if translated {
						rememberHitchanceMessages(p, r, "test-key")
					}
					fr, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", r, []byte(`{"model":"model","messages":[],"max_tokens":10}`), &bytes.Buffer{}, httptest.NewRecorder())
					if err != nil {
						t.Fatal(err)
					}
					upstreams := p.Cfg.Get().Upstreams
					if fr.hitchance.Category != tc.category || (len(upstreams) == 0) != tc.deleted || calls != 1 {
						t.Fatalf("decision=%+v upstreams=%d calls=%d", fr.hitchance, len(upstreams), calls)
					}
				})
			}
		}
	}
}
