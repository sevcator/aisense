package admin

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/debuglog"
	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/protocol"
	"aisense/internal/routehealth"
	"aisense/internal/store"
)

// ProxyInfo is the subset of proxy.Proxy capabilities used by the admin server.
// Defined as an interface to avoid a package import cycle.
type ProxyInfo interface {
	UsedModels() []string
	CachedUpstreams() []CachedRouteEntry
}

// CachedRouteEntry mirrors proxy.CachedUpstreamEntry but is re-declared here
// to avoid a cross-package dependency.
type CachedRouteEntry struct {
	Type       string    `json:"type"`
	Model      string    `json:"model"`
	UpstreamID string    `json:"upstream_id"`
	CachedAt   time.Time `json:"cached_at"`
}

type Server struct {
	autoDiscovery interface {
		Refresh(context.Context) ([]string, error)
	}
	Cfg     *config.Manager
	Store   *store.Store
	UI      []byte // embedded index.html
	Login   []byte // embedded login.html, supplied by the application
	FlagsFS embed.FS
	Debug   *debuglog.Logger // optional full-request trace logger
	Health  *routehealth.Manager
	Proxy   ProxyInfo // optional; enables /cached-routes and used_models in /stats

	mu               sync.Mutex
	testMu           sync.Mutex
	discoveryRun     sync.Mutex
	discoveryMu      sync.Mutex
	discoveryWake    chan struct{}
	discoveryPending map[string]bool
	discoveryAll     bool
	discoveryStarted bool
	discoveryStatus  discoveryProgress
	authMu           sync.Mutex
	sessions         map[string]*adminSession
}

func (s *Server) healthManager() *routehealth.Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Health == nil {
		s.Health = routehealth.New()
	}
	return s.Health
}

func (s *Server) auth(r *http.Request) bool {
	if _, ok := s.session(r); ok {
		return true
	}
	cfg := s.Cfg.Get().Server.Admin
	username, password, ok := r.BasicAuth()
	if !ok || cfg.TOTPSecret != "" || !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		return false
	}
	return credentialsMatch(cfg, username, password)
}

func (s *Server) unauthorized(w http.ResponseWriter, api bool) {
	w.Header().Set("Cache-Control", "no-store")
	if api {
		s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authorization required"})
		return
	}
	http.Error(w, "Authorization required", http.StatusUnauthorized)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	// Never trace arbitrary HTTP payloads or headers: even unrelated routes can
	// carry cookies, credentials, or UI debug events containing form values.
	if s.Debug != nil {
		s.handleDebug(w, r)
		return
	}
	s.handle(w, r)
}

// Handler routes the admin panel beneath its configured URL path.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := config.NormalizeAdminPath(s.Cfg.Get().Server.Admin.Path)
		if base != "/" {
			if r.URL.Path == base {
				http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
				return
			}
			if !strings.HasPrefix(r.URL.Path, base+"/") {
				// Hold an incorrect Web Access path open until the browser
				// abandons the request and reports a connection timeout.
				<-r.Context().Done()
				return
			}
			http.StripPrefix(base, http.HandlerFunc(s.Handle)).ServeHTTP(w, r)
			return
		}
		s.Handle(w, r)
	})
}

func (s *Server) basePath() string {
	base := config.NormalizeAdminPath(s.Cfg.Get().Server.Admin.Path)
	if base == "/" {
		return ""
	}
	return base
}

func (s *Server) writePage(w http.ResponseWriter, content []byte) {
	path := html.EscapeString(config.NormalizeAdminPath(s.Cfg.Get().Server.Admin.Path))
	body := strings.ReplaceAll(string(content), `__AISENSE_BASE_PATH__`, path)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, body)
}

type adminTraceResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *adminTraceResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *adminTraceResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += int64(n)
	return n, err
}

