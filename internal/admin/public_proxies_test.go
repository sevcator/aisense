package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aisense/internal/config"
)

func TestPublicProxyListAcceptsSupportedAddressForms(t *testing.T) {
	list := strings.Join([]string{
		"127.0.0.1:8080",
		"socks5://127.0.0.2:9050",
		`http:\127.0.0.3:3128`,
		`https:\\127.0.0.4:443`,
		"# comment",
		"invalid address",
	}, "\n")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(list))
	}))
	defer source.Close()
	srv, cfg := testServer(t)
	body := fmt.Sprintf(`{"sources":[%q]}`, source.URL)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/proxies/public/fetch", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	for _, want := range []string{"http://127.0.0.1:8080", "socks5://127.0.0.2:9050", "http://127.0.0.3:3128", "https://127.0.0.4:443"} {
		if !strings.Contains(res.Body.String(), want) {
			t.Fatalf("missing %s in %s", want, res.Body.String())
		}
	}
	if got := cfg.Get().Proxies.PublicSources; len(got) != 1 || got[0] != source.URL {
		t.Fatalf("saved sources = %#v", got)
	}
}

func TestPublicProxyCheckTagsOnlyWorkingCandidates(t *testing.T) {
	srv, cfg := testServer(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(""))
	}))
	defer proxy.Close()
	if err := cfg.Update(func(c *config.Config) error {
		c.Proxies.CheckURL = "http://example.test/"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"proxies":[%q,"http://127.0.0.1:1"],"add_working":true,"source":"public"}`, proxy.URL)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/proxies/check", strings.NewReader(body))
	req.SetBasicAuth("admin", "correct-horse")
	res := httptest.NewRecorder()
	srv.Handle(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if got := cfg.Get().Proxies.List; len(got) != 1 || got[0].URL != proxy.URL || got[0].Source != "public" || !got[0].Working {
		t.Fatalf("saved proxies = %#v", got)
	}
}
