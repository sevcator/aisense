package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamTestAndAddOnlySavesConfirmedEndpoint(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"messages must be an array"}}`))
	}))
	defer valid.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer invalid.Close()
	srv, cfg := testServer(t)
	for _, tc := range []struct {
		base string
		code int
	}{
		{invalid.URL + "/v1", http.StatusUnprocessableEntity},
		{valid.URL + "/v1", http.StatusOK},
	} {
		body := fmt.Sprintf(`{"base_url":%q,"api_keys":["secret"],"enabled":true}`, tc.base)
		req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/test-and-add?mode=create", strings.NewReader(body))
		req.SetBasicAuth("admin", "correct-horse")
		res := httptest.NewRecorder()
		srv.Handle(res, req)
		if res.Code != tc.code {
			t.Fatalf("base %s: status=%d body=%s", tc.base, res.Code, res.Body.String())
		}
		if strings.Contains(res.Body.String(), "secret") {
			t.Fatal("response exposed credential")
		}
	}
	if got := cfg.Get().Upstreams; len(got) != 1 || got[0].BaseURL != valid.URL+"/v1" {
		t.Fatalf("saved upstreams = %#v", got)
	}
}

func TestBatchTestAndAddSkipsUnconfirmedEntries(t *testing.T) {
	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"messages must be an array"}}`))
	}))
	defer valid.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer invalid.Close()
	srv, cfg := testServer(t)
	text := valid.URL + "/v1 good-secret\n" + invalid.URL + "/v1 bad-secret"
	body, _ := json.Marshal(map[string]any{"text": text, "test_first": true})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstream/import", strings.NewReader(string(body)))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"imported":1`) || !strings.Contains(res.Body.String(), `"skipped":1`) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "bad-secret") {
		t.Fatal("response exposed rejected credential")
	}
	if got := cfg.Get().Upstreams; len(got) != 1 || got[0].BaseURL != valid.URL+"/v1" {
		t.Fatalf("saved upstreams = %#v", got)
	}
}
