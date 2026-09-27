package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aisense/internal/config"
	"aisense/internal/protocol"
	"aisense/internal/proxy"
)

func runBatch(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.testAllUpstreams(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	return w
}

func TestBatchRequestValidationAndHarmlessDefault(t *testing.T) {
	s, cfg := testServer(t)
	for _, body := range []string{
		`{"tasks":{"delete_invalid":true,"disable_invalid":true},"confirm":true}`,
		`{"tasks":{"delete_invalid":true,"hide_invalid":true},"confirm":true}`,
		`{"tasks":{"disable_invalid":true,"hide_invalid":true},"confirm":true}`,
		`{"checks":{"responses":true}}`, `{"tasks":{"hide_invalid":true}}`,
		`{"unknown":true}`, `{`, `{} {}`, `{"checks":{"models":"yes"}}`,
	} {
		if w := runBatch(t, s, body); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"messages":null`) || strings.Contains(string(b), `"hi"`) {
			t.Errorf("default made non-capability request: %s %s", r.URL, b)
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"requires authentication"}}`)
	}))
	defer api.Close()
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "on", BaseURL: api.URL, Enabled: true, Models: []string{"gpt-test"}}, {ID: "off", BaseURL: api.URL + "/disabled", Enabled: false, Models: []string{"gpt-test"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := cfg.Get()
	for _, body := range []string{`{}`, `{"tasks":{"delete_invalid":true},"confirm":true}`, `{"tasks":{"disable_invalid":true},"confirm":true}`, `{"tasks":{"hide_invalid":true},"confirm":true}`} {
		w := runBatch(t, s, body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) || !strings.Contains(w.Body.String(), `"unknown_count":1`) || cfg.Get() != before {
			t.Fatalf("default/unknown mutated config: %s", w.Body.String())
		}
	}
}

// This fixture distinguishes harmless probes from actual inference requests.
func batchProbe(w http.ResponseWriter, r *http.Request, body map[string]any) bool {
	if body["messages"] != nil {
		return false
	}
	if strings.HasSuffix(r.URL.Path, "/messages") {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":"endpoint missing"}`)
	} else {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"message":"messages must be an array"}}`)
	}
	return true
}

func TestBatchResponsesEveryRawModelEveryKeyAndTransactionalEdits(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[string]int{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"provider/gpt-working"},{"id":"gpt-bad"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if batchProbe(w, r, body) {
			return
		}
		raw, _ := body["model"].(string)
		messages, _ := body["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["content"] != "hi" {
			t.Errorf("not hi: %#v", body)
		}
		mu.Lock()
		if seen[key] == nil {
			seen[key] = map[string]int{}
		}
		seen[key][raw]++
		mu.Unlock()
		if key == "permission-secret" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"permission denied"}}`)
			return
		}
		if key == "dead-secret" || key == "mixed-secret" && raw == "gpt-bad" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"model_not_found","message":"retired"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer api.Close()
	s, cfg := testServer(t)
	keys := []string{"healthy-secret", "mixed-secret", "dead-secret", "permission-secret"}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: keys, Models: []string{"gpt-original"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := runBatch(t, s, `{"checks":{"responses":true},"confirm":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":1`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	up := cfg.Get().Upstreams[0]
	if !reflect.DeepEqual(up.APIKeys, []string{"healthy-secret", "mixed-secret", "permission-secret"}) {
		t.Fatal("incorrect keys retained")
	}
	if !up.KeyModelBlocked("mixed-secret", "gpt-bad") || up.KeyModelBlocked("healthy-secret", "gpt-bad") || len(up.BlockedModels) != 0 {
		t.Fatal("per-key block leaked to another credential/family")
	}
	if !reflect.DeepEqual(up.Models, []string{"gpt-original"}) {
		t.Fatal("response check replaced original editor catalog")
	}
	for _, key := range keys {
		if strings.Contains(w.Body.String(), key) {
			t.Fatal("credential leaked in results")
		}
		for _, raw := range []string{"provider/gpt-working", "gpt-bad"} {
			if seen[key][raw] == 0 {
				t.Errorf("model not checked for every credential: %s", raw)
			}
		}
	}
	if seen["mixed-secret"]["gpt-bad"] != 2 {
		t.Fatal("destructive failure was not confirmed twice")
	}
}

func TestBatchAllFailedRemovesUpstreamButUnknownAndSkippedNeverDo(t *testing.T) {
	for _, scenario := range []string{"dead", "rate", "permission", "generic", "skipped", "mixed-unknown", "second-success"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/models") {
					models := `{"data":[{"id":"gpt-test"}]}`
					if scenario == "skipped" {
						models = `{"data":[{"id":"gpt-test"},{"id":"text-embedding-3-small"}]}`
					}
					if scenario == "mixed-unknown" {
						models = `{"data":[{"id":"gpt-test"},{"id":"unknown-model"}]}`
					}
					_, _ = io.WriteString(w, models)
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if batchProbe(w, r, body) {
					return
				}
				calls++
				if scenario == "rate" || body["model"] == "unknown-model" {
					w.WriteHeader(429)
					return
				}
				if scenario == "permission" {
					w.WriteHeader(401)
					return
				}
				if scenario == "generic" {
					w.WriteHeader(500)
					return
				}
				if scenario == "second-success" && calls == 2 {
					_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`)
					return
				}
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":{"code":"model_not_found"}}`)
			}))
			defer api.Close()
			s, cfg := testServer(t)
			if err := cfg.Update(func(c *config.Config) error {
				c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: []string{"secret"}, Models: []string{"gpt-test"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w := runBatch(t, s, `{"checks":{"responses":true},"confirm":true}`)
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if (len(cfg.Get().Upstreams) == 0) != (scenario == "dead") {
				t.Fatalf("incorrect deletion: %s", w.Body.String())
			}
			if scenario == "second-success" && len(cfg.Get().Upstreams[0].KeyBlockedModels) != 0 {
				t.Fatal("transient failure was blocked")
			}
		})
	}
}

