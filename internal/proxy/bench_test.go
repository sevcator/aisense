package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

// benchGateway mirrors a working installation: 20 upstreams, 14 of them
// enabled, ~110 models spread over them, two keys each.
func benchGateway(b *testing.B) (*Proxy, *config.Manager) {
	b.Helper()
	gateway, manager := newBenchGateway(b, func(c *config.Config) {
		sizes := []int{5, 9, 9, 20, 8, 6, 9, 2, 1, 10, 1, 1, 1, 2, 1, 7, 2, 10, 1, 7}
		for i, size := range sizes {
			up := &config.Upstream{
				ID: fmt.Sprintf("up-%02d", i), Enabled: i < 14, Priority: 10, Type: "openai",
				BaseURL:  fmt.Sprintf("https://api%02d.example.com/v1", i),
				APIKeys:  []string{fmt.Sprintf("key-%02d-a", i), fmt.Sprintf("key-%02d-b", i)},
				AuthMode: "swap",
			}
			for m := 0; m < size; m++ {
				up.Models = append(up.Models, fmt.Sprintf("model-%02d-%02d", i, m))
			}
			// The model every benchmark asks for lives on several upstreams.
			up.Models = append(up.Models, "shared-model")
			c.Upstreams = append(c.Upstreams, up)
		}
	})
	return gateway, manager
}

func newBenchGateway(b *testing.B, mutate func(c *config.Config)) (*Proxy, *config.Manager) {
	b.Helper()
	manager, err := config.Load(b.TempDir() + "/config.json")
	if err != nil {
		b.Fatal(err)
	}
	if err := manager.Update(func(c *config.Config) error {
		c.APIKeys = []*config.APIKey{{ID: "key", Key: "gateway-key", Enabled: true}}
		mutate(c)
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	return New(manager, nil), manager
}

// Choosing the upstreams for one request.
func BenchmarkCandidates(b *testing.B) {
	gateway, _ := benchGateway(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if cands := gateway.candidates("openai", "shared-model", true, nil); len(cands) == 0 {
			b.Fatal("no candidates")
		}
	}
}

// The routes tried on one upstream for one request.
func BenchmarkCombinedModelRoutes(b *testing.B) {
	gateway, manager := benchGateway(b)
	up := manager.Get().Upstreams[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if routes := gateway.combinedModelRoutes(up, "shared-model", nil); len(routes) == 0 {
			b.Fatal("no routes")
		}
	}
}

// The model list a client receives.
func BenchmarkModelsList(b *testing.B) {
	gateway, _ := benchGateway(b)
	key := gateway.Cfg.Get().APIKeys[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := httptest.NewRecorder()
		gateway.writeModelsList(res, "openai", key)
		if res.Code != http.StatusOK {
			b.Fatal(res.Code)
		}
	}
}

// A whole request that finds no usable upstream: authentication, routing and
// the retry cycles, without any network.
func BenchmarkRequestWithoutUpstreams(b *testing.B) {
	gateway, _ := benchGateway(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing-model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		gateway.ServeOpenAI(httptest.NewRecorder(), req)
	}
}
