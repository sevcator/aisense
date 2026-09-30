package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aisense/internal/hitchance"
	"aisense/internal/modelalias"
	"aisense/internal/vault"
)

type ListenerCfg struct {
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`
	Path    string `json:"path"`
}

type AdminCfg struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
	Path     string `json:"path"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"` // legacy: migrated to Password on startup
	// TOTPSecret enables two-factor authentication for the admin panel when
	// non-empty (base32 RFC 6238 secret).
	TOTPSecret      string `json:"totp_secret,omitempty"`
	TOTPLastCounter int64  `json:"totp_last_counter,omitempty"`
}

type ServerCfg struct {
	OpenAI    ListenerCfg `json:"openai"`
	Anthropic ListenerCfg `json:"anthropic"`
	Admin     AdminCfg    `json:"admin"`
}

type UsageCfg struct {
	Enabled bool `json:"enabled"`
}

type ModelDiscoveryCfg struct {
	Enabled                bool `json:"enabled"`
	RefreshIntervalMinutes int  `json:"refresh_interval_minutes"`
	// AutoFixProblems: on "The plain HTTP request was sent to HTTPS port"
	// errors, rewrite the upstream base_url from http:// to https:// (and
	// merge with an existing https twin — see fixUpstreamToHTTPS).
	AutoFixProblems bool `json:"auto_fix_problems"`
	// IgnoreCertErrors: skip TLS certificate verification for upstream
	// requests (model discovery, upstream test, proxy forwarding).
	IgnoreCertErrors bool `json:"ignore_cert_errors"`
}

// ModelsCfg controls model list presentation and variant routing.
type ModelsCfg struct {
	ReasoningVariants   *bool `json:"reasoning_variants,omitempty"`
	PreferOtherVariants bool  `json:"prefer_other_variants"`
	// FastMode hides "-fast" model variants from the model list and prefers
	// them over the regular model when one exists upstream.
	FastMode bool `json:"fast_mode"`
}

// TierPricingCfg controls the online price catalog behind the built-in
// "best"/"shit" tier router. Prices refresh in the background so tier
// rankings follow the current market rather than a shipped snapshot.
type TierPricingCfg struct {
	Enabled                bool `json:"enabled"`
	RefreshIntervalMinutes int  `json:"refresh_interval_minutes"`
}

// Missing reasoning_variants preserves the shipped effort-routing default.
func (c ModelsCfg) VariantOptions() modelalias.VariantOptions {
	return modelalias.VariantOptions{Reasoning: c.ReasoningVariants == nil || *c.ReasoningVariants, Other: c.PreferOtherVariants, Fast: c.FastMode}
}

type Upstream struct {
	Health         *UpstreamHealth            `json:"health,omitempty"`
	HitchanceState map[string]hitchance.State `json:"hitchance_state,omitempty"`
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Type           string                     `json:"type"` // legacy identity hint, not a protocol filter
	BaseURL        string                     `json:"base_url"`
	APIKeys        []string                   `json:"api_keys,omitempty"`
	Models         []string                   `json:"models"`
	ModelAliases   map[string][]string        `json:"model_aliases,omitempty"`
	BlockedModels  []string                   `json:"blocked_models,omitempty"`
	// AllowedModels is the whitelist counterpart of BlockedModels: when set,
	// only these model patterns are visible and routable on this upstream.
	AllowedModels []string `json:"allowed_models,omitempty"`
	HiddenInvalid bool     `json:"hidden_invalid,omitempty"`
	// Fingerprinted credentials map to exact raw routes, not entire model families.
	KeyBlockedModels map[string][]string `json:"key_blocked_models,omitempty"`
	Priority         int                 `json:"priority"`
	Enabled          bool                `json:"enabled"`
	FailCodes        []int               `json:"-"` // retired; ignored on disk and never emitted or routed
	// AuthMode: "swap" (use api_key), "passthrough" (keep client auth as-is),
	// "none" (no auth), "oauth" (fetch bearer via client_credentials).
	AuthMode string    `json:"auth_mode,omitempty"`
	OAuth    *OAuthCfg `json:"oauth,omitempty"`
	UseProxy bool      `json:"use_proxy,omitempty"`

	ModelsRefreshedAt  time.Time `json:"models_refreshed_at,omitempty"`
	ModelsRefreshError string    `json:"models_refresh_error,omitempty"`
}