func TestBatchCatalogTasksAndAnthropicCredentials(t *testing.T) {
	for _, task := range []string{"delete_invalid", "disable_invalid", "hide_invalid"} {
		t.Run(task, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/models") {
					if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Authorization") != "" {
						t.Error("catalog used wrong credentials")
					}
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/messages") {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"messages must be an array"}}`)
				} else {
					w.WriteHeader(404)
				}
			}))
			defer api.Close()
			s, cfg := testServer(t)
			if err := cfg.Update(func(c *config.Config) error {
				c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: []string{"secret"}, Models: []string{"claude-test"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w := runBatch(t, s, `{"tasks":{"`+task+`":true},"checks":{"models":true},"confirm":true}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":1`) {
				t.Fatal(w.Body.String())
			}
			if task == "delete_invalid" {
				if len(cfg.Get().Upstreams) != 0 {
					t.Fatal("not deleted")
				}
				return
			}
			up := cfg.Get().Upstreams[0]
			if task == "disable_invalid" && up.Enabled || task == "hide_invalid" && !up.HiddenInvalid {
				t.Fatal("task not applied")
			}
			if !reflect.DeepEqual(up.Models, []string{"claude-test"}) || len(up.APIKeys) != 1 {
				t.Fatal("hide/disable deleted routes or credentials")
			}
		})
	}
}

