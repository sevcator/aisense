package proxy

import (
	"aisense/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForwardingPersistsInferenceHealth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		disabled bool
		want     string
	}{
		{"success", 200, openaiReply, false, "available"},
		{"disabled-success", 200, openaiReply, true, "available"},
		{"classified-failure", 429, `{"error":{"message":"rate limit"}}`, false, "unavailable"},
		{"validation", 400, `{"error":{"message":"Unknown field invalid_api_key"}}`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer s.Close()
			p := hitchanceProxy(t, s.URL, []string{"a"})
			if err := p.Cfg.Update(func(c *config.Config) error {
				c.Hitchance.Enabled = !tc.disabled
				c.Hitchance.CoolingRetrySeconds = 0 // a rate limit would otherwise be waited out
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			hitchanceRequest(p)
			got := p.Cfg.Get().Upstreams[0].Health
			if tc.want == "" {
				if got != nil {
					t.Fatalf("non-inference validation saved health: %+v", got)
				}
				return
			}
			if got == nil || got.Status != tc.want {
				t.Fatalf("saved health=%+v want=%s", got, tc.want)
			}
		})
	}
}
