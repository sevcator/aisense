package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/torbundle"
)

type torController struct {
	mu         sync.Mutex
	root       string
	binary     string
	version    string
	cmd        *exec.Cmd
	done       chan error
	config     config.TorCfg
	requests   int
	generation int
	lastUpdate time.Time
	state      string
	lastError  string
}

func newTorController(path string) *torController {
	return &torController{root: filepath.Join(filepath.Dir(path), "tor-bundle"), state: "disabled"}
}

func (t *torController) stopLocked() {
	if t.cmd == nil {
		return
	}
	_ = t.cmd.Process.Kill()
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
	t.cmd, t.done = nil, nil
	t.requests = 0
}

func (t *torController) startLocked(ctx context.Context, cfg config.TorCfg) error {
	t.stopLocked()
	if err := config.ValidateTorCfg(cfg); err != nil {
		return err
	}
	if t.binary == "" {
		return fmt.Errorf("Tor bundle is unavailable")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.SOCKSPort))
	if conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("Tor SOCKS port %d is already in use", cfg.SOCKSPort)
	}
	if err := os.MkdirAll(filepath.Join(t.root, "data"), 0700); err != nil {
		return err
	}
	geoipDir := filepath.Join(t.root, t.version, "data")
	torrc := strings.Join([]string{
		"ClientOnly 1",
		"SocksPort " + address,
		"DataDirectory " + strconv.Quote(filepath.ToSlash(filepath.Join(t.root, "data"))),
		"GeoIPFile " + strconv.Quote(filepath.ToSlash(filepath.Join(geoipDir, "geoip"))),
		"GeoIPv6File " + strconv.Quote(filepath.ToSlash(filepath.Join(geoipDir, "geoip6"))),
		cfg.Config,
	}, "\n") + "\n"
	torrcPath := filepath.Join(t.root, "aisense.torrc")
	if err := os.WriteFile(torrcPath, []byte(torrc), 0600); err != nil {
		return err
	}
	cmd := exec.Command(t.binary, "-f", torrcPath)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.state = "starting"
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return fmt.Errorf("Tor exited before SOCKS was ready: %v", err)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return ctx.Err()
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			return fmt.Errorf("Tor SOCKS port did not become ready")
		case <-tick.C:
			conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
			if err == nil {
				conn.Close()
				select {
				case err := <-done:
					return fmt.Errorf("Tor exited during startup: %v", err)
				default:
				}
				t.cmd, t.done, t.config = cmd, done, cfg
				t.requests = 0
				t.generation++
				t.state = "running"
				t.lastError = ""
				return nil
			}
		}
	}
}

func (t *torController) ensure(ctx context.Context, cfg config.TorCfg) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !cfg.Enabled {
		t.stopLocked()
		t.state = "disabled"
		return nil
	}
	if t.cmd != nil {
		select {
		case <-t.done:
			t.cmd, t.done = nil, nil
		default:
		}
	}
	if t.binary == "" || time.Since(t.lastUpdate) > 24*time.Hour {
		t.state = "updating bundle"
		t.lastUpdate = time.Now()
		previousBinary := t.binary
		binary, version, err := torbundle.Ensure(ctx, t.root)
		if binary != "" {
			t.binary, t.version = binary, version
		}
		if previousBinary != "" && previousBinary != t.binary {
			t.stopLocked()
		}
		if err != nil && t.binary == "" {
			t.state, t.lastError = "unavailable", err.Error()
			return err
		}
		if err != nil {
			t.lastError = err.Error()
		}
	}
	if t.cmd != nil && t.config == cfg {
		return nil
	}
	if err := t.startLocked(ctx, cfg); err != nil {
		t.state, t.lastError = "unavailable", err.Error()
		return err
	}
	return nil
}

func (t *torController) next(cfg config.TorCfg) string {
	port := cfg.SOCKSPort
	if port == 0 {
		port = 9050
	}
	base := "socks5://127.0.0.1:" + strconv.Itoa(port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := t.ensure(ctx, cfg); err != nil {
		return base + "#unavailable"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.requests >= cfg.RestartAfter {
		if err := t.startLocked(ctx, cfg); err != nil {
			t.state, t.lastError = "unavailable", err.Error()
			return base + "#unavailable"
		}
	}
	t.requests++
	return base + "#g" + strconv.Itoa(t.generation)
}

func (t *torController) status() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{"state": t.state, "version": t.version, "requests": t.requests, "error": t.lastError}
}

func (p *Proxy) RunTor(ctx context.Context) {
	if p.tor == nil {
		return
	}
	defer p.tor.ensure(context.Background(), config.TorCfg{})
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		cfg := p.Cfg.Get().Proxies
		if cfg.Enabled && cfg.Tor.Enabled {
			updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_ = p.tor.ensure(updateCtx, cfg.Tor)
			cancel()
		} else {
			_ = p.tor.ensure(ctx, config.TorCfg{})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Proxy) TorStatus() map[string]any {
	if p.tor == nil {
		return map[string]any{"state": "disabled"}
	}
	return p.tor.status()
}