func TestCatalogAndResponseClassifiersConservative(t *testing.T) {
	for _, tc := range []struct{ code, want string }{{"model_not_supported", "skipped"}, {"unsupported_model_type", "skipped"}, {"model_unavailable", "unknown"}, {"model_not_found", "invalid"}, {"model_retired", "invalid"}} {
		send := func(context.Context, string, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"` + tc.code + `"}}`))}, nil
		}
		if got, _ := testModelResponse(context.Background(), send, protocol.Chat, "opaque-model-name"); got != tc.want {
			t.Fatalf("%s: %s", tc.code, got)
		}
	}
	for _, tc := range []struct{ body, want string }{
		{`{"data":[]}`, "invalid"}, {`{"data":[{"id":"gpt-test"}]}`, "valid"},
		{`{}`, "unknown"}, {`{"data":null}`, "unknown"}, {`{"data":[{}]}`, "unknown"},
		{`<html>no</html>`, "unknown"}, {`{"data":[{"id":"*"}]}`, "unknown"},
		{`{"data":[{"id":"gpt-test"}],"next_page_token":"more"}`, "unknown"},
		{`{"data":[{"id":"gpt-test"}],"total":10}`, "unknown"},
	} {
		send := func(context.Context, string, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		}
		if _, got, _ := testCatalog(context.Background(), send); got != tc.want {
			t.Fatalf("%s: %s", tc.body, got)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("connection reset")} {
		send := func(context.Context, string, []byte) (*http.Response, error) { return nil, err }
		if got, _ := testModelResponse(context.Background(), send, protocol.Chat, "gpt-test"); got != "unknown" {
			t.Fatal(got)
		}
	}
	for _, message := range []string{"does not exist or you do not have access", "permission denied", "temporarily unavailable", "quota exhausted", "authentication required"} {
		send := func(context.Context, string, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"model_not_found","message":"` + message + `"}}`))}, nil
		}
		if got, _ := testModelResponse(context.Background(), send, protocol.Chat, "gpt-test"); got != "unknown" {
			t.Fatal(message)
		}
	}
}

