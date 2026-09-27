package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aisense/internal/config"
	"aisense/internal/hitchance"
)

func TestHitchanceR3MultilineSSEProviderEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		deleted              bool
	}{
		{"invalid-message", "data: {\"error\":\ndata: {\"message\":\"invalid api key\"}}\n\n", "auth", true},
		{"invalid-code", "data: {\"error\":\ndata: {\"code\":\"invalid_api_key\",\"message\":\"Rejected\"}}\n\n", "auth", true},
		{"quota", "data: {\"error\":\ndata: {\"code\":\"insufficient_quota\",\"message\":\"authentication failed\"}}\n\n", "limit", false},
		{"validation", "data: {\"error\":\ndata: {\"type\":\"invalid_request_error\",\"message\":\"invalid api key\"}}\n\n", "request", false},
	} {
		for _, translated := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/native", true: "/translated"}[translated], func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write([]byte(tc.body))
				}))
				defer server.Close()
				p := hitchanceProxy(t, server.URL, []string{"test-key"})
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				if translated {
					rememberHitchanceMessages(p, r, "test-key")
				}
				fr, err := p.forwardOnce(p.Cfg.Get().Upstreams[0], "openai", "/v1", r, []byte(`{"model":"model","messages":[],"max_tokens":10,"stream":true}`), &bytes.Buffer{}, httptest.NewRecorder())
				if err != nil {
					t.Fatal(err)
				}
				if fr.committed || fr.localFailure || fr.hitchance.Category != tc.category {
					t.Errorf("provider evidence lost: %+v", fr)
				}
				if (len(p.Cfg.Get().Upstreams) == 0) != tc.deleted {
					t.Errorf("deleted=%v want %v", len(p.Cfg.Get().Upstreams) == 0, tc.deleted)
				}
				if calls.Load() != 1 {
					t.Errorf("calls=%d", calls.Load())
				}
			})
		}
	}
	for _, body := range []string{
		"data: {\"error\":\ndata: {\"message\":\"invalid api key\"}}",    // no event terminator
		"data: {\"error\":\ndata: {\"message\":\"invalid api key\"}\n\n", // truncated JSON
		"event: error\ndata: invalid api key\n\n",
	} {
		if hasLogicalErrorEnvelope([]byte(body)) || hitchance.ErrorText([]byte(body)) != "" {
			t.Errorf("incomplete/synthetic error accepted: %q", body)
		}
	}
}

func TestHitchanceR2OriginalEvidenceSurvivesConversion(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		deleted              bool
	}{
		{"quota-code", `{"error":{"code":"insufficient_quota","message":"authentication failed"}}`, "limit", false},
		{"quota-type", `{"error":{"type":"insufficient_quota","message":"authentication failed"}}`, "limit", false},
		{"invalid-code", `{"error":{"code":"invalid_api_key","message":"Rejected"}}`, "auth", true},
		{"invalid-type", `{"error":{"type":"authentication_error","message":"Rejected"}}`, "auth", true},
	} {
		for _, translated := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/native", true: "/translated"}[translated], func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					want := "/v1/chat/completions"
					if translated {
						want = "/v1/messages"
					}
					if r.URL.Path != want {
						t.Errorf("path=%s want %s", r.URL.Path, want)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(401)
					w.Write([]byte(tc.body))
				}))
				defer server.Close()
				p := hitchanceProxy(t, server.URL, []string{"test-key"})
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[],"max_tokens":10}`))
				r.Header.Set("Authorization", "Bearer gateway-test-key")
				if translated {
					rememberHitchanceMessages(p, r, "test-key")
				}
				w := httptest.NewRecorder()
				p.ServeOpenAI(w, r)
				upstreams := p.Cfg.Get().Upstreams
				if (len(upstreams) == 0) != tc.deleted {
					t.Errorf("deletion=%v want %v; client body=%s", len(upstreams) == 0, tc.deleted, w.Body.String())
				}
				if !tc.deleted && len(upstreams) > 0 {
					state := upstreams[0].HitchanceState[config.HitchanceTarget("key", "test-key", "model")]
					if state.Category != tc.category {
						t.Errorf("category=%s want %s", state.Category, tc.category)
					}
				}
				if calls.Load() != 1 || w.Code != 503 {
					t.Errorf("calls=%d status=%d", calls.Load(), w.Code)
				}
			})
		}
	}
}

func TestHitchanceR1UpstreamValidationCannotDelete(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "custom-delete"}[custom], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer valid" {
					t.Error("wrong upstream credential")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"Unrecognized request argument supplied: invalid_api_key"}}`))
			}))
			defer server.Close()
			p := hitchanceProxy(t, server.URL, []string{"valid"})
			if custom {
				if err := p.Cfg.Update(func(c *config.Config) error {
					c.Hitchance.Rules = []hitchance.Rule{{ID: "custom", Messages: []string{"invalid_api_key"}, Category: "invalid_key", Action: "delete", Scope: "key", CooldownSeconds: 60}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[],"invalid_api_key":true}`))
			r.Header.Set("Authorization", "Bearer gateway-test-key")
			w := httptest.NewRecorder()
			p.ServeOpenAI(w, r)
			up := p.Cfg.Get().Upstreams[0]
			if len(up.APIKeys) != 1 || !up.Enabled || len(up.HitchanceState) != 0 {
				t.Fatalf("request validation changed valid key: keys=%d enabled=%v state=%v", len(up.APIKeys), up.Enabled, up.HitchanceState)
			}
			if w.Code != 400 || calls.Load() != 1 {
				t.Fatalf("validation should be terminal: status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}
