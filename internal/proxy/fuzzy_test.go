package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aisense/internal/config"
	"aisense/internal/store"
)

func TestFuzzyRoutingAndPolicy(t *testing.T) {
	const target = "abcdefghijklmnopqrst"
	const typo = "abcdefghijklmnopqrsx"
	for _, tc := range []struct {
		name                     string
		models, allowed, blocked []string
		wildcard                 bool
		status                   int
		want                     string
	}{
		{"typo", []string{target}, nil, nil, false, 200, target},
		{"query typo", []string{target}, nil, nil, false, 200, target},
		{"known beats wildcard", []string{target}, nil, nil, true, 200, target},
		{"exact beats fuzzy", []string{target, typo}, nil, nil, false, 200, typo},
		{"ambiguous", []string{target, "abcdefghijklmnopqrsy"}, nil, nil, false, 503, ""},
		{"target blocked", []string{target}, nil, []string{target}, true, 503, ""},
		{"request blocked", []string{target}, nil, []string{typo}, true, 503, ""},
		{"target disallowed", []string{target}, []string{typo}, nil, true, 403, ""},
		{"request disallowed", []string{target}, []string{target}, nil, true, 403, ""},
		{"both authorized", []string{target}, []string{target, typo}, nil, false, 200, target},
		{"no substitution", []string{target + "-mini"}, nil, nil, false, 503, ""},
		{"wildcard unchanged", []string{target + "-mini"}, nil, nil, true, 200, typo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan string, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Model == "" {
					body.Model = r.URL.Query().Get("model")
				}
				seen <- body.Model
				_, _ = w.Write([]byte(`{"choices":[]}`))
			}))
			defer server.Close()
			manager, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			err = manager.Update(func(c *config.Config) error {
				c.APIKeys = []*config.APIKey{{ID: "key", Key: "secret", Enabled: true, AllowedModels: tc.allowed}}
				c.Upstreams = []*config.Upstream{{ID: "known", Enabled: true, Type: "openai", BaseURL: server.URL, Models: tc.models, BlockedModels: tc.blocked, AuthMode: "none"}}
				if tc.wildcard {
					c.Upstreams = append([]*config.Upstream{{ID: "wildcard", Enabled: true, Type: "openai", BaseURL: server.URL + "/wild", Models: []string{"*"}, AuthMode: "none"}}, c.Upstreams...)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			usage, err := store.New(filepath.Join(t.TempDir(), "usage.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer usage.Close()
			gateway := New(manager, usage)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client/`+typo+`","messages":[],"reasoning_effort":"high"}`))
			if tc.name == "query typo" {
				req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model="+typo, strings.NewReader(`{"messages":[]}`))
			}
			req.Header.Set("Authorization", "Bearer secret")
			res := httptest.NewRecorder()
			gateway.ServeOpenAI(res, req)
			if res.Code != tc.status {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			select {
			case got := <-seen:
				if tc.want == "" || got != tc.want {
					t.Fatalf("forwarded %q, want %q", got, tc.want)
				}
			default:
				if tc.want != "" {
					t.Fatal("no request forwarded")
				}
			}
		})
	}
}

func TestFuzzyRoutesPreserveRawVariantAndAliasPolicy(t *testing.T) {
	const requested = "abcdefghijklmnopqrsx-thinking"
	const target = "provider/abcdefghijklmnopqrst-thinking"
	up := &config.Upstream{
		Models:       []string{"abcdefghijklmnopqrst"},
		ModelAliases: map[string][]string{"abcdefghijklmnopqrst": {target, "abcdefghijklmnopqrst", "abcdefghijklmnopqrst-high"}},
	}
	key := &config.APIKey{}
	got := fuzzyModelRoutes(up, key, requested, target)
	if len(got) != 1 || got[0] != target {
		t.Fatalf("variant substitution: %#v", got)
	}
	up.BlockedModels = []string{"abcdefghijklmnopqrst"}
	if got := fuzzyModelRoutes(up, key, requested, target); len(got) != 0 {
		t.Fatalf("blocked canonical routed: %#v", got)
	}
	up.BlockedModels = nil
	up.Models = []string{"custom-alias"}
	up.ModelAliases = map[string][]string{"custom-alias": {target}}
	key.AllowedModels = []string{requested, target}
	if got := fuzzyModelRoutes(up, key, requested, target); len(got) != 0 {
		t.Fatalf("disallowed canonical routed: %#v", got)
	}
}

func TestFuzzyResolutionExactAliasesAndGlobalAmbiguity(t *testing.T) {
	const requested = "abcdefghijklmnopqrsx"
	manager, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []bool{false, true} {
		err := manager.Update(func(c *config.Config) error {
			c.Upstreams = []*config.Upstream{
				{ID: "one", Enabled: true, Type: "openai", BaseURL: "http://unused.invalid/one", Models: []string{"abcdefghijklmnopqrst"}},
				{ID: "two", Enabled: true, Type: "openai", BaseURL: "http://unused.invalid/two", Models: []string{"abcdefghijklmnopqrsy"}},
			}
			if exact {
				c.Upstreams[1].Models = []string{"custom-alias"}
				c.Upstreams[1].ModelAliases = map[string][]string{"custom-alias": {requested}}
				c.Upstreams[1].BlockedModels = []string{requested}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		p := &Proxy{Cfg: manager}
		if got, ok := p.fuzzyRoutingModel("openai", requested); ok {
			t.Fatalf("exact=%v: unexpected fuzzy route %q", exact, got)
		}
	}
}