func (w *adminTraceResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newAdminTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func adminHandlerName(r *http.Request) string {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		return "admin.serveUI"
	}
	if r.URL.Path == "/favicon.ico" {
		return "admin.serveFavicon"
	}
	if strings.HasPrefix(r.URL.Path, "/admin/flags/") {
		return "admin.serveFlag"
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	handlers := map[string]string{
		http.MethodGet + " state":                      "admin.state",
		http.MethodGet + " cached-routes":              "admin.cachedRoutes",
		http.MethodPost + " debug/event":               "admin.debugUIEvent",
		http.MethodPost + " settings":                  "admin.saveSettings",
		http.MethodPost + " upstream":                  "admin.upsertUpstream",
		http.MethodPost + " upstream/import":           "admin.importUpstream",
		http.MethodPost + " upstreams/mass-edit":       "admin.massEditUpstreams",
		http.MethodPost + " upstreams/refresh-models":  "admin.refreshModelsNow",
		http.MethodGet + " upstreams/discovery":        "admin.discoveryStatus",
		http.MethodDelete + " upstream":                "admin.deleteUpstream",
		http.MethodPost + " upstream/test":             "admin.testUpstream",
		http.MethodPost + " upstreams/test-all":        "admin.testAllUpstreams",
		http.MethodPost + " upstreams/test-all/stream": "admin.streamTestAllUpstreams",
		http.MethodGet + " upstreams/health":           "admin.upstreamHealth",
		http.MethodPost + " key":                       "admin.upsertKey",
		http.MethodDelete + " key":                     "admin.deleteKey",
		http.MethodGet + " usage":                      "admin.usage",
		http.MethodGet + " stats":                      "admin.stats",
		http.MethodPost + " proxies":                   "admin.addProxies",
		http.MethodDelete + " proxies":                 "admin.deleteProxy",
		http.MethodPost + " proxies/update":            "admin.updateProxy",
		http.MethodPost + " proxies/check":             "admin.checkProxies",
	}
	if name := handlers[r.Method+" "+path]; name != "" {
		return name
	}
	return "admin.notFound"
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	if path == "login" || path == "logout" || path == "settings" || path == "state" || path == "debug/event" || strings.HasPrefix(path, "2fa") || strings.HasPrefix(path, "auth") {
		s.handle(w, r)
		return
	}
	traceID := newAdminTraceID()
	handler := adminHandlerName(r)
	s.Debug.Event(traceID, "admin.request", map[string]any{
		"handler": handler,
	})

	writer := &adminTraceResponseWriter{ResponseWriter: w}
	defer func(start time.Time) {
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		s.Debug.Event(traceID, "admin.response", map[string]any{
			"status":      status,
			"bytes_out":   writer.bytes,
			"duration_ms": time.Since(start).Milliseconds(),
			"handler":     handler,
		})
	}(time.Now())

	s.handle(writer, r)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/admin/api/login" && r.Method == http.MethodPost {
		s.login(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/favicon.ico" {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	isAPI := strings.HasPrefix(r.URL.Path, "/admin/api/")
	if !s.auth(r) {
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			s.writePage(w, s.Login)
			return
		}
		s.unauthorized(w, isAPI)
		return
	}
	if _, cookie := s.session(r); cookie && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-Aisense-Request") != "1" || !sameOrigin(r) {
			s.writeJSON(w, 403, map[string]string{"error": "same-origin request required"})
			return
		}
	}

	// static UI at root
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		s.writePage(w, s.UI)
		return
	}
	// country flags
	if strings.HasPrefix(r.URL.Path, "/admin/flags/") {
		code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/flags/"), ".png")
		code = strings.ToLower(strings.TrimSpace(code))
		data, err := s.FlagsFS.ReadFile("internal/ui/flags/" + code + ".png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	switch {
	case path == "logout" && r.Method == http.MethodPost:
		s.logout(w, r)
	case path == "2fa" || strings.HasPrefix(path, "2fa/"):
		s.twoFactor(w, r, path)
	case path == "state" && r.Method == http.MethodGet:
		state := *s.Cfg.Get()
		state.Server.Admin.Password = ""
		state.Server.Admin.Token = ""
		state.Server.Admin.TOTPSecret = ""
		state.Server.Admin.TOTPLastCounter = 0
		var names []string
		for _, up := range state.Upstreams {
			names = append(names, up.VisibleModelNames()...)
		}
		s.writeJSON(w, 200, struct {
			*config.Config
			PresentationModels []string `json:"presentation_models"`
		}{&state, modelalias.PresentationNames(names, state.Models.VariantOptions())})
	case path == "cached-routes" && r.Method == http.MethodGet:
		s.cachedRoutes(w)
	case path == "debug/event" && r.Method == http.MethodPost:
		s.debugUIEvent(w, r)
	case path == "hitchance/defaults" && r.Method == http.MethodGet:
		s.writeJSON(w, 200, hitchance.Default())
	case path == "hitchance/preview" && r.Method == http.MethodPost:
		s.hitchancePreview(w, r)
	case path == "hitchance/reset" && r.Method == http.MethodPost:
		s.hitchanceReset(w, r)
	case path == "settings" && r.Method == http.MethodPost:
		s.saveSettings(w, r)
	case path == "upstream" && r.Method == http.MethodPost:
		s.upsertUpstream(w, r)
	case path == "upstream/import" && r.Method == http.MethodPost:
		s.importUpstream(w, r)
	case path == "upstreams/mass-edit" && r.Method == http.MethodPost:
		s.massEditUpstreams(w, r)
	case path == "upstreams/refresh-models" && r.Method == http.MethodPost:
		s.refreshModelsNow(w, r)
	case path == "upstreams/discovery" && r.Method == http.MethodGet:
		s.discoveryMu.Lock()
		status := s.discoveryStatus
		status.Queued = len(s.discoveryPending)
		status.AllQueued = s.discoveryAll
		s.discoveryMu.Unlock()
		s.writeJSON(w, 200, status)
	case path == "upstream" && r.Method == http.MethodDelete:
		s.deleteUpstream(w, r)
	case path == "upstream/test" && r.Method == http.MethodPost:
		s.testUpstream(w, r)
	case path == "upstreams/test-all" && r.Method == http.MethodPost:
		s.testAllUpstreams(w, r)
	case path == "upstreams/test-all/stream" && r.Method == http.MethodPost:
		s.streamTestAllUpstreams(w, r)
	case path == "upstreams/health" && r.Method == http.MethodGet:
		s.writeJSON(w, 200, map[string]any{"upstreams": s.hitchanceHealth(), "updated_at": time.Now()})
	case path == "combo" && r.Method == http.MethodPost:
		s.upsertCombo(w, r)
	case path == "combo" && r.Method == http.MethodDelete:
		s.deleteCombo(w, r)
	case path == "key" && r.Method == http.MethodPost:
		s.upsertKey(w, r)
	case path == "key" && r.Method == http.MethodDelete:
		s.deleteKey(w, r)
	case path == "usage" && r.Method == http.MethodGet:
		s.usage(w, r)
	case path == "stats" && r.Method == http.MethodGet:
		s.stats(w)
	case path == "proxies" && r.Method == http.MethodPost:
		s.addProxies(w, r)
	case path == "proxies" && r.Method == http.MethodDelete:
		s.deleteProxy(w, r)
	case path == "proxies/update" && r.Method == http.MethodPost:
		s.updateProxy(w, r)
	case path == "proxies/check" && r.Method == http.MethodPost:
		s.checkProxies(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) debugUIEvent(w http.ResponseWriter, r *http.Request) {
	// Client-controlled events can contain passwords and authenticator secrets.
	s.writeJSON(w, http.StatusOK, map[string]string{"ok": "disabled"})
}

// ---------------------------------------------------------------------------
// settings

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hitchance           json.RawMessage `json:"hitchance"`
		LoggingEnabled      *bool           `json:"logging_enabled"`
		AutoModelsDiscovery *bool           `json:"auto_models_discovery"`
		Server              *struct {
			OpenAI    *config.ListenerCfg `json:"openai"`
			Anthropic *config.ListenerCfg `json:"anthropic"`
			Admin     struct {
				Enabled  *bool   `json:"enabled"`
				Port     *int    `json:"port"`
				Path     *string `json:"path"`
				Username string  `json:"username"`
				Password string  `json:"password"`
			} `json:"admin"`
		} `json:"server"`
		Usage          *config.UsageCfg          `json:"usage"`
		Failover       json.RawMessage           `json:"failover"`
		ModelDiscovery *config.ModelDiscoveryCfg `json:"model_discovery"`
		Models         *struct {
			ReasoningVariants   *bool `json:"reasoning_variants"`
			PreferOtherVariants *bool `json:"prefer_other_variants"`
			FastMode            *bool `json:"fast_mode"`
		} `json:"models"`
		Proxies *config.ProxyCfg `json:"proxies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if len(in.Hitchance) > 0 {
		if _, err := hitchance.Decode(s.Cfg.Get().Hitchance, in.Hitchance); err != nil {
			s.writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if len(in.Failover) > 0 {
		s.writeJSON(w, 400, map[string]string{"error": "failover is retired; configure hitchance instead"})
		return
	}
	if in.Server != nil && in.Server.Admin.Path != nil && strings.ContainsAny(*in.Server.Admin.Path, "?#\\") {
		s.writeJSON(w, 400, map[string]string{"error": "admin path must not contain a query, fragment or backslash"})
		return
	}
	err := s.Cfg.Update(func(c *config.Config) error {
		if len(in.Hitchance) > 0 {
			var err error
			c.Hitchance, err = hitchance.Decode(c.Hitchance, in.Hitchance)
			if err != nil {
				return err
			}
			c.Hitchance.RetryCycles = 0
		}
		if in.AutoModelsDiscovery != nil {
			c.AutoModelsDiscovery = *in.AutoModelsDiscovery
		}
		if in.LoggingEnabled != nil {
			if *in.LoggingEnabled && s.Debug == nil {
				return fmt.Errorf("logging is not configured")
			}
			c.LoggingEnabled = *in.LoggingEnabled
		}
		if in.Server != nil {
			if in.Server.OpenAI != nil {
				c.Server.OpenAI = *in.Server.OpenAI
			}
			if in.Server.Anthropic != nil {
				c.Server.Anthropic = *in.Server.Anthropic
			}
			if in.Server.Admin.Enabled != nil {
				c.Server.Admin.Enabled = *in.Server.Admin.Enabled
			}
			if in.Server.Admin.Port != nil {
				c.Server.Admin.Port = *in.Server.Admin.Port
			}
			if in.Server.Admin.Path != nil {
				c.Server.Admin.Path = config.NormalizeAdminPath(*in.Server.Admin.Path)
			}
			if username := strings.TrimSpace(in.Server.Admin.Username); username != "" {
				c.Server.Admin.Username = username
			}
			if in.Server.Admin.Password != "" {
				c.Server.Admin.Password = in.Server.Admin.Password
			}
			c.Server.Admin.Token = ""
		}
		if in.Usage != nil {
			c.Usage = *in.Usage
		}
		if in.ModelDiscovery != nil {
			c.ModelDiscovery = *in.ModelDiscovery
			if c.ModelDiscovery.RefreshIntervalMinutes <= 0 {
				c.ModelDiscovery.RefreshIntervalMinutes = 60
			}
		}
		if in.Models != nil {
			if in.Models.ReasoningVariants != nil {
				c.Models.ReasoningVariants = in.Models.ReasoningVariants
			}
			if in.Models.PreferOtherVariants != nil {
				c.Models.PreferOtherVariants = *in.Models.PreferOtherVariants
			}
			if in.Models.FastMode != nil {
				c.Models.FastMode = *in.Models.FastMode
			}
		}
		if in.Proxies != nil {
			// merge: keep the pool, update settings
			c.Proxies.Enabled = in.Proxies.Enabled
			c.Proxies.CheckURL = in.Proxies.CheckURL
			c.Proxies.ExcludeCountries = in.Proxies.ExcludeCountries
			if in.Proxies.List != nil {
				c.Proxies.List = in.Proxies.List
			}
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "saved"})
}

// ---------------------------------------------------------------------------
// upstreams

func (s *Server) upsertUpstream(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var up config.Upstream
	if err := json.Unmarshal(payload, &up); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var provided map[string]json.RawMessage
	_ = json.Unmarshal(payload, &provided)
	up.ID = strings.TrimSpace(up.ID)
	editRequested := strings.EqualFold(r.URL.Query().Get("mode"), "edit")
	if editRequested && up.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required for edit"})
		return
	}
	up.Name = strings.TrimSpace(up.Name)
	up.BaseURL = config.NormalizeBaseURL(up.BaseURL)
	up.APIKeys = config.NormalizeAPIKeys(up.APIKeys)
	if up.BaseURL == "" {
		s.writeJSON(w, 400, map[string]string{"error": "base_url is required"})
		return
	}
	if err := config.ValidateBaseURL(up.BaseURL); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": "base_url must be an absolute HTTP(S) URL without query or fragment"})
		return
	}
	if up.Type != "openai" && up.Type != "anthropic" {
		up.Type = "auto"
	}
	if up.ID == "" {
		up.ID = uniqueUpstreamID(sanitizeImportID(up.BaseURL), s.Cfg.Get().Upstreams)
	}
	if up.Name == "" && !editRequested {
		up.Name = up.ID
	}
	if up.Priority <= 0 {
		up.Priority = 10
	}
	if up.AuthMode == "" {
		up.AuthMode = "swap"
		if !editRequested && len(up.APIKeys) == 0 {
			up.AuthMode = "none"
		}
	}
	up.Models = normalizeStringList(up.Models)
	if len(up.Models) == 0 {
		up.Models = []string{"*"}
	}
	if up.ModelAliases != nil {
		up.ModelAliases = filterModelAliases(up.Models, up.ModelAliases)
	}
	up.BlockedModels = normalizeStringList(up.BlockedModels)
	changedID := up.ID
	created, merged, keysAdded := false, false, 0
	errUpstreamNotFound := errors.New("upstream not found")
	err = s.Cfg.Update(func(c *config.Config) error {
		// Edits are authoritative: if the edited endpoint collides with another
		// row, keep the edited row and absorb every duplicate key into it.
		for i, u := range c.Upstreams {
			if u.ID == up.ID {
				// Detection never rewrites the legacy identity hint.
				up.Type = u.Type
				if _, ok := provided["name"]; !ok {
					up.Name = u.Name
				}
				if up.Name == "" {
					up.Name = up.ID
				}
				if up.ModelAliases == nil {
					up.ModelAliases = preserveModelAliases(u, up.Models)
				}
				if _, ok := provided["priority"]; !ok {
					up.Priority = u.Priority
				}
				if _, ok := provided["auth_mode"]; !ok {
					up.AuthMode = u.AuthMode
				}
				if _, ok := provided["oauth"]; !ok {
					up.OAuth = u.OAuth
				}
				if _, ok := provided["fail_codes"]; !ok {
					up.FailCodes = append([]int(nil), u.FailCodes...)
				}
				if _, ok := provided["hidden_invalid"]; !ok {
					up.HiddenInvalid = u.HiddenInvalid
				}
				if _, ok := provided["key_blocked_models"]; !ok {
					up.KeyBlockedModels = config.CloneModelAliases(u.KeyBlockedModels)
				}
				if _, ok := provided["hitchance_state"]; !ok && up.HitchanceIdentity() == u.HitchanceIdentity() {
					up.HitchanceState = u.HitchanceState
				}
				up.ModelsRefreshedAt = u.ModelsRefreshedAt
				up.ModelsRefreshError = u.ModelsRefreshError
				identity := config.ExactEndpointIdentity(&up)
				out := make([]*config.Upstream, 0, len(c.Upstreams))
				for j, other := range c.Upstreams {
					if j == i {
						out = append(out, &up)
						continue
					}
					if identity != "" && config.ExactEndpointIdentity(other) == identity {
						keysAdded += config.MergeUpstreamKeys(&up, other)
						merged = true
						continue
					}
					out = append(out, other)
				}
				c.Upstreams = out
				return nil
			}
		}
		if editRequested {
			return errUpstreamNotFound
		}
		inheritImportHint(&up, c.Upstreams)
		identity := config.ExactEndpointIdentity(&up)
		for _, existing := range c.Upstreams {
			if identity != "" && config.ExactEndpointIdentity(existing) == identity {
				keysAdded += config.MergeUpstreamKeys(existing, &up)
				changedID = existing.ID
				merged = true
				return nil
			}
		}
		c.Upstreams = append(c.Upstreams, &up)
		created = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errUpstreamNotFound) {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{"ok": "saved", "created": created, "merged": merged, "keys_added": keysAdded, "id": changedID}
	s.queueModelDiscovery([]string{changedID})
	resp["models_discovery_queued"] = true
	s.writeJSON(w, 200, resp)
}

func preserveModelAliases(existing *config.Upstream, requested []string) map[string][]string {
	return filterModelAliases(requested, existing.ModelAliases)
}

func filterModelAliases(requested []string, aliases map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, model := range requested {
		for canonical, routes := range aliases {
			if modelalias.Matches(model, canonical) {
				out[canonical] = append([]string(nil), routes...)
				break
			}
		}
	}
	return out
}

// importUpstream imports valid entries locally, reporting sanitized per-entry
// errors. Catalog discovery is queued independently of periodic settings.
func (s *Server) importUpstream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	result := parseUpstreamImports(in.Text)
	if len(result.Upstreams) == 0 && len(result.Skipped) == 0 {
		s.writeJSON(w, 400, map[string]string{"error": "could not find any upstream URL in pasted text"})
		return
	}

	created := 0
	merged := 0
	keysAdded := 0
	importedIDs := []string{}
	err := s.Cfg.Update(func(c *config.Config) error {
		createdIDs := map[string]bool{}
		for _, up := range result.Upstreams {
			inheritImportHint(up, c.Upstreams)
			identity := config.ExactEndpointIdentity(up)
			var existing *config.Upstream
			for _, candidate := range c.Upstreams {
				if identity != "" && config.ExactEndpointIdentity(candidate) == identity {
					existing = candidate
					break
				}
			}
			if existing != nil {
				keysAdded += config.MergeUpstreamKeys(existing, up)
				// Keys win over URL-only entries within this batch, not over
				// authentication settings of previously persisted upstreams.
				if createdIDs[existing.ID] && len(existing.APIKeys) > 0 {
					existing.AuthMode = "swap"
				}
				merged++
				importedIDs = append(importedIDs, existing.ID)
				continue
			}
			up.ID = uniqueUpstreamID(up.ID, c.Upstreams)
			if up.Name == "" {
				up.Name = up.ID
			}
			c.Upstreams = append(c.Upstreams, up)
			createdIDs[up.ID] = true
			created++
			keysAdded += len(up.APIKeys)
			importedIDs = append(importedIDs, up.ID)
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	affected := []*config.Upstream{}
	affectedSeen := map[string]bool{}
	for _, id := range importedIDs {
		if affectedSeen[id] {
			continue
		}
		for _, up := range s.Cfg.Get().Upstreams {
			if up != nil && up.ID == id {
				affected = append(affected, up)
				affectedSeen[id] = true
				break
			}
		}
	}
	resp := map[string]any{
		"ok":         "imported",
		"imported":   created + merged,
		"created":    created,
		"merged":     merged,
		"keys_added": keysAdded,
		"skipped":    len(result.Skipped),
		"errors":     result.Skipped, // [{url, reason}] — kept even on partial success
		"upstreams":  affected,
	}
	if len(importedIDs) > 0 {
		s.queueModelDiscovery(importedIDs)
		resp["models_discovery_queued"] = true
	}
	if len(affected) == 1 {
		resp["upstream"] = affected[0]
	}
	s.writeJSON(w, 200, resp)
}

func (s *Server) refreshModelsNow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
		All bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && err != io.EOF {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	ids := in.IDs
	if in.All {
		ids = nil
	}
	s.queueModelDiscovery(ids)
	s.writeJSON(w, 202, map[string]any{"ok": "queued", "models_discovery_queued": true})
}

func (s *Server) massEditUpstreams(w http.ResponseWriter, r *http.Request) {
	var in upstreamMassEditRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !in.All && len(in.IDs) == 0 {
		s.writeJSON(w, 400, map[string]string{"error": "select at least one upstream or use all"})
		return
	}
	if in.Patch.empty() {
		s.writeJSON(w, 400, map[string]string{"error": "select at least one setting to apply"})
		return
	}
	selected := map[string]bool{}
	for _, id := range in.IDs {
		id = strings.TrimSpace(id)
		if id != "" {
			selected[id] = true
		}
	}
	updated := 0
	err := s.Cfg.Update(func(c *config.Config) error {
		for i, up := range c.Upstreams {
			if up == nil || (!in.All && !selected[up.ID]) {
				continue
			}
			copy := *up
			copy.Models = append([]string(nil), up.Models...)
			copy.ModelAliases = config.CloneModelAliases(up.ModelAliases)
			copy.BlockedModels = append([]string(nil), up.BlockedModels...)
			copy.FailCodes = append([]int(nil), up.FailCodes...)
			if err := applyUpstreamMassEdit(&copy, in.Patch); err != nil {
				return fmt.Errorf("upstream %s: %w", up.ID, err)
			}
			c.Upstreams[i] = &copy
			updated++
		}
		if updated == 0 {
			return errors.New("no matching upstreams")
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]any{"ok": "updated", "updated": updated})
}

func (s *Server) deleteUpstream(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	removed := false
	err := s.Cfg.Update(func(c *config.Config) error {
		out := c.Upstreams[:0]
		for _, u := range c.Upstreams {
			if u.ID == id {
				removed = true
			} else {
				out = append(out, u)
			}
		}
		c.Upstreams = out
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "upstream not found"})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "deleted"})
}

// testUpstream fires a minimal chat completion against one upstream.
func (s *Server) testUpstream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var up *config.Upstream
	for _, u := range s.Cfg.Get().Upstreams {
		if u.ID == in.ID {
			up = u
		}
	}
	if up == nil {
		s.writeJSON(w, 404, map[string]string{"error": "upstream not found"})
		return
	}
	s.writeJSON(w, 200, s.testOneUpstream(r.Context(), up, in.Model))
}

func snapshotTestUpstreams(cfg *config.Config, ids []string) []*config.Upstream {
	wanted := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	var ups []*config.Upstream
	for _, u := range cfg.Upstreams {
		if u == nil || !u.Enabled || (len(wanted) > 0 && !wanted[u.ID]) {
			continue
		}
		cp := *u
		cp.APIKeys = append([]string(nil), u.APIKeys...)
		cp.Models = append([]string(nil), u.Models...)
		cp.ModelAliases = config.CloneModelAliases(u.ModelAliases)
		cp.BlockedModels = append([]string(nil), u.BlockedModels...)
		cp.KeyBlockedModels = config.CloneModelAliases(u.KeyBlockedModels)
		cp.FailCodes = append([]int(nil), u.FailCodes...)
		ups = append(ups, &cp)
	}
	return ups
}

// testAllUpstreams runs the same guarded pipeline without streaming.
func (s *Server) testAllUpstreams(w http.ResponseWriter, r *http.Request) {
	s.runUpstreamTests(w, r, false)
}

// streamTestAllUpstreams supports SSE and the existing NDJSON API.
func (s *Server) streamTestAllUpstreams(w http.ResponseWriter, r *http.Request) {
	s.runUpstreamTests(w, r, true)
}

// testOneUpstream checks schema capabilities without inference or config writes.
func (s *Server) testOneUpstream(ctx context.Context, up *config.Upstream, model string) map[string]any {
	start := time.Now()
	result := map[string]any{
		"id":        up.ID,
		"name":      up.Name,
		"base_url":  up.BaseURL,
		"key_count": len(config.NormalizeAPIKeys(up.APIKeys)),
	}
	md := s.Cfg.Get().ModelDiscovery
	client := discoveryClient(md, 3*time.Second)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	capabilities := probeCapabilities(ctx, up, client)
	result["capabilities"] = capabilities
	result["ok"] = capabilities[protocol.Chat].Supported || capabilities[protocol.Messages].Supported
	result["status"] = capabilities[protocol.Chat].Status
	if capabilities[protocol.Messages].Supported {
		result["status"] = capabilities[protocol.Messages].Status
	}
	result["latency_ms"] = time.Since(start).Milliseconds()
	result["detail"] = "Schema capability check only; no inference or model availability test was performed."
	if result["ok"] == false {
		result["error"] = "No operation confirmed; authentication, rate limits, and ambiguous responses are inconclusive."
	}
	return result
}

// ---------------------------------------------------------------------------
// api keys

func newKeyID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "aisense-" + hex.EncodeToString(b)[:24]
}

func (s *Server) upsertKey(w http.ResponseWriter, r *http.Request) {
	var in config.APIKey
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	editRequested := strings.EqualFold(r.URL.Query().Get("mode"), "edit")
	if editRequested && in.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required for edit"})
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		s.writeJSON(w, 400, map[string]string{"error": "name is required"})
		return
	}
	isNew := true
	errKeyNotFound := errors.New("API key not found")
	err := s.Cfg.Update(func(c *config.Config) error {
		for i, k := range c.APIKeys {
			if k.ID == in.ID {
				isNew = false
				in.Key = k.Key // keep existing credential
				in.CreatedAt = k.CreatedAt
				in.LastUsed = k.LastUsed
				c.APIKeys[i] = &in
				return nil
			}
		}
		if editRequested {
			return errKeyNotFound
		}
		if in.ID == "" {
			in.ID = newKeyID()
		}
		in.Key = "sk-" + strings.ToLower(hex.EncodeToString(func() []byte {
			b := make([]byte, 24)
			_, _ = rand.Read(b)
			return b
		}()))
		in.CreatedAt = time.Now()
		c.APIKeys = append(c.APIKeys, &in)
		return nil
	})
	if err != nil {
		if errors.Is(err, errKeyNotFound) {
			s.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// return the fresh object (with generated key) to the client
	for _, k := range s.Cfg.Get().APIKeys {
		if k.ID == in.ID {
			s.writeJSON(w, 200, map[string]any{"ok": "saved", "key": k, "created": isNew})
			return
		}
	}
	s.writeJSON(w, 200, map[string]any{"ok": "saved", "key": in, "created": isNew})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	removed := false
	err := s.Cfg.Update(func(c *config.Config) error {
		out := c.APIKeys[:0]
		for _, k := range c.APIKeys {
			if k.ID == id {
				removed = true
			} else {
				out = append(out, k)
			}
		}
		c.APIKeys = out
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "API key not found"})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "deleted"})
}

// ---------------------------------------------------------------------------
// proxy rotation pool

// addProxies accepts a list of proxy URLs (protocol://host:port or
// protocol://user:pass@host:port) and appends new ones to the pool.
func normalizeAdminProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("proxy URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", errors.New("proxy URL must include protocol and host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5":
	default:
		return "", errors.New("proxy protocol must be http, https, or socks5")
	}
	return parsed.String(), nil
}

func (s *Server) addProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	validated := make([]string, 0, len(in.URLs))
	for index, raw := range in.URLs {
		normalized, err := normalizeAdminProxyURL(raw)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("proxy %d: %v", index+1, err)})
			return
		}
		validated = append(validated, normalized)
	}
	if len(validated) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one proxy URL is required"})
		return
	}
	added := []string{}
	err := s.Cfg.Update(func(c *config.Config) error {
		seen := map[string]bool{}
		for _, e := range c.Proxies.List {
			seen[strings.TrimSpace(e.URL)] = true
		}
		for _, u := range validated {
			if seen[u] {
				continue
			}
			seen[u] = true
			c.Proxies.List = append(c.Proxies.List, &config.ProxyEntry{URL: u})
			added = append(added, u)
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]any{"ok": "added", "added": added})
}

func (s *Server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	if u == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	removed := false
	err := s.Cfg.Update(func(c *config.Config) error {
		out := c.Proxies.List[:0]
		for _, e := range c.Proxies.List {
			if e.URL == u {
				removed = true
			} else {
				out = append(out, e)
			}
		}
		c.Proxies.List = out
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "proxy not found"})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "deleted"})
}

func (s *Server) updateProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OldURL string `json:"old_url"`
		NewURL string `json:"new_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	in.OldURL = strings.TrimSpace(in.OldURL)
	if in.OldURL == "" || strings.TrimSpace(in.NewURL) == "" {
		s.writeJSON(w, 400, map[string]string{"error": "old_url and new_url required"})
		return
	}
	normalized, normalizeErr := normalizeAdminProxyURL(in.NewURL)
	if normalizeErr != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": normalizeErr.Error()})
		return
	}
	in.NewURL = normalized
	err := s.Cfg.Update(func(c *config.Config) error {
		for _, p := range c.Proxies.List {
			if p.URL == in.NewURL && p.URL != in.OldURL {
				return errors.New("proxy URL already exists")
			}
		}
		for _, p := range c.Proxies.List {
			if p.URL == in.OldURL {
				p.URL = in.NewURL
				return nil
			}
		}
		return errors.New("proxy not found")
	})
	if err != nil {
		s.writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]string{"ok": "updated"})
}