// UnmarshalJSON accepts the legacy singular api_key field while keeping
// api_keys as the only canonical field emitted by json.Marshal.
func (u *Upstream) UnmarshalJSON(data []byte) error {
	type plain Upstream
	var in struct {
		*plain
		LegacyAPIKey string `json:"api_key"`
	}
	in.plain = (*plain)(u)
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	u.APIKeys = NormalizeAPIKeys(append(u.APIKeys, in.LegacyAPIKey))
	return nil
}

type OAuthCfg struct {
	TokenURL     string `json:"token_url"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type ProxyEntry struct {
	URL       string    `json:"url"`
	Working   bool      `json:"working"`
	Excluded  bool      `json:"excluded"`
	Country   string    `json:"country,omitempty"`
	City      string    `json:"city,omitempty"`
	Region    string    `json:"region,omitempty"`
	IP        string    `json:"ip,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	LastCheck time.Time `json:"last_check,omitempty"`
}

type ProxyCfg struct {
	Enabled          bool          `json:"enabled"`
	CheckURL         string        `json:"check_url"` // exit-IP canary
	ExcludeCountries []string      `json:"exclude_countries"`
	List             []*ProxyEntry `json:"list"`
}

type APIKey struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Key           string   `json:"key,omitempty"`
	Enabled       bool     `json:"enabled"`
	AllowedModels []string `json:"allowed_models"`
	// BlockedModels is the blacklist counterpart of AllowedModels: a matching
	// model is refused for this key even when whitelists would allow it.
	BlockedModels []string  `json:"blocked_models,omitempty"`
	RPM           int       `json:"rpm"`
	RPD           int       `json:"rpd"`
	TPD           int       `json:"tpd"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsed      time.Time `json:"last_used,omitempty"`
}

type Config struct {
	CredentialsRevision string            `json:"credentials_revision,omitempty"`
	Hitchance           hitchance.Config  `json:"hitchance"`
	AutoModelsDiscovery bool              `json:"auto_models_discovery"`
	LoggingEnabled      bool              `json:"logging_enabled"`
	Server              ServerCfg         `json:"server"`
	Usage               UsageCfg          `json:"usage"`
	ModelDiscovery      ModelDiscoveryCfg `json:"model_discovery"`
	Models              ModelsCfg         `json:"models"`
	TierPricing         TierPricingCfg    `json:"tier_pricing"`
	Proxies             ProxyCfg          `json:"proxies"`
	Upstreams           []*Upstream       `json:"upstreams"`
	APIKeys             []*APIKey         `json:"api_keys"`
	CachedRoutes        []CachedRoute     `json:"cached_routes,omitempty"`
	// Combos are user-made model names that stand for an ordered list of models.
	Combos []*ModelCombo `json:"combos,omitempty"`
}

// DefaultAdminPassword is the admin panel password of a fresh install. It is
// filled in at start-up, not in Default(), so an old admin token still migrates.
const DefaultAdminPassword = "admin"

func Default() *Config {
	return &Config{
		Hitchance: hitchance.Default(),
		Server: ServerCfg{
			OpenAI:    ListenerCfg{Enabled: true, Port: 8080, Path: "/v1"},
			Anthropic: ListenerCfg{Enabled: true, Port: 8081, Path: "/v1"},
			Admin:     AdminCfg{Enabled: true, Port: 8082, Path: "/", Username: "admin"},
		},
		Usage: UsageCfg{
			Enabled: true,
		},
		ModelDiscovery: ModelDiscoveryCfg{Enabled: false, RefreshIntervalMinutes: 60, AutoFixProblems: true},
		TierPricing:    TierPricingCfg{Enabled: true, RefreshIntervalMinutes: 60},
		Proxies: ProxyCfg{
			Enabled:          false,
			CheckURL:         "https://api.ipify.org",
			ExcludeCountries: []string{},
			List:             []*ProxyEntry{},
		},
		Upstreams: []*Upstream{},
		APIKeys:   []*APIKey{},
	}
}

type Manager struct {
	path            string
	mu              sync.RWMutex
	cfg             *Config
	active          atomic.Pointer[Config]
	validateLogging func(bool) error
	// lastWritten is the plain text of the file as this process last wrote it.
	// Observations that change nothing then cost no encryption and no disk write.
	lastWritten []byte
}

// SetLoggingValidator installs the disk check before listeners and Watch start.
func (m *Manager) SetLoggingValidator(fn func(bool) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := fn(m.cfg.LoggingEnabled); err != nil {
		return err
	}
	m.validateLogging = fn
	return nil
}

func Load(path string) (*Manager, error) {
	m := &Manager{path: path}
	// Services and containers may have no home directory; keep the key beside
	// the config there rather than refusing to start.
	if dir := filepath.Dir(path); dir != "" {
		vault.SetFallbackKeyDir(dir)
	}
	cfg := Default()
	loaded, wasSealed := false, false
	var original []byte
	if b, sealed, err := readConfig(path, cfg); err == nil {
		loaded, wasSealed, original = true, sealed, b
		if err := migrateHitchanceScheduler(b, &cfg.Hitchance); err != nil {
			return nil, err
		}
		// Untouched copies of older default rule sets become the current rules,
		// so an updated binary gains new default behavior. Customised policies
		// never match and are left alone.
		if reflect.DeepEqual(cfg.Hitchance.Rules, hitchance.PreviousExpandedDefaultRules()) ||
			reflect.DeepEqual(cfg.Hitchance.Rules, hitchance.LegacyDefaultRules()) ||
			reflect.DeepEqual(cfg.Hitchance.Rules, hitchance.PreviousDefaultRules()) {
			cfg.Hitchance.Rules = hitchance.Default().Rules
		}
		// Retry cycles are no longer a user limit; old files migrate to
		// request-scoped retries that continue until the client disconnects.
		cfg.Hitchance.RetryCycles = 0
	} else {
		if !os.IsNotExist(err) {
			// Never treat an unreadable file as empty: that could overwrite keys.
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if _, vaultErr := os.Stat(credentialsPath(path)); vaultErr == nil {
			return nil, fmt.Errorf("settings file %s is missing but its credential store exists; restore the settings file before starting", path)
		} else if !os.IsNotExist(vaultErr) {
			return nil, fmt.Errorf("check credentials for %s: %w", path, vaultErr)
		}
		if err := m.persist(cfg); err != nil {
			return nil, err
		}
	}
	cfg.Server.Admin.Enabled = true
	cfg.Server.Admin.Path = NormalizeAdminPath(cfg.Server.Admin.Path)
	normalizeModelDiscovery(&cfg.ModelDiscovery)
	normalizeTierPricing(&cfg.TierPricing)
	normalizeUpstreams(cfg)
	cfg.Combos = NormalizeCombos(cfg.Combos)
	if err := cfg.Hitchance.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateCombos(cfg.Combos); err != nil {
		return nil, err
	}
	if loaded {
		public, _, err := splitCredentials(cfg)
		if err != nil {
			return nil, err
		}
		canonical, _ := json.MarshalIndent(public, "", "  ")
		// Legacy sealed and plaintext files are migrated to split storage.
		if wasSealed || cfg.CredentialsRevision == "" || !bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(canonical)) {
			if err := m.persist(cfg); err != nil {
				return nil, err
			}
		} else {
			m.lastWritten = canonical
		}
	}
	m.cfg = cfg
	m.active.Store(cfg)
	return m, nil
}

func (m *Manager) Get() *Config { return m.active.Load() }

// Path is the absolute location of the settings file in use by this process.
func (m *Manager) Path() string {
	absolute, err := filepath.Abs(m.path)
	if err != nil {
		return m.path
	}
	return absolute
}

// ReadFile returns a complete logical config, including credentials. Tools
// must treat this result as sensitive and use WriteFile to save it again.
func ReadFile(path string) ([]byte, error) {
	cfg := &Config{}
	if _, _, err := readConfig(path, cfg); err != nil {
		return nil, err
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// ExportFile produces a self-contained plaintext config for transfer. The
// destination must create its own credential generation and vault.
func ExportFile(path string) ([]byte, error) {
	cfg := &Config{}
	if _, _, err := readConfig(path, cfg); err != nil {
		return nil, err
	}
	cfg.CredentialsRevision = ""
	return json.MarshalIndent(cfg, "", "  ")
}

// WriteFile splits a complete logical config into readable settings and the
// encrypted credential store. It atomically replaces each file in order.
func WriteFile(path string, plain []byte) error {
	var cfg Config
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return err
	}
	_, err := writeSplit(path, &cfg, nil)
	return err
}

func (m *Manager) persist(cfg *Config) error {
	plain, err := writeSplit(m.path, cfg, m.lastWritten)
	if err != nil {
		return err
	}
	m.lastWritten = plain
	return nil
}

// Update applies a mutation under lock, persists, and atomically publishes.
func (m *Manager) Update(fn func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.cfg
	next := *cur // shallow copy
	next.Upstreams = cloneUpstreams(cur.Upstreams)
	next.APIKeys = cloneAPIKeys(cur.APIKeys)
	next.Proxies = cloneProxies(cur.Proxies)
	next.Hitchance = cur.Hitchance.Clone()
	next.Combos = cloneCombos(cur.Combos)
	if err := fn(&next); err != nil {
		return err
	}
	if err := next.Hitchance.Validate(); err != nil {
		return err
	}
	next.Combos = NormalizeCombos(next.Combos)
	if err := ValidateCombos(next.Combos); err != nil {
		return err
	}
	next.Server.Admin.Enabled = true
	next.Server.Admin.Path = NormalizeAdminPath(next.Server.Admin.Path)
	normalizeModelDiscovery(&next.ModelDiscovery)
	normalizeTierPricing(&next.TierPricing)
	normalizeUpstreams(&next)
	if m.validateLogging != nil && next.LoggingEnabled {
		if err := m.validateLogging(true); err != nil {
			return fmt.Errorf("logging: %w", err)
		}
	}
	if err := m.persist(&next); err != nil {
		return err
	}
	m.cfg = &next
	m.active.Store(&next)
	return nil
}

// Mutate applies an in-memory-only mutation (no disk persist). Used for
// hot counters like last_used.
func (m *Manager) Mutate(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := *m.cfg
	next.Upstreams = cloneUpstreams(m.cfg.Upstreams)
	next.APIKeys = cloneAPIKeys(m.cfg.APIKeys)
	next.Proxies = cloneProxies(m.cfg.Proxies)
	fn(&next)
	next.Server.Admin.Enabled = true
	next.Server.Admin.Path = NormalizeAdminPath(next.Server.Admin.Path)
	normalizeModelDiscovery(&next.ModelDiscovery)
	normalizeTierPricing(&next.TierPricing)
	normalizeUpstreams(&next)
	m.cfg = &next
	m.active.Store(&next)
}

// Watch polls file mtime and reloads when the file changed on disk.
func (m *Manager) Watch(interval time.Duration) {
	go func() {
		var last time.Time
		if st, err := os.Stat(m.path); err == nil {
			last = st.ModTime()
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			func() {
				// Serialize the entire disk read through publication with Update.
				// Locking only publication can resurrect keys from a stale file.
				m.mu.Lock()
				defer m.mu.Unlock()
				st, err := os.Stat(m.path)
				if err != nil || !st.ModTime().After(last) {
					return
				}
				last = st.ModTime()
				cfg := *Default()
				b, _, err := readConfig(m.path, &cfg)
				if err != nil {
					log.Printf("[config] reload rejected: %v", err)
					return
				}
				if err := migrateHitchanceScheduler(b, &cfg.Hitchance); err != nil {
					return
				}
				cfg.Hitchance.RetryCycles = 0
				if err := cfg.Hitchance.Validate(); err != nil {
					log.Printf("[config] hitchance reload rejected: %v", err)
					return
				}
				cfg.Server.Admin.Enabled = true
				cfg.Server.Admin.Path = NormalizeAdminPath(cfg.Server.Admin.Path)
				normalizeModelDiscovery(&cfg.ModelDiscovery)
				normalizeTierPricing(&cfg.TierPricing)
				normalizeUpstreams(&cfg)
				if m.validateLogging != nil && cfg.LoggingEnabled {
					if err := m.validateLogging(true); err != nil {
						log.Printf("[config] logging reload rejected: %v", err)
						return
					}
				}
				m.cfg = &cfg
				m.active.Store(&cfg)
				m.lastWritten = nil // the file no longer matches our last write
			}()
		}
	}()
}

// normalizeUpstreamURLs repairs base_urls that were saved with a wrapper
// (OmniRoute paste format @url:`https://host:port/v1`, stray quotes/backticks).
func normalizeUpstreams(cfg *Config) {
	kept := cfg.Upstreams[:0]
	for _, up := range cfg.Upstreams {
		if up == nil {
			continue
		}
		if cleaned := NormalizeBaseURL(up.BaseURL); cleaned != up.BaseURL {
			up.BaseURL = cleaned
		}
		if up.Health != nil && up.Health.Identity != up.HitchanceIdentity() {
			up.Health = nil
		}
		for target, state := range up.HitchanceState {
			if state.Identity != "" && state.Identity != up.HitchanceIdentity() {
				delete(up.HitchanceState, target)
			}
		}
		up.APIKeys = NormalizeAPIKeys(up.APIKeys)
		// Older versions disabled an upstream after auto-deleting its last
		// invalid key. Remove those exhausted records on load as well.
		if len(up.APIKeys) == 0 && !up.Enabled && (up.AuthMode == "" || up.AuthMode == "swap") {
			deletedLastKey := false
			for target, state := range up.HitchanceState {
				if strings.HasPrefix(target, "key:") && state.Action == "delete" {
					deletedLastKey = true
					break
				}
			}
			if deletedLastKey {
				continue
			}
		}
		up.Models, up.ModelAliases = modelalias.Normalize(up.Models, up.ModelAliases)
		kept = append(kept, up)
	}
	cfg.Upstreams = kept
	cfg.Upstreams, _, _ = ConsolidateExactUpstreams(cfg.Upstreams)
	normalizeCachedRoutes(cfg)
}

