package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"aisense/internal/admin"
	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/pricing"
	"aisense/internal/proxy"
	"aisense/internal/store"
)

//go:embed internal/ui/index.html
var uiHTML []byte

//go:embed internal/ui/login.html
var loginHTML []byte

//go:embed internal/ui/flags/*.png
var flagsFS embed.FS

// proxyInfoAdapter implements admin.ProxyInfo by wrapping *proxy.Proxy and
// converting proxy.CachedUpstreamEntry → admin.CachedRouteEntry.
type proxyInfoAdapter struct{ p *proxy.Proxy }

func (a proxyInfoAdapter) UsedModels() []string { return a.p.UsedModels() }

func (a proxyInfoAdapter) TorStatus() map[string]any { return a.p.TorStatus() }

// Optional admin capability; ProxyInfo's existing read-only mocks stay valid.
func (a proxyInfoAdapter) ResetHitchanceKeys(up *config.Upstream) { a.p.ResetHitchanceKeys(up) }

func (a proxyInfoAdapter) CachedUpstreams() []admin.CachedRouteEntry {
	snaps := a.p.CachedUpstreams()
	out := make([]admin.CachedRouteEntry, len(snaps))
	for i, s := range snaps {
		out[i] = admin.CachedRouteEntry{Type: s.Type, UpstreamID: s.UpstreamID, Model: s.Model, CachedAt: s.CachedAt}
	}
	return out
}

