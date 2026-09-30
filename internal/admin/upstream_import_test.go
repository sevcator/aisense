package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"aisense/internal/config"
)

func TestImportFormats(t *testing.T) {
	for _, tc := range []struct {
		name, text, url, keys string
	}{
		{"exact short", "https://127.0.0.1:443/v1 sk-1234", "https://127.0.0.1:443/v1", "sk-1234"},
		{"exact none", "https://127.0.0.1:443/v1", "https://127.0.0.1:443/v1", ""},
		{"comma", "https://one.test/v1/,sk-1", "https://one.test/v1", "sk-1"},
		{"semicolon", "https://one.test/v1;sk-1", "https://one.test/v1", "sk-1"},
		{"tab", "https://one.test/v1\tsk-1", "https://one.test/v1", "sk-1"},
		{"blank lines", "https://one.test/v1\r\n\r\nsk-1", "https://one.test/v1", "sk-1"},
		{"labels", "base_url: https://one.test/v1\napi_key: x", "https://one.test/v1", "x"},
		{"human labels", "URL: https://one.test/v1\nAPI key: x", "https://one.test/v1", "x"},
		{"bearer", "https://one.test/v1 Bearer x", "https://one.test/v1", "x"},
		{"authorization", "https://one.test/v1\nAuthorization: Bearer x", "https://one.test/v1", "x"},
		{"key first", "api_key: x\n\nURL: https://one.test/v1", "https://one.test/v1", "x"},
		{"omniroute", "@url(https://one.test/v1) sk-1", "https://one.test/v1", "sk-1"},
		{"omniroute quotes", "@url:`https://one.test/v1\u201c sk-1", "https://one.test/v1", "sk-1"},
		{"markdown", "```text\n- `https://one.test/v1` `sk-1`\n```", "https://one.test/v1", "sk-1"},
		{"ipv6", "http://[::1]:443/v1 sk-1", "http://[::1]:443/v1", "sk-1"},
		{"ipv6 root", "http://[::1]", "http://[::1]", ""},
		{"padding", "https://one.test/v1 api_key: ab==", "https://one.test/v1", "ab=="},
		{"pool lines", "https://one.test/v1 sk-1\napi_key: sk-2\nsk-1", "https://one.test/v1", "sk-1,sk-2"},
		{"json", `{"base_url":"https://one.test/v1/","api_keys":["x","y","x",""]}`, "https://one.test/v1", "x,y"},
		{"json token", `{"endpoint":"https://one.test/v1","token":"x"}`, "https://one.test/v1", "x"},
		{"json empty", `{"url":"https://one.test/v1","api_key":"","api_keys":[]}`, "https://one.test/v1", ""},
		{"json no key", "```json\n{\"url\":\"https://one.test/v1\"}\n```", "https://one.test/v1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseUpstreamImports(tc.text)
			if len(r.Skipped) != 0 || len(r.Upstreams) != 1 {
				t.Fatalf("unexpected result: %+v", r)
			}
			u := r.Upstreams[0]
			mode := "none"
			if tc.keys != "" {
				mode = "swap"
			}
			if u.BaseURL != tc.url || strings.Join(u.APIKeys, ",") != tc.keys || u.AuthMode != mode || u.Type != "auto" || !u.Enabled {
				t.Fatalf("unexpected upstream: %+v", u)
			}
		})
	}
}