func cloneUpstreams(in []*Upstream) []*Upstream {
	out := make([]*Upstream, 0, len(in))
	for _, up := range in {
		if up == nil {
			out = append(out, nil)
			continue
		}
		cp := *up
		if up.Health != nil {
			health := *up.Health
			if health.LastSuccessAt != nil {
				value := *health.LastSuccessAt
				health.LastSuccessAt = &value
			}
			if health.LastFailureAt != nil {
				value := *health.LastFailureAt
				health.LastFailureAt = &value
			}
			cp.Health = &health
		}
		if up.HitchanceState != nil {
			cp.HitchanceState = make(map[string]hitchance.State, len(up.HitchanceState))
			for k, v := range up.HitchanceState {
				cp.HitchanceState[k] = v
			}
		}
		cp.APIKeys = append([]string(nil), up.APIKeys...)
		cp.Models = append([]string(nil), up.Models...)
		cp.ModelAliases = CloneModelAliases(up.ModelAliases)
		cp.BlockedModels = append([]string(nil), up.BlockedModels...)
		cp.KeyBlockedModels = CloneModelAliases(up.KeyBlockedModels)
		cp.FailCodes = append([]int(nil), up.FailCodes...)
		if up.OAuth != nil {
			oauth := *up.OAuth
			cp.OAuth = &oauth
		}
		out = append(out, &cp)
	}
	return out
}