func runAdminListener(ctx context.Context, cfg *config.Manager, handler http.Handler, trace *debuglog.Logger, cancel context.CancelFunc) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	serverErrors := make(chan error, 1)
	var srv *http.Server
	activePort := 0
	stop := func() {
		if srv == nil {
			return
		}
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shCtx)
		shCancel()
		srv = nil
		activePort = 0
	}
	start := func(port int) {
		srv = &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", port), Handler: handler}
		activePort = port
		go func(current *http.Server) {
			log.Printf("[aisense] Admin (web UI + API) listening on :%d", port)
			if trace != nil {
				trace.Event("process", "listener.start", map[string]any{"name": "Admin (web UI + API)", "address": current.Addr, "port": port})
			}
			if err := current.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverErrors <- err
			}
		}(srv)
	}
	reconcile := func() {
		listener := cfg.Get().Server.Admin
		enabled := listener.Enabled && listener.Port > 0
		if srv != nil && (!enabled || activePort != listener.Port) {
			stop()
		}
		if srv == nil && enabled {
			start(listener.Port)
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case err := <-serverErrors:
			log.Printf("[aisense] Admin (web UI + API) server error: %v", err)
			if trace != nil {
				trace.Event("process", "listener.error", map[string]any{"name": "Admin (web UI + API)", "error": err.Error()})
			}
			cancel()
			stop()
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func tierPricingEnabled(cfg *config.Config) bool {
	if !cfg.TierPricing.Enabled {
		return false
	}
	for _, up := range cfg.Upstreams {
		if up != nil && up.Enabled {
			return true
		}
	}
	return false
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	debugEnabled := flag.Bool("debug", false, "enable and persist full request/response logging with credential redaction")
	debugPath := flag.String("debug-file", "", "log file override; default is YYYY-MM-DD.log beside the config (local date)")
	exportPath := flag.String("export-config", "", "write a readable copy of the config to this file and exit")
	flag.Parse()

	// Export combines readable settings with the protected credentials for a
	// manual transfer to another machine. The export itself is sensitive.
	if *exportPath != "" {
		plain, err := config.ExportFile(*cfgPath)
		if err != nil {
			log.Fatalf("export: %v", err)
		}
		if err := os.WriteFile(*exportPath, plain, 0o600); err != nil {
			log.Fatalf("export: %v", err)
		}
		log.Printf("[aisense] readable copy written to %s; it holds login credentials and every API key, so protect it and delete it when done", *exportPath)
		return
	}

	dir := filepath.Dir(*cfgPath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	configLocation, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Fatalf("config path: %v", err)
	}
	_, statErr := os.Stat(*cfgPath)
	newConfig := os.IsNotExist(statErr)
	if statErr != nil && !newConfig {
		log.Fatalf("config: %v", statErr)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v\nUse a complete readable config export when moving the installation to another account or machine.", err)
	}
	if newConfig {
		log.Printf("[aisense] no config found at %s; created a new empty installation (0 upstreams, 0 client API keys)", configLocation)
	} else {
		log.Printf("[aisense] loaded config from %s (%d upstreams, %d client API keys)", configLocation, len(cfg.Get().Upstreams), len(cfg.Get().APIKeys))
	}

	// Session login uses admin credentials. Existing installations
	// migrate the old admin token into the password so they are not locked out;
	// new ones start with admin / admin.
	adminCfg := cfg.Get().Server.Admin
	if adminCfg.Username == "" || adminCfg.Password == "" {
		password := adminCfg.Password
		defaulted := password == "" && adminCfg.Token == ""
		if password == "" {
			password = adminCfg.Token
		}
		if password == "" {
			password = config.DefaultAdminPassword
		}
		username := adminCfg.Username
		if username == "" {
			username = "admin"
		}
		if err := cfg.Update(func(c *config.Config) error {
			c.Server.Admin.Username = username
			c.Server.Admin.Password = password
			c.Server.Admin.Token = ""
			return nil
		}); err != nil {
			log.Fatalf("persist admin credentials: %v", err)
		}
		if defaulted {
			log.Printf("[aisense] admin login set to the default password %q; change it in Settings", config.DefaultAdminPassword)
		} else if adminCfg.Token != "" {
			log.Printf("[aisense] admin auth migrated: username=%s, password is the previous admin token", username)
		}
	}

	usagePath := filepath.Join(filepath.Dir(*cfgPath), "usage.json")
	if filepath.Dir(*cfgPath) == "." {
		usagePath = "usage.json"
	}
	st, err := store.New(usagePath)
	if err != nil {
		log.Fatalf("usage store: %v", err)
	}
	defer st.Close()

	trace := debuglog.NewLastResponseDynamic(filepath.Dir(*cfgPath), *debugPath, func() bool { return cfg.Get().LoggingEnabled })
	defer func() {
		if err := trace.Close(); err != nil {
			log.Printf("[logging] close: %v", err)
		}
	}()
	if err := cfg.SetLoggingValidator(trace.Validate); err != nil {
		log.Fatalf("logging: %v", err)
	}
	if *debugEnabled {
		if err := cfg.Update(func(c *config.Config) error { c.LoggingEnabled = true; return nil }); err != nil {
			log.Fatalf("logging: %v", err)
		}
	}
	cfg.Watch(2 * time.Second)
	trace.Event("process", "debug.session.start", map[string]any{"pid": os.Getpid(), "config_path": *cfgPath, "log_path": trace.Path()})

	// The best/shit tier router ranks models by live market prices. The
	// catalog refreshes in the background and survives restarts through a
	// JSON cache beside the config.
	prices := pricing.New(filepath.Join(filepath.Dir(*cfgPath), "tier-pricing.json"))
	prices.Enabled = func() bool { return tierPricingEnabled(cfg.Get()) }
	prices.Interval = func() time.Duration {
		return time.Duration(cfg.Get().TierPricing.RefreshIntervalMinutes) * time.Minute
	}
	prices.IgnoreCerts = func() bool { return cfg.Get().ModelDiscovery.IgnoreCertErrors }
	prices.Debug = func(event string, fields map[string]any) { trace.Event("", event, fields) }

	p := proxy.New(cfg, st, trace)
	p.Pricing = prices
	adm := &admin.Server{
		Cfg:     cfg,
		Store:   st,
		UI:      uiHTML,
		Login:   loginHTML,
		FlagsFS: flagsFS,
		Debug:   trace,
		Health:  p.Health,
		Proxy:   proxyInfoAdapter{p},
	}

	// OpenAI listener
	openaiMux := http.NewServeMux()
	openaiMux.HandleFunc("/", p.ServeOpenAI)

	// Anthropic listener
	anthropicMux := http.NewServeMux()
	anthropicMux.HandleFunc("/", p.ServeAnthropic)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go prices.Run(ctx)
	go p.RunTor(ctx)
	adm.StartModelDiscovery(ctx)

	// Hit chances are counted in memory. Save them now and then, and once more on
	// the way out, so the panel still shows real numbers after a restart.
	saveHitChances := func() {
		if err := cfg.SaveHitWindows(p.Health.HitSnapshot()); err != nil {
			log.Printf("[aisense] could not save hit chances: %v", err)
		}
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				saveHitChances()
			}
		}
	}()

	var servers []*http.Server
	start := func(name string, enabled bool, port int, h http.Handler) {
		if !enabled || port <= 0 {
			log.Printf("[aisense] %s listener disabled", name)
			if trace != nil {
				trace.Event("process", "listener.disabled", map[string]any{"name": name, "enabled": enabled, "port": port})
			}
			return
		}
		srv := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", port), Handler: h}
		servers = append(servers, srv)
		go func() {
			log.Printf("[aisense] %s listening on :%d", name, port)
			if trace != nil {
				trace.Event("process", "listener.start", map[string]any{"name": name, "address": srv.Addr, "port": port})
			}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[aisense] %s server error: %v", name, err)
				if trace != nil {
					trace.Event("process", "listener.error", map[string]any{"name": name, "address": srv.Addr, "port": port, "error": err.Error()})
				}
				cancel()
			}
		}()
	}

	c := cfg.Get()
	start("OpenAI (chat/completions, /v1/*)", c.Server.OpenAI.Enabled, c.Server.OpenAI.Port, openaiMux)
	start("Anthropic (/v1/messages)", c.Server.Anthropic.Enabled, c.Server.Anthropic.Port, anthropicMux)
	adminDone := make(chan struct{})
	go func() {
		defer close(adminDone)
		runAdminListener(ctx, cfg, adm.Handler(), trace, cancel)
	}()

	// OpenAI and Anthropic listener changes require restart; Web access is reconciled live.

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-ctx.Done():
	}
	log.Println("[aisense] shutting down")
	cancel()
	<-adminDone
	if trace != nil {
		trace.Event("process", "debug.session.stop", map[string]any{"pid": os.Getpid(), "reason": "shutdown"})
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shCtx)
	}
	saveHitChances()
}