func TestImportURLLabelFormats(t *testing.T) {
	for _, tc := range []struct {
		name, text, wantURL, wantKeys string
	}{
		// APIBASE: label, scheme-less value
		{"apibase no scheme", "KEY: sk-1\nAPIBASE: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// APIBASE: label, value already has scheme
		{"apibase with scheme", "KEY: sk-1\nAPIBASE: https://api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// APIBASE label reversed (URL first)
		{"apibase url first", "APIBASE: api.example.com/v1\nKEY: sk-1", "https://api.example.com/v1", "sk-1"},
		// BASE_URL: label
		{"base_url label", "BASE_URL: api.example.com/v1\nKEY: sk-1", "https://api.example.com/v1", "sk-1"},
		// API_BASE: label
		{"api_base label", "KEY: sk-1\nAPI_BASE: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// ENDPOINT: label
		{"endpoint label", "KEY: sk-1\nENDPOINT: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// OPENAI_BASE_URL label
		{"openai_base_url", "KEY: sk-1\nOPENAI_BASE_URL: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// OPENAI_API_BASE label
		{"openai_api_base", "KEY: sk-1\nOPENAI_API_BASE: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// IP:port without scheme
		{"ip port no scheme", "KEY: sk-1\nAPIBASE: 152.136.138.192:8080/v1", "https://152.136.138.192:8080/v1", "sk-1"},
		// Mixed single-line: KEY: x APIBASE: y
		{"inline key apibase", "KEY: sk-1 APIBASE: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// Mixed single-line with scheme
		{"inline key apibase scheme", "KEY: sk-1 APIBASE: https://api.example.com/v1", "https://api.example.com/v1", "sk-1"},
		// Mixed single-line ip:port
		{"inline key ip port", "KEY: sk-1 APIBASE: 192.168.0.1:9000/v1", "https://192.168.0.1:9000/v1", "sk-1"},
		// Multiple entries, one per line in inline format
		{"multi pair", "KEY: sk-1 APIBASE: a.test/v1\nKEY: sk-2 APIBASE: b.test/v1", "https://a.test/v1,https://b.test/v1", "sk-1,sk-2"},
		// Plain text key label without "api_" prefix
		{"bare key label", "key: sk-1\nAPIBASE: api.example.com/v1", "https://api.example.com/v1", "sk-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseUpstreamImports(tc.text)
			if len(r.Skipped) != 0 {
				t.Fatalf("unexpected skips: %v", r.Skipped)
			}
			if tc.name == "multi pair" {
				// special multi-entry check
				if len(r.Upstreams) != 2 {
					t.Fatalf("want 2 upstreams, got %d: %+v", len(r.Upstreams), r.Upstreams)
				}
				urls := r.Upstreams[0].BaseURL + "," + r.Upstreams[1].BaseURL
				keys := strings.Join(r.Upstreams[0].APIKeys, ",") + "," + strings.Join(r.Upstreams[1].APIKeys, ",")
				if urls != tc.wantURL || keys != tc.wantKeys {
					t.Fatalf("got urls=%q keys=%q", urls, keys)
				}
				return
			}
			if len(r.Upstreams) != 1 {
				t.Fatalf("want 1 upstream, got %d: %+v", len(r.Upstreams), r.Upstreams)
			}
			u := r.Upstreams[0]
			if u.BaseURL != tc.wantURL || strings.Join(u.APIKeys, ",") != tc.wantKeys {
				t.Fatalf("got url=%q keys=%q, want url=%q keys=%q", u.BaseURL, u.APIKeys, tc.wantURL, tc.wantKeys)
			}
		})
	}
}

func TestImportMixedEntriesAndOwnership(t *testing.T) {
	for _, text := range []string{
		"https://a.test/v1,sk-1; https://b.test/v1\n\nhttps://c.test/v1\napi_key: sk-3\nhttps://d.test/v1 https://e.test/v1 sk-5",
		`[{"url":"https://a.test/v1","api_key":"sk-1"},{"url":"https://b.test/v1"},{"endpoint":"https://c.test/v1","token":"sk-3"},{"base_url":"https://d.test/v1"},{"url":"https://e.test/v1","api_keys":["sk-5"]}]`,
	} {
		r := parseUpstreamImports(text)
		if len(r.Skipped) != 0 || len(r.Upstreams) != 5 {
			t.Fatalf("unexpected result: %+v", r)
		}
		for i, want := range []string{"sk-1", "", "sk-3", "", "sk-5"} {
			if strings.Join(r.Upstreams[i].APIKeys, ",") != want {
				t.Fatalf("entry %d has wrong credentials", i)
			}
		}
	}
}

func TestImportRejectsMalformedAndSanitizesErrors(t *testing.T) {
	for _, text := range []string{
		"ftp://one.test/v1 sk-secret", "https:// sk-secret", "https://one.test:bad sk-secret",
		"https://one.test:99999 sk-secret", "https://one.test: sk-secret",
		"https://user:sk-secret@one.test/v1", "https://one.test/?key=sk-secret",
		"https://one.test/#sk-secret", "https://one.test/%zz sk-secret",
		"one.test/v1 sk-secret", "https:/one.test sk-secret",
		"https://one.test/v1 contact administrator for access", "https://one.test/v1 documentation",
		"https://one.test/v1\napi_key:", "https://one.test/v1\napi_key: \"\"",
		"https://one.test/v1\nBearer", "api_key: sk-secret",
		`{"url":"https://one.test/v1","api_key":123}`, `{"url":"https://one.test/v1","api_keys":"sk-secret"}`,
		`{"url":"https://one.test/v1","api_key":null}`,
		`{"url":"https://one.test/v1","base_url":"https://two.test/v1","api_key":"sk-secret"}`,
		`[{"url":"https://one.test/v1","api_key":"sk-secret"}`,
	} {
		r := parseUpstreamImports(text)
		if len(r.Upstreams) != 0 || len(r.Skipped) == 0 {
			t.Fatalf("malformed input accepted: %+v", r)
		}
		data, _ := json.Marshal(r.Skipped)
		if strings.Contains(string(data), "sk-secret") {
			t.Fatal("error leaked credential")
		}
	}
	r := parseUpstreamImports("https://bad.test/v1\napi_key:\nhttps://good.test/v1 sk-1")
	if len(r.Upstreams) != 1 || r.Upstreams[0].BaseURL != "https://good.test/v1" || len(r.Skipped) != 1 {
		t.Fatalf("bad entry contaminated next: %+v", r)
	}
	r = parseUpstreamImports(`[{"url":"ftp://bad.test","token":"sk-secret"},{"url":"https://good.test"},false]`)
	if len(r.Upstreams) != 1 || len(r.Skipped) != 2 {
		t.Fatalf("JSON partial import: %+v", r)
	}
}

func TestImportDuplicatesPreservePersistedSettingsOffline(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "none first", true: "key first"}[reverse], func(t *testing.T) {
			s, cfg := testServer(t)
			persisted := &config.Upstream{ID: "existing", Name: "Keep", Type: "anthropic", BaseURL: "https://existing.invalid/v1", AuthMode: "none", APIKeys: []string{"old"}, Priority: 27, Models: []string{"kept"}, Enabled: false, UseProxy: true}
			unrelated := &config.Upstream{ID: "unrelated", Type: "auto", BaseURL: "https://unrelated.invalid", AuthMode: "swap", APIKeys: []string{"untouched"}, Models: []string{"*"}, Enabled: true}
			if err := cfg.Update(func(c *config.Config) error {
				c.ModelDiscovery.Enabled, c.AutoModelsDiscovery = false, false
				c.Upstreams = []*config.Upstream{persisted, unrelated}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := *cfg.Get().Upstreams[0]
			other := *cfg.Get().Upstreams[1]
			pair := []string{"https://127.0.0.1:443/v1", "https://127.0.0.1:443/v1/ sk-1234"}
			if reverse {
				pair[0], pair[1] = pair[1], pair[0]
			}
			text := strings.Join(pair, "\n") + "\nhttps://127.0.0.1:443/v1 sk-2\nhttps://existing.invalid/v1\nhttps://existing.invalid/v1 new-key"
			body, _ := json.Marshal(map[string]string{"text": text})
			w := httptest.NewRecorder()
			s.importUpstream(w, httptest.NewRequest("POST", "/", strings.NewReader(string(body))))
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			ups := cfg.Get().Upstreams
			if len(ups) != 3 {
				t.Fatalf("got %d upstreams", len(ups))
			}
			before.APIKeys = []string{"old", "new-key"}
			if !reflect.DeepEqual(before, *ups[0]) || !reflect.DeepEqual(other, *ups[1]) {
				t.Fatal("persisted settings changed")
			}
			if ups[2].AuthMode != "swap" || ups[2].Type != "auto" || strings.Join(ups[2].APIKeys, ",") != "sk-1234,sk-2" {
				t.Fatalf("wrong duplicate merge: %+v", ups[2])
			}
		})
	}
}

func TestImportQueuesDiscoveryWithoutWaitingEvenWhenPeriodicOff(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer endpoint.Close()
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.ModelDiscovery.Enabled, c.AutoModelsDiscovery = false, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"text": endpoint.URL + "/none\n" + endpoint.URL + "/key sk-1234"})
	w := httptest.NewRecorder()
	s.importUpstream(w, httptest.NewRequest("POST", "/", strings.NewReader(string(body))))
	var response map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || requests.Load() != 0 || response["models_refreshed"] != nil || response["models_refresh_error"] != nil {
		t.Fatalf("offline import: status=%d requests=%d", w.Code, requests.Load())
	}
	if string(response["models_discovery_queued"]) != "true" || len(s.discoveryPending) != 2 {
		t.Fatal("import did not queue discovery")
	}
	ups := cfg.Get().Upstreams
	if len(ups) != 2 || ups[0].AuthMode != "none" || len(ups[0].APIKeys) != 0 || ups[1].AuthMode != "swap" {
		t.Fatal("incorrect auth defaults")
	}
}

func TestSingleAddDefaultsWithoutKeys(t *testing.T) {
	for _, keys := range []string{`[]`, `["sk-1234"]`} {
		s, cfg := testServer(t)
		if err := cfg.Update(func(c *config.Config) error {
			c.ModelDiscovery.Enabled, c.AutoModelsDiscovery = false, false
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.upsertUpstream(w, httptest.NewRequest("POST", "/?mode=create", strings.NewReader(`{"base_url":"https://127.0.0.1:443/v1","api_keys":`+keys+`,"enabled":true}`)))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		u := cfg.Get().Upstreams[0]
		want := "none"
		if keys != `[]` {
			want = "swap"
		}
		if u.AuthMode != want || u.Type != "auto" {
			t.Fatalf("wrong defaults: %+v", u)
		}
	}
}

func TestImportPipeModelLists(t *testing.T) {
	r := parseUpstreamImports("http://13.211.156.204:80/v1 sk-7e91d1d6a8301f27-3ww9ar-60d6de78 | cbai/glm-5.1,cbai/glm-5.2,cf/@cf/meta/llama-3.2-1b-instruct")
	if len(r.Skipped) != 0 || len(r.Upstreams) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	u := r.Upstreams[0]
	if u.BaseURL != "http://13.211.156.204:80/v1" || strings.Join(u.APIKeys, ",") != "sk-7e91d1d6a8301f27-3ww9ar-60d6de78" || u.AuthMode != "swap" || u.Type != "auto" || !u.Enabled {
		t.Fatalf("unexpected upstream: %+v", u)
	}
	// Canonical Models with raw provider-prefixed aliases, like discovery.
	for raw, canonical := range map[string]string{
		"cbai/glm-5.1":                      "glm-5.1",
		"cbai/glm-5.2":                      "glm-5.2",
		"cf/@cf/meta/llama-3.2-1b-instruct": "llama-3.2.1b-instruct",
	} {
		if !slices.Contains(u.Models, canonical) {
			t.Fatalf("canonical %q missing from Models %v", canonical, u.Models)
		}
		if !slices.Contains(u.ModelAliases[canonical], raw) {
			t.Fatalf("raw alias %q missing under %q: %v", raw, canonical, u.ModelAliases[canonical])
		}
	}
	if len(u.Models) != 3 {
		t.Fatalf("unexpected model count: %v", u.Models)
	}
	// Trailing bracketed variant must survive verbatim in the raw aliases.
	r = parseUpstreamImports("http://one.test/v1 sk-1 | oc-prod/claude-fable-5,oc-prod/claude-fable-5[1M]")
	if len(r.Skipped) != 0 || len(r.Upstreams) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	allAliases := map[string]bool{}
	for _, list := range r.Upstreams[0].ModelAliases {
		for _, raw := range list {
			allAliases[raw] = true
		}
	}
	if !allAliases["oc-prod/claude-fable-5"] || !allAliases["oc-prod/claude-fable-5[1M]"] || len(allAliases) != 2 {
		t.Fatalf("bracketed model lost: %+v", r.Upstreams[0].ModelAliases)
	}
	// Continuation key lines still pool into the same entry.
	r = parseUpstreamImports("http://one.test/v1 sk-1 | m1\napi_key: sk-2")
	if len(r.Skipped) != 0 || len(r.Upstreams) != 1 || strings.Join(r.Upstreams[0].APIKeys, ",") != "sk-1,sk-2" {
		t.Fatalf("continuation keys broken: %+v", r)
	}
	// A pipe with an empty segment keeps the wildcard default.
	r = parseUpstreamImports("http://one.test/v1 sk-1 |")
	if len(r.Skipped) != 0 || len(r.Upstreams) != 1 || strings.Join(r.Upstreams[0].Models, ",") != "*" {
		t.Fatalf("empty model segment broken: %+v", r)
	}
	// Legacy formats without a pipe are unchanged.
	r = parseUpstreamImports("https://one.test/v1 sk-1")
	if len(r.Skipped) != 0 || len(r.Upstreams) != 1 || strings.Join(r.Upstreams[0].Models, ",") != "*" || r.Upstreams[0].ModelAliases != nil {
		t.Fatalf("legacy format changed: %+v", r.Upstreams[0])
	}
}

func TestImportNoAuthPlaceholder(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"url line", "http://206.183.130.97:8081/v1 <no-auth-needed> | qwen36,qwen36-128k"},
		{"url line upper", "http://206.183.130.97:8081/v1 <NO-AUTH-NEEDED> | qwen36"},
		{"own line", "http://206.183.130.97:8081/v1\n<no-auth-needed>"},
		{"labeled line", "http://206.183.130.97:8081/v1\napi_key: <no-auth-needed>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseUpstreamImports(tc.text)
			if len(r.Skipped) != 0 || len(r.Upstreams) != 1 {
				t.Fatalf("unexpected result: %+v", r)
			}
			u := r.Upstreams[0]
			if u.AuthMode != "none" || len(u.APIKeys) != 0 {
				t.Fatalf("no-auth entry got credentials: %+v", u)
			}
			if u.BaseURL != "http://206.183.130.97:8081/v1" || !u.Enabled {
				t.Fatalf("unexpected upstream: %+v", u)
			}
		})
	}
	r := parseUpstreamImports("http://206.183.130.97:8081/v1 <no-auth-needed> | qwen36,qwen36-128k")
	if len(r.Upstreams) != 1 || len(r.Upstreams[0].Models) != 2 || len(r.Upstreams[0].ModelAliases["qwen36"]) != 0 {
		t.Fatalf("no-auth models missing: %+v", r.Upstreams[0])
	}
}

func TestImportRawModelsParallelSlice(t *testing.T) {
	r := parseUpstreamImports("http://a.test/v1 sk-1 | m1,m2\nhttp://b.test/v1\nhttp://c.test/v1 sk-2 | m3")
	if len(r.RawModels) != len(r.Upstreams) {
		t.Fatalf("misaligned RawModels: %d vs %d", len(r.RawModels), len(r.Upstreams))
	}
	if strings.Join(r.RawModels[0], ",") != "m1,m2" || len(r.RawModels[1]) != 0 || strings.Join(r.RawModels[2], ",") != "m3" {
		t.Fatalf("unexpected RawModels: %v", r.RawModels)
	}
}

func TestMergeImportUpstreamsByURL(t *testing.T) {
	text := "http://13.211.156.204:80/v1 sk-key-1 | cbai/glm-5.1,cbai/glm-5.2\n" +
		"http://13.211.156.204:80/v1 sk-key-2 | cbai/glm-5.2,cbai/kimi-k2.5\n" +
		"http://13.211.156.204:80/v1 sk-key-1 | cbai/glm-5.1\n" +
		"http://206.183.130.97:8081/v1 <no-auth-needed> | qwen36,qwen36-128k\n" +
		"http://existing.invalid/v1 sk-existing | m1"
	res := parseUpstreamImports(text)
	existing := []*config.Upstream{
		{ID: "other", Type: "openai", BaseURL: "http://existing.invalid/v1/", AuthMode: "swap", APIKeys: []string{"old"}, Models: []string{"*"}, Priority: 3, Enabled: true},
		{ID: "import-13-211-156-204-80", Type: "auto", BaseURL: "https://elsewhere.invalid/v1", AuthMode: "none", Models: []string{"*"}, Priority: 10, Enabled: true},
	}
	merged := MergeImportUpstreams(res, existing)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged upstreams, got %d: %+v", len(merged), merged)
	}
	a := merged[0]
	if a.ID != "import-13-211-156-204-80-2" {
		t.Fatalf("ID not uniquified: %q", a.ID)
	}
	if a.Name != "13.211.156.204:80" || a.Type != "openai" || a.Priority != 10 || !a.Enabled || a.AuthMode != "swap" {
		t.Fatalf("unexpected merged defaults: %+v", a)
	}
	if strings.Join(a.APIKeys, ",") != "sk-key-1,sk-key-2" {
		t.Fatalf("keys not pooled in first-seen order: %v", a.APIKeys)
	}
	raws := map[string]bool{}
	for _, list := range a.ModelAliases {
		for _, raw := range list {
			raws[raw] = true
		}
	}
	for _, raw := range []string{"cbai/glm-5.1", "cbai/glm-5.2", "cbai/kimi-k2.5"} {
		if !raws[raw] {
			t.Fatalf("raw route %q lost in union", raw)
		}
	}
	if len(a.Models) != 3 {
		t.Fatalf("unexpected canonical model count: %v", a.Models)
	}
	b := merged[1]
	if b.AuthMode != "none" || len(b.APIKeys) != 0 {
		t.Fatalf("no-auth merge got credentials: %+v", b)
	}
	if len(b.Models) != 2 || len(b.ModelAliases["qwen36"]) != 0 {
		t.Fatalf("no-auth models missing: %+v", b)
	}
	// Parsing again and merging against the already-merged set skips both URLs.
	if got := MergeImportUpstreams(parseUpstreamImports(text), append(append([]*config.Upstream{}, existing...), merged...)); len(got) != 0 {
		t.Fatalf("re-merge did not skip everything: %+v", got)
	}
}