func TestBatchCommitCancellationAndConcurrentEdits(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, Models: []string{"gpt-test"}, APIKeys: []string{"secret"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := cfg.Get()
	in := upstreamTestRequest{Confirm: true}
	in.Tasks.DeleteInvalid = true
	outcomes := []upstreamTestOutcome{{up: snapshot.Upstreams[0], invalid: true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.commitUpstreamTests(ctx, snapshot, in, outcomes); err == nil || cfg.Get() != snapshot {
		t.Fatal("cancelled plan committed")
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams[0].APIKeys = append(c.Upstreams[0].APIKeys, "new-secret")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitUpstreamTests(context.Background(), snapshot, in, outcomes); err == nil || len(cfg.Get().Upstreams[0].APIKeys) != 2 {
		t.Fatal("concurrent key edit lost")
	}
	snapshot = cfg.Get()
	outcomes[0].up = snapshot.Upstreams[0]
	if err := cfg.Update(func(c *config.Config) error { c.Server.Admin.Username = "changed-admin"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitUpstreamTests(context.Background(), snapshot, in, outcomes); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Get().Upstreams) != 0 || cfg.Get().Server.Admin.Username != "changed-admin" {
		t.Fatal("unrelated edit overwritten")
	}
}

func TestBatchSSEAndOverlapRejection(t *testing.T) {
	s, _ := testServer(t)
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	s.streamTestAllUpstreams(w, r)
	if w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "data: {") || !strings.Contains(w.Body.String(), `"type":"complete"`) {
		t.Fatal(w.Body.String())
	}
	s.testMu.Lock()
	defer s.testMu.Unlock()
	if w := runBatch(t, s, `{}`); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestBatchResponseConversionAndDualProtocolRecovery(t *testing.T) {
	for _, dual := range []bool{false, true} {
		s, cfg := testServer(t)
		messageCalls := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/models") {
				_, _ = io.WriteString(w, `{"data":[{"id":"claude-test"}]}`)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			isMessages := strings.HasSuffix(r.URL.Path, "/messages")
			if body["messages"] == nil {
				if isMessages || dual {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"messages must be an array"}}`)
				} else {
					w.WriteHeader(404)
				}
				return
			}
			if !isMessages {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"error":{"code":"model_not_found"}}`)
				return
			}
			if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Anthropic-Version") == "" {
				t.Error("incorrect messages auth")
			}
			messageCalls++
			encoded, _ := json.Marshal(body["messages"])
			if !strings.Contains(string(encoded), `"hi"`) || body["max_tokens"] != float64(16) {
				t.Errorf("wrong converted request: %s", encoded)
			}
			_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"hello"}]}`)
		}))
		if err := cfg.Update(func(c *config.Config) error {
			c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: []string{"secret"}, Models: []string{"claude-test"}, KeyBlockedModels: map[string][]string{config.KeyFingerprint("secret"): {"claude-test"}}}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w := runBatch(t, s, `{"checks":{"responses":true},"confirm":true}`)
		api.Close()
		if w.Code != 200 || messageCalls != 1 || len(cfg.Get().Upstreams) != 1 || len(cfg.Get().Upstreams[0].KeyBlockedModels) != 0 {
			t.Fatalf("dual=%v: %s", dual, w.Body.String())
		}
	}
}

func TestCatalogPaginationAndIncompleteCatalog(t *testing.T) {
	calls := 0
	send := func(_ context.Context, op string, _ []byte) (*http.Response, error) {
		calls++
		body := `{"data":[{"id":"provider/a"}],"has_more":true,"last_id":"provider/a"}`
		if calls == 2 {
			if op != "models?after_id=provider%2Fa" {
				t.Error(op)
			}
			body = `{"data":[{"id":"provider/b"}],"has_more":false}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	models, state, _ := testCatalog(context.Background(), send)
	if state != "valid" || !reflect.DeepEqual(models, []string{"provider/a", "provider/b"}) {
		t.Fatalf("%v %s", models, state)
	}
	send = func(context.Context, string, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[],"has_more":true}`))}, nil
	}
	if _, state, _ := testCatalog(context.Background(), send); state != "unknown" {
		t.Fatal("partial empty catalog declared invalid")
	}
}

func TestUpsertPreservesHiddenAndKeyBlocksWhenOmitted(t *testing.T) {
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: "https://example.invalid", HiddenInvalid: true, Models: []string{"gpt-test"}, APIKeys: []string{"secret"}, KeyBlockedModels: map[string][]string{config.KeyFingerprint("secret"): {"gpt-test"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.upsertUpstream(w, httptest.NewRequest("POST", "/?mode=edit", strings.NewReader(`{"id":"up","enabled":true,"base_url":"https://example.invalid","api_keys":["secret"],"models":["gpt-test"]}`)))
	if w.Code != 200 || !cfg.Get().Upstreams[0].HiddenInvalid || !cfg.Get().Upstreams[0].KeyModelBlocked("secret", "gpt-test") {
		t.Fatalf("fields lost: %s", w.Body.String())
	}
}

func TestBatchDisconnectDiscardsAlreadyCompletedDestructiveResults(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			if strings.Contains(r.URL.Path, "/slow/") {
				<-r.Context().Done()
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-test"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if batchProbe(w, r, body) {
			return
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"model_retired"}}`)
	}))
	defer upstream.Close()
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		for _, id := range []string{"fast", "slow"} {
			c.Upstreams = append(c.Upstreams, &config.Upstream{ID: id, Enabled: true, BaseURL: upstream.URL + "/" + id, APIKeys: []string{"secret"}, Models: []string{"gpt-test"}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := cfg.Get()
	finished := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(finished); s.streamTestAllUpstreams(w, r) }))
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, "POST", endpoint.URL, strings.NewReader(`{"checks":{"responses":true},"confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		var event struct {
			Type   string `json:"type"`
			Result struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "result" && event.Result.ID == "fast" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing completed destructive result")
	}
	if cfg.Get() != before {
		t.Fatal("partial result changed configuration")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel workers")
	}
	if cfg.Get() != before {
		t.Fatal("cancelled batch committed partial destructive changes")
	}
}

func TestBatchDiagnosticEvidenceAndSafety(t *testing.T) {
	for _, tc := range []struct {
		name, body, checks, want string
		status                   int
	}{
		{"auth", `{"error":{"code":"invalid_api_key","message":"secret-provider-text"}}`, `"models":true`, "unknown", 401},
		{"rate", `{"error":{"message":"secret-provider-text"}}`, `"models":true`, "unknown", 429},
		{"server", `{"error":{}}`, `"models":true`, "unknown", 503},
		{"html", `<html>secret-provider-text</html>`, `"models":true`, "unknown", 200},
		{"generic-json", `{}`, `"models":true`, "unknown", 200},
		{"catalog-only", `{"data":[{"id":"gpt-test"}]}`, `"models":true`, "yes", 200},
		{"empty-choices", `{"object":"chat.completion","choices":[]}`, `"responses":true`, "unknown", 200},
		{"missing-choices", `{"object":"chat.completion"}`, `"responses":true`, "unknown", 200},
		{"wrong-choices-type", `{"object":"chat.completion","choices":"ok"}`, `"responses":true`, "unknown", 200},
		{"response", `{"object":"chat.completion","choices":[{"message":{"content":"hello"}}]}`, `"responses":true`, "yes", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.checks == `"responses":true` {
					if strings.HasSuffix(r.URL.Path, "/models") {
						io.WriteString(w, `{"data":[{"id":"gpt-test"}]}`)
						return
					}
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if batchProbe(w, r, body) {
						return
					}
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer api.Close()
			s, cfg := testServer(t)
			if err := cfg.Update(func(c *config.Config) error {
				c.Upstreams = []*config.Upstream{{ID: "up", BaseURL: api.URL + "/private-route?token=secret-query", Enabled: true, APIKeys: []string{"secret-key"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := cfg.Get()
			w := runBatch(t, s, `{"checks":{`+tc.checks+`},"tasks":{"delete_invalid":true},"confirm":true}`)
			var result struct {
				Results []struct {
					Valid       string           `json:"valid"`
					Requests    int              `json:"requests"`
					Diagnostics []map[string]any `json:"diagnostics"`
				} `json:"results"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Results) != 1 {
				t.Fatalf("invalid batch result: %d", w.Code)
			}
			r := result.Results[0]
			if r.Valid != tc.want || r.Requests < 3 || len(r.Diagnostics) < 3 || cfg.Get() != before {
				t.Fatalf("unexpected result: %s", w.Body.String())
			}
			for _, secret := range []string{"secret-provider-text", "secret-key", "secret-query", "private-route", api.URL} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("diagnostics exposed provider data")
				}
			}
		})
	}
}