// checkProxies validates entries: exit-IP canary through the proxy,
// geo lookup (country, city, region), latency. Working + non-excluded proxies join the
// rotation pool automatically.
func (s *Server) checkProxies(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Get()
	checkURL := cfg.Proxies.CheckURL
	if checkURL == "" {
		checkURL = "https://api.ipify.org"
	}
	excluded := map[string]bool{}
	for _, c := range cfg.Proxies.ExcludeCountries {
		excluded[strings.ToUpper(strings.TrimSpace(c))] = true
	}

	var req struct {
		Proxies    []string `json:"proxies"`
		AddWorking bool     `json:"add_working"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var targets []string
	isCandidateCheck := len(req.Proxies) > 0
	if isCandidateCheck {
		targets = req.Proxies
	} else {
		for _, e := range cfg.Proxies.List {
			targets = append(targets, e.URL)
		}
	}

	results := make([]config.ProxyEntry, len(targets))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for i, uStr := range targets {
		wg.Add(1)
		go func(i int, uStr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			uStr = strings.TrimSpace(uStr)
			res := config.ProxyEntry{URL: uStr, LastCheck: time.Now()}
			res.Working, res.IP, res.Country, res.City, res.Region, res.LatencyMS, res.Error = checkOne(uStr, checkURL, excluded)
			res.Excluded = res.Working && excluded[strings.ToUpper(res.Country)]
			if res.Excluded {
				res.Working = false
				if res.Error == "" {
					res.Error = "country " + res.Country + " excluded"
				}
			}
			results[i] = res
		}(i, uStr)
	}
	wg.Wait()

	// persist results
	err := s.Cfg.Update(func(c *config.Config) error {
		if isCandidateCheck {
			existing := map[string]*config.ProxyEntry{}
			for _, e := range c.Proxies.List {
				existing[e.URL] = e
			}
			for _, r := range results {
				if r.Working && !r.Excluded {
					if ex, ok := existing[r.URL]; ok {
						*ex = r
					} else {
						copy := r
						c.Proxies.List = append(c.Proxies.List, &copy)
					}
				}
			}
		} else {
			for i, e := range c.Proxies.List {
				if i < len(results) && results[i].URL == e.URL {
					e.Working = results[i].Working
					e.Excluded = results[i].Excluded
					e.Country = results[i].Country
					e.City = results[i].City
					e.Region = results[i].Region
					e.IP = results[i].IP
					e.LatencyMS = results[i].LatencyMS
					e.Error = results[i].Error
					e.LastCheck = results[i].LastCheck
				}
			}
		}
		return nil
	})
	if err != nil {
		s.writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, 200, map[string]any{"ok": "checked", "proxies": results})
}

// checkOne probes one proxy. Returns working, exitIP, country, city, region, latency, error.
func checkOne(proxyURL, checkURL string, excluded map[string]bool) (bool, string, string, string, string, int64, string) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return false, "", "", "", "", 0, "invalid proxy url"
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" {
		return false, "", "", "", "", 0, "unsupported scheme " + u.Scheme
	}
	start := time.Now()
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(u),
			DisableCompression:  true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	resp, err := client.Get(checkURL)
	if err != nil {
		return false, "", "", "", "", time.Since(start).Milliseconds(), err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode >= 300 {
		return false, "", "", "", "", latency, fmt.Sprintf("canary HTTP %d", resp.StatusCode)
	}
	exitIP := strings.TrimSpace(string(body))
	country, city, region := lookupGeo(exitIP)
	return true, exitIP, country, city, region, latency, ""
}

func lookupGeo(ip string) (string, string, string) {
	if ip == "" || strings.ContainsAny(ip, "<>") {
		return "", "", ""
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("http://ip-api.com/json/" + ip + "?fields=status,countryCode,city,regionName")
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var m struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
		RegionName  string `json:"regionName"`
	}
	if json.Unmarshal(b, &m) == nil && m.Status == "success" {
		return m.CountryCode, m.City, m.RegionName
	}
	return "", "", ""
}

// ---------------------------------------------------------------------------
// usage + stats

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	snap := s.Store.Snapshot()
	type rec struct {
		KeyID     string `json:"key_id"`
		Model     string `json:"model"`
		Day       string `json:"day"`
		Requests  int64  `json:"requests"`
		Errors    int64  `json:"errors"`
		TokensIn  int64  `json:"tokens_in"`
		TokensOut int64  `json:"tokens_out"`
	}
	out := []rec{}
	for k, v := range snap {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, rec{
			KeyID: parts[0], Model: parts[1], Day: parts[2],
			Requests: v.Requests, Errors: v.Errors,
			TokensIn: v.TokensIn, TokensOut: v.TokensOut,
		})
	}
	// sort by day desc, then key
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Day > out[j-1].Day ||
			(out[j].Day == out[j-1].Day && out[j].KeyID < out[j-1].KeyID)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	s.writeJSON(w, 200, map[string]any{"records": out})
}

func (s *Server) stats(w http.ResponseWriter) {
	cfg := s.Cfg.Get()
	var reqs, errs, tin, tout int64
	if s.Store != nil {
		snap := s.Store.Snapshot()
		for _, v := range snap {
			reqs += v.Requests
			errs += v.Errors
			tin += v.TokensIn
			tout += v.TokensOut
		}
	}
	active := 0
	enabledUp := 0
	for _, k := range cfg.APIKeys {
		if k.Enabled {
			active++
		}
	}
	for _, u := range cfg.Upstreams {
		if u.Enabled {
			enabledUp++
		}
	}
	var usedModels []string
	if s.Proxy != nil {
		for _, model := range s.Proxy.UsedModels() {
			for _, up := range cfg.Upstreams {
				if up.ModelVisible(model) {
					usedModels = append(usedModels, model)
					break
				}
			}
		}
	}
	if usedModels == nil {
		usedModels = []string{}
	}
	s.writeJSON(w, 200, map[string]any{
		"requests":     reqs,
		"errors":       errs,
		"tokens_in":    tin,
		"tokens_out":   tout,
		"keys_total":   len(cfg.APIKeys),
		"keys_active":  active,
		"upstreams":    len(cfg.Upstreams),
		"upstreams_on": enabledUp,
		"proxies":      len(cfg.Proxies.List),
		"proxies_on": func() int {
			n := 0
			for _, e := range cfg.Proxies.List {
				if e.Working && !e.Excluded {
					n++
				}
			}
			return n
		}(),
		"used_models": usedModels,
	})
}

func (s *Server) cachedRoutes(w http.ResponseWriter) {
	ttlHours := s.Cfg.Get().Hitchance.UpstreamCacheTTLHours
	if ttlHours <= 0 {
		ttlHours = 24
	}
	var routes []CachedRouteEntry
	if s.Proxy != nil {
		routes = s.Proxy.CachedUpstreams()
	}
	if routes == nil {
		routes = []CachedRouteEntry{}
	}
	var names []string
	visibleRoutes := []CachedRouteEntry{}
	upstreamByID := map[string]any{}
	for _, route := range routes {
		for _, up := range s.Cfg.Get().Upstreams {
			if up.ID == route.UpstreamID && up.ModelVisible(route.Model) {
				names = append(names, route.Model)
				visibleRoutes = append(visibleRoutes, route)
				upstreamByID[up.ID] = map[string]any{
					"id": up.ID, "base_url": up.BaseURL, "api_keys": up.APIKeys,
					"enabled": up.Enabled, "hidden_invalid": up.HiddenInvalid,
				}
				break
			}
		}
	}
	upstreams := make([]any, 0, len(upstreamByID))
	for _, up := range s.Cfg.Get().Upstreams {
		if info, ok := upstreamByID[up.ID]; ok {
			upstreams = append(upstreams, info)
		}
	}
	s.writeJSON(w, 200, map[string]any{
		"presentation_models": modelalias.PresentationNames(names, s.Cfg.Get().Models.VariantOptions()),
		"routes":              visibleRoutes,
		"upstreams":           upstreams,
		"ttl_hours":           ttlHours,
	})
}