func cloneAPIKeys(in []*APIKey) []*APIKey {
	out := make([]*APIKey, len(in))
	for i, key := range in {
		if key == nil {
			continue
		}
		copy := *key
		copy.AllowedModels = append([]string(nil), key.AllowedModels...)
		copy.BlockedModels = append([]string(nil), key.BlockedModels...)
		out[i] = &copy
	}
	return out
}

func cloneProxies(in ProxyCfg) ProxyCfg {
	out := in
	out.ExcludeCountries = append([]string(nil), in.ExcludeCountries...)
	out.List = make([]*ProxyEntry, len(in.List))
	for i, proxy := range in.List {
		if proxy != nil {
			copy := *proxy
			out.List[i] = &copy
		}
	}
	return out
}

func CloneModelAliases(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for model, aliases := range in {
		out[model] = append([]string(nil), aliases...)
	}
	return out
}

func normalizeModelDiscovery(cfg *ModelDiscoveryCfg) {
	if cfg.RefreshIntervalMinutes <= 0 {
		cfg.RefreshIntervalMinutes = 60
	}
	// HTTP/HTTPS auto-detection is a default behavior, not a user-facing toggle.
	cfg.AutoFixProblems = true
}

// normalizeTierPricing keeps the refresh cadence inside a polite band: the
// catalogs are public and free, but they are someone else's service.
func normalizeTierPricing(cfg *TierPricingCfg) {
	if cfg.RefreshIntervalMinutes < 15 {
		cfg.RefreshIntervalMinutes = 60
	}
}

// NormalizeAdminPath returns a clean absolute URL path for the admin panel.
func NormalizeAdminPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if strings.ContainsAny(value, "?#\\") || strings.ContainsRune(value, 0) {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = pathpkg.Clean(value)
	if value == "." || value == "" {
		return "/"
	}
	return value
}

func jsonFieldPresent(data []byte, object, field string) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	var child map[string]json.RawMessage
	if json.Unmarshal(root[object], &child) != nil {
		return false
	}
	_, ok := child[field]
	return ok
}
