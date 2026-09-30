package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"aisense/internal/vault"
)

// The public config names a generation in the encrypted credential store. A
// save writes the new generation first and retains the old one, so a crash
// between the two file replacements cannot make the old config lose its keys.
type credentialStore struct {
	Versions map[string]credentialSnapshot `json:"versions"`
}

type credentialSnapshot struct {
	Admin      adminCredentials               `json:"admin"`
	ClientKeys map[string]string              `json:"client_keys"`
	Upstreams  map[string]upstreamCredentials `json:"upstreams"`
	Proxies    map[string]string              `json:"proxies"`
}

type adminCredentials struct {
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	Token           string `json:"token,omitempty"`
	TOTPSecret      string `json:"totp_secret,omitempty"`
	TOTPLastCounter int64  `json:"totp_last_counter,omitempty"`
}

type upstreamCredentials struct {
	APIKeys           []string `json:"api_keys,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	OAuthTokenURL     string   `json:"oauth_token_url,omitempty"`
	OAuthClientID     string   `json:"oauth_client_id,omitempty"`
	OAuthClientSecret string   `json:"oauth_client_secret,omitempty"`
}

func credentialsPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return filepath.Join(filepath.Dir(path), name+".credentials.vault")
}

func credentialID(id string, index int) string {
	if id == "" {
		return "index:" + strconv.Itoa(index)
	}
	return "id:" + id
}

// redactCredentialURL leaves ordinary URLs readable. A URL containing userinfo
// or a query is kept in the vault, since both commonly carry credentials.
func redactCredentialURL(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", true
	}
	if parsed.User == nil && parsed.RawQuery == "" {
		return raw, false
	}
	parsed.User = nil
	parsed.RawQuery = ""
	return parsed.String(), true
}

func splitCredentials(cfg *Config) (*Config, credentialSnapshot, error) {
	public := *cfg
	secrets := credentialSnapshot{
		Admin: adminCredentials{
			Username: cfg.Server.Admin.Username, Password: cfg.Server.Admin.Password,
			Token: cfg.Server.Admin.Token, TOTPSecret: cfg.Server.Admin.TOTPSecret,
			TOTPLastCounter: cfg.Server.Admin.TOTPLastCounter,
		},
		ClientKeys: make(map[string]string),
		Upstreams:  make(map[string]upstreamCredentials),
		Proxies:    make(map[string]string),
	}
	public.Server.Admin.Username = ""
	public.Server.Admin.Password = ""
	public.Server.Admin.Token = ""
	public.Server.Admin.TOTPSecret = ""
	public.Server.Admin.TOTPLastCounter = 0
	public.APIKeys = make([]*APIKey, len(cfg.APIKeys))
	for i, key := range cfg.APIKeys {
		if key == nil {
			continue
		}
		id := credentialID(key.ID, i)
		if _, exists := secrets.ClientKeys[id]; exists {
			return nil, secrets, fmt.Errorf("duplicate client API key ID %q", key.ID)
		}
		secrets.ClientKeys[id] = key.Key
		copy := *key
		copy.Key = ""
		public.APIKeys[i] = &copy
	}
	public.Upstreams = cloneUpstreams(cfg.Upstreams)
	for i, up := range public.Upstreams {
		if up == nil {
			continue
		}
		id := credentialID(up.ID, i)
		if _, exists := secrets.Upstreams[id]; exists {
			return nil, secrets, fmt.Errorf("duplicate upstream ID %q", up.ID)
		}
		saved := upstreamCredentials{APIKeys: append([]string(nil), up.APIKeys...)}
		up.APIKeys = nil
		if clean, sensitive := redactCredentialURL(up.BaseURL); sensitive {
			saved.BaseURL = up.BaseURL
			up.BaseURL = clean
		}
		if up.OAuth != nil {
			saved.OAuthClientID = up.OAuth.ClientID
			saved.OAuthClientSecret = up.OAuth.ClientSecret
			up.OAuth.ClientID = ""
			up.OAuth.ClientSecret = ""
			if clean, sensitive := redactCredentialURL(up.OAuth.TokenURL); sensitive {
				saved.OAuthTokenURL = up.OAuth.TokenURL
				up.OAuth.TokenURL = clean
			}
		}
		secrets.Upstreams[id] = saved
	}
	public.Proxies.List = make([]*ProxyEntry, len(cfg.Proxies.List))
	for i, proxy := range cfg.Proxies.List {
		if proxy == nil {
			continue
		}
		copy := *proxy
		if clean, sensitive := redactCredentialURL(copy.URL); sensitive {
			secrets.Proxies[strconv.Itoa(i)] = copy.URL
			copy.URL = clean
		}
		public.Proxies.List[i] = &copy
	}
	return &public, secrets, nil
}

func applyCredentials(cfg *Config, secrets credentialSnapshot) error {
	cfg.Server.Admin.Username = secrets.Admin.Username
	cfg.Server.Admin.Password = secrets.Admin.Password
	cfg.Server.Admin.Token = secrets.Admin.Token
	cfg.Server.Admin.TOTPSecret = secrets.Admin.TOTPSecret
	cfg.Server.Admin.TOTPLastCounter = secrets.Admin.TOTPLastCounter
	for i, key := range cfg.APIKeys {
		if key == nil {
			continue
		}
		value, ok := secrets.ClientKeys[credentialID(key.ID, i)]
		if !ok {
			return fmt.Errorf("credentials missing client API key %q", key.ID)
		}
		key.Key = value
	}
	for i, up := range cfg.Upstreams {
		if up == nil {
			continue
		}
		saved, ok := secrets.Upstreams[credentialID(up.ID, i)]
		if !ok {
			return fmt.Errorf("credentials missing upstream %q", up.ID)
		}
		up.APIKeys = append([]string(nil), saved.APIKeys...)
		if saved.BaseURL != "" {
			clean, _ := redactCredentialURL(saved.BaseURL)
			if up.BaseURL != clean {
				return fmt.Errorf("upstream %q URL no longer matches its credentials", up.ID)
			}
			up.BaseURL = saved.BaseURL
		}
		if up.OAuth != nil {
			up.OAuth.ClientID = saved.OAuthClientID
			up.OAuth.ClientSecret = saved.OAuthClientSecret
			if saved.OAuthTokenURL != "" {
				clean, _ := redactCredentialURL(saved.OAuthTokenURL)
				if up.OAuth.TokenURL != clean {
					return fmt.Errorf("upstream %q OAuth URL no longer matches its credentials", up.ID)
				}
				up.OAuth.TokenURL = saved.OAuthTokenURL
			}
		}
	}
	for index, fullURL := range secrets.Proxies {
		i, err := strconv.Atoi(index)
		if err != nil || i < 0 || i >= len(cfg.Proxies.List) || cfg.Proxies.List[i] == nil {
			return fmt.Errorf("proxy credentials refer to missing entry %q", index)
		}
		clean, _ := redactCredentialURL(fullURL)
		if cfg.Proxies.List[i].URL != clean {
			return fmt.Errorf("proxy %d URL no longer matches its credentials", i)
		}
		cfg.Proxies.List[i].URL = fullURL
	}
	return nil
}

func readCredentialStore(path string) (credentialStore, error) {
	data, err := os.ReadFile(credentialsPath(path))
	if err != nil {
		return credentialStore{}, err
	}
	plain, err := vault.Open(data)
	if err != nil {
		return credentialStore{}, err
	}
	var store credentialStore
	if err := json.Unmarshal(plain, &store); err != nil {
		return credentialStore{}, err
	}
	return store, nil
}

func readConfig(path string, cfg *Config) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	plain, sealed, err := vault.OpenOrPlain(raw)
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(plain, cfg); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.CredentialsRevision != "" {
		store, err := readCredentialStore(path)
		if err != nil {
			return nil, false, fmt.Errorf("read credentials for %s: %w", path, err)
		}
		secrets, ok := store.Versions[cfg.CredentialsRevision]
		if !ok {
			return nil, false, fmt.Errorf("credentials revision %q is unavailable", cfg.CredentialsRevision)
		}
		if err := applyCredentials(cfg, secrets); err != nil {
			return nil, false, err
		}
	}
	return plain, sealed, nil
}

func newCredentialRevision() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// writeSplit keeps the previous credential generation until the public config
// has been replaced. It only rewrites the vault when credentials changed.
func writeSplit(path string, cfg *Config, lastWritten []byte) ([]byte, error) {
	public, secrets, err := splitCredentials(cfg)
	if err != nil {
		return nil, err
	}
	store := credentialStore{Versions: make(map[string]credentialSnapshot)}
	var previous credentialSnapshot
	if cfg.CredentialsRevision != "" {
		store, err = readCredentialStore(path)
		if err != nil {
			return nil, fmt.Errorf("read credentials: %w", err)
		}
		var ok bool
		previous, ok = store.Versions[cfg.CredentialsRevision]
		if !ok {
			return nil, fmt.Errorf("credentials revision %q is unavailable", cfg.CredentialsRevision)
		}
	}
	if cfg.CredentialsRevision == "" || !reflect.DeepEqual(previous, secrets) {
		revision, err := newCredentialRevision()
		if err != nil {
			return nil, err
		}
		versions := map[string]credentialSnapshot{revision: secrets}
		if cfg.CredentialsRevision != "" {
			versions[cfg.CredentialsRevision] = previous
		}
		store.Versions = versions
		payload, err := json.Marshal(store)
		if err != nil {
			return nil, err
		}
		if err := vault.WriteSealed(credentialsPath(path), payload); err != nil {
			return nil, fmt.Errorf("write credentials: %w", err)
		}
		cfg.CredentialsRevision = revision
		public.CredentialsRevision = revision
	}
	plain, err := json.MarshalIndent(public, "", "  ")
	if err != nil {
		return nil, err
	}
	if bytes.Equal(plain, lastWritten) {
		return plain, nil
	}
	if err := vault.WritePlain(path, plain); err != nil {
		return nil, err
	}
	return plain, nil
}