func TestCatalogSharesDeadlineAcrossPages(t *testing.T) {
	var deadline time.Time
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	send := func(c context.Context, _ string, _ []byte) (*http.Response, error) {
		calls++
		d, ok := c.Deadline()
		if !ok || d.After(time.Now().Add(21*time.Second)) {
			t.Fatal("catalog deadline not applied")
		}
		if calls == 1 {
			deadline = d
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-test"}],"has_more":true,"last_id":"gpt-test"}`))}, nil
		}
		if d != deadline {
			t.Fatal("pagination renewed deadline")
		}
		return nil, context.DeadlineExceeded
	}
	if _, state, reason := testCatalog(ctx, send); state != "unknown" || reason == "" || calls != 2 {
		t.Fatalf("timeout classified incorrectly: %s %s", state, reason)
	}
}

func TestBatchNestedDeadlineRetainsUnknown(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer api.Close()
	s, cfg := testServer(t)
	transport := proxy.New(cfg, nil)
	defer transport.CloseTestConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	in := upstreamTestRequest{}
	in.Checks.Models = true
	start := time.Now()
	out := s.checkUpstream(ctx, transport, &config.Upstream{ID: "up", BaseURL: api.URL, APIKeys: []string{"a", "b"}}, in, func(map[string]any) {})
	if time.Since(start) > time.Second || out.invalid || out.result["valid"] != "unknown" || len(out.removeKeys) != 0 || len(out.blocked) != 0 {
		t.Fatalf("deadline ignored or destructive result: %#v", out.result)
	}
	if intValue(out.result["requests"]) != 1 {
		t.Fatalf("requests continued after cancellation: %#v", out.result)
	}
}

