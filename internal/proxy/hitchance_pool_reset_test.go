package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHitchanceResetClearsExplicitRuntimePoolCooldown(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(openaiReply)) }))
	defer s.Close()
	p := hitchanceProxy(t, s.URL, []string{"a"})
	up := p.Cfg.Get().Upstreams[0]
	p.markUpstreamKeyFailure(up, "a", 429, http.Header{})
	if w := hitchanceRequest(p); w.Code != 503 || calls != 0 {
		t.Fatalf("cooldown bypassed: %d calls=%d", w.Code, calls)
	}
	p.ResetHitchanceKeys(up)
	if w := hitchanceRequest(p); w.Code != 200 || calls != 1 {
		t.Fatalf("reset ineffective: %d calls=%d", w.Code, calls)
	}
}
