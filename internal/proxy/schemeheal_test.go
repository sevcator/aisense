package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

// An upstream configured with an https:// base whose server actually speaks
// plain HTTP dies in the transport before any policy can react. The proxy
// retries over the opposite scheme, remembers it, and serves the request.
func TestSchemeHealRetriesOverWorkingProtocol(t *testing.T) {
	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer api.Close()

	p, _ := newTestGateway(t, func(c *config.Config) {
		// The config lies about the scheme: https:// pointed at a plain HTTP server.
		c.Upstreams = []*config.Upstream{{ID: "wrong-scheme", Enabled: true, Type: "openai", BaseURL: "https://" + strings.TrimPrefix(api.URL, "http://"), Models: []string{"glm-x"}, AuthMode: "none"}}
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-x","messages":[]}`))
	req.Header.Set("Authorization", "Bearer gateway-key")
	res := httptest.NewRecorder()
	p.ServeOpenAI(res, req)
	if res.Code != 200 {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if calls == 0 {
		t.Fatal("upstream was never asked")
	}
	// The healed scheme is remembered and applied to the next request.
	if scheme, ok := p.healedScheme("wrong-scheme"); !ok || scheme != "http" {
		t.Fatalf("healed scheme = %q ok=%v", scheme, ok)
	}
	res = httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-x","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer gateway-key")
	p.ServeOpenAI(res, req2)
	if res.Code != 200 {
		t.Fatalf("second request status = %d", res.Code)
	}
}

func TestSchemeMismatchErrorDetection(t *testing.T) {
	yes := []string{
		`Post "https://x/v1/chat/completions": http: server gave HTTP response to HTTPS client`,
		`Get "https://x": tls: first record does not look like a TLS handshake`,
	}
	for _, s := range yes {
		if !schemeMismatchError(errString(s)) {
			t.Fatalf("missed: %s", s)
		}
	}
	if schemeMismatchError(errString("connection refused")) {
		t.Fatal("false positive on a plain network error")
	}
}

func TestSwappedScheme(t *testing.T) {
	if got := swappedScheme("https://h:8080/v1"); got != "http://h:8080/v1" {
		t.Fatalf("https swap = %q", got)
	}
	if got := swappedScheme("http://h/v1"); got != "https://h/v1" {
		t.Fatalf("http swap = %q", got)
	}
	if got := swappedScheme("ftp://h"); got != "" {
		t.Fatalf("unknown scheme swap = %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