func TestBatchEndpointOutageStopsAfterTwoSlots(t *testing.T) {
	for _, sc := range []struct {
		name   string
		status int
	}{
		{"transport", 0},
		{"gateway502", 502},
	} {
		t.Run(sc.name, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				mu.Unlock()
				if sc.status == 0 {
					panic(http.ErrAbortHandler) // transport-level failure
				}
				w.WriteHeader(sc.status)
			}))
			defer api.Close()
			s, cfg := testServer(t)
			if err := cfg.Update(func(c *config.Config) error {
				c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: []string{"k1", "k2", "k3", "k4", "k5"}, Models: []string{"gpt-test"}}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w := runBatch(t, s, `{"checks":{"models":true,"responses":true},"confirm":true}`)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			mu.Lock()
			total := attempts
			mu.Unlock()
			// Two slots at ~3 requests each must bound the work; five slots
			// would need at least 15 requests.
			if total < 4 || total > 12 {
				t.Fatalf("unexpected attempt count %d", total)
			}
			if !strings.Contains(w.Body.String(), `"untested_keys":3`) || !strings.Contains(w.Body.String(), "endpoint unavailable") {
				t.Fatalf("missing early-stop evidence: %s", w.Body.String())
			}
			up := cfg.Get().Upstreams[0]
			if up == nil || !up.Enabled || len(up.APIKeys) != 5 || len(up.Models) != 1 || up.HiddenInvalid {
				t.Fatalf("destructive edit after outage: %#v", up)
			}
		})
	}
}

func TestBatchPositiveEvidencePreventsOutageStop(t *testing.T) {
	var mu sync.Mutex
	modelHits := map[string]int{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "/models") {
			mu.Lock()
			modelHits[key]++
			mu.Unlock()
			if key == "good-secret" {
				_, _ = io.WriteString(w, `{"data":[{"id":"gpt-test"}]}`)
				return
			}
			w.WriteHeader(502)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if batchProbe(w, r, body) {
			if key != "good-secret" {
				w.WriteHeader(502)
			}
			return
		}
		if key != "good-secret" {
			w.WriteHeader(502)
			return
		}
		_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer api.Close()
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "up", Enabled: true, BaseURL: api.URL, APIKeys: []string{"good-secret", "outage-a", "outage-b"}, Models: []string{"gpt-test"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := runBatch(t, s, `{"checks":{"models":true,"responses":true},"confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "endpoint unavailable") || strings.Contains(w.Body.String(), `"untested_keys"`) {
		t.Fatalf("positive evidence must prevent early stop: %s", w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"good-secret", "outage-a", "outage-b"} {
		if modelHits[key] == 0 {
			t.Fatalf("credential %q was skipped", key)
		}
	}
	up := cfg.Get().Upstreams[0]
	if up == nil || len(up.APIKeys) != 3 {
		t.Fatalf("keys must be retained: %#v", up)
	}
}

func TestBatchCannedGatewayResponseRemoved(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if batchProbe(w, r, body) {
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"[9router Gateway Response via model 'fake-model']:\nSuccessfully processed prompt: 'hi'\n(Gateway status: Operational | Model: fake-model)"}}]}`)
	}))
	defer api.Close()
	s, cfg := testServer(t)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upstreams = []*config.Upstream{{ID: "fake", Enabled: true, Type: "openai", BaseURL: api.URL, APIKeys: []string{"sk-fake"}, Models: []string{"fake-model"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := runBatch(t, s, `{"checks":{"responses":true},"confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(cfg.Get().Upstreams) != 0 {
		t.Fatalf("canned gateway upstream must be removed, config = %#v", cfg.Get().Upstreams)
	}
	if !strings.Contains(w.Body.String(), "canned gateway response") {
		t.Fatalf("missing canned evidence: %s", w.Body.String())
	}
}
