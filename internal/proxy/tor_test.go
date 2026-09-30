package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aisense/internal/config"
)

func TestTorSelectionDoesNotFallBackToDirectConnection(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unused := port.Addr().(*net.TCPAddr).Port
	port.Close()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Proxies.Enabled = true
		c.Proxies.Tor = config.TorCfg{Enabled: true, SOCKSPort: unused, RestartAfter: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := New(cfg, nil)
	// Model an unavailable Tor process without initiating a bundle download.
	p.tor = nil
	request, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	if response, err := p.httpClientFor(true).Do(request); err == nil {
		response.Body.Close()
		t.Fatal("request bypassed the unavailable Tor SOCKS port")
	}
}

func TestOfficialTorRestartsAfterRequestLimit(t *testing.T) {
	if os.Getenv("AISENSE_TEST_TOR_DOWNLOAD") != "1" {
		t.Skip("requires Tor Project bundle")
	}
	controller := newTorController(filepath.Join(t.TempDir(), "config.json"))
	defer func() { _ = controller.ensure(context.Background(), config.TorCfg{}) }()
	cfg := config.TorCfg{Enabled: true, SOCKSPort: freeTorPort(t), RestartAfter: 1}
	first := controller.next(cfg)
	if strings.Contains(first, "unavailable") {
		t.Fatalf("Tor failed to start: %#v", controller.status())
	}
	second := controller.next(cfg)
	if strings.Contains(second, "unavailable") || second == first {
		t.Fatalf("Tor did not restart: %q -> %q, status %#v", first, second, controller.status())
	}
}

func freeTorPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
