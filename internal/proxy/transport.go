package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"aisense/internal/config"
	"aisense/internal/protocol"
)

// TestRequest shares forwarding authentication, TLS and proxy selection, but
// never runs failover, HTTPS repair, usage accounting or config mutations.
func (p *Proxy) TestRequest(ctx context.Context, up *config.Upstream, key, operation string, body []byte) (*http.Response, error) {
	if up.AuthMode == "passthrough" {
		return nil, fmt.Errorf("passthrough requires client credentials")
	}
	operation, query, _ := strings.Cut(operation, "?")
	method := http.MethodPost
	if operation == "models" {
		method = http.MethodGet
	}
	r, err := http.NewRequestWithContext(ctx, method, protocol.Endpoint(up.BaseURL, operation), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if operation == "models" {
		r.URL.RawQuery = query
	}
	format := protocol.Format(operation)
	if operation == "models" && up.Type == "anthropic" {
		format = "anthropic"
	}
	req, err := p.buildUpstreamRequest(up, format, r, body, r.URL.String(), key, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return p.httpClientFor(up.UseProxy).Do(req)
}

func (p *Proxy) CloseTestConnections() {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	for _, client := range p.clients {
		client.CloseIdleConnections()
	}
}

// ---------------------------------------------------------------------------
// proxy rotation

// workingProxies returns the rotation pool (working, not excluded).
func (p *Proxy) workingProxies() []*config.ProxyEntry {
	cfg := p.Cfg.Get()
	if !cfg.Proxies.Enabled {
		return nil
	}
	var out []*config.ProxyEntry
	for _, e := range cfg.Proxies.List {
		if e.Working && !e.Excluded {
			out = append(out, e)
		}
	}
	if cfg.Proxies.Tor.Enabled {
		port := cfg.Proxies.Tor.SOCKSPort
		if port == 0 {
			port = 9050
		}
		out = append(out, &config.ProxyEntry{URL: fmt.Sprintf("socks5://127.0.0.1:%d", port), Source: "tor", Working: true})
	}
	return out
}

var (
	rrMu  sync.Mutex
	rrIdx int
	rrKey string // rotate counter per pool identity
)

// nextProxyURL round-robins the working pool. Returns "" when no proxy.
func (p *Proxy) nextProxyURL() string {
	pool := p.workingProxies()
	if len(pool) == 0 {
		return ""
	}
	rrMu.Lock()
	key := fmt.Sprintf("%p", p)
	if key != rrKey {
		rrIdx = 0
		rrKey = key
	}
	selected := pool[rrIdx%len(pool)]
	rrIdx++
	rrMu.Unlock()
	if selected.Source == "tor" && p.tor != nil {
		return p.tor.next(p.Cfg.Get().Proxies.Tor)
	}
	return selected.URL
}

// ---------------------------------------------------------------------------
// pooled HTTP clients
//
// The original implementation built a brand new *http.Client (and therefore a
// brand new *http.Transport, connection pool, and TLS session cache) on every
// single upstream request. That throws away keep-alive connections and forces
// a fresh TCP+TLS handshake per call — expensive under load. Clients are now
// cached per destination (direct, or per rotation-proxy URL) and reused for
// the lifetime of the process, so HTTP/1.1 keep-alive and HTTP/2 connection
// multiplexing actually kick in.

const (
	directClientKey = "\x00direct"
)

// httpClientFor returns a pooled *http.Client for either a direct connection
// or (when useProxy is set and the pool has an entry) a rotation proxy. The
// same round-robin selection as before is preserved — each call to
// nextProxyURL still advances the rotation index — but the resulting client
// is cached and its connections reused across requests to that same proxy.
func (p *Proxy) httpClientFor(useProxy bool) *http.Client {
	key := directClientKey
	proxyURL := ""
	if useProxy && p.Cfg.Get().Proxies.Enabled {
		if pu := p.nextProxyURL(); pu != "" {
			proxyURL = pu
			key = pu
		}
	}
	// cert-ignore toggles the TLS layer: keep a separate pooled client so a
	// runtime settings change can't reuse a client built for the other mode.
	if p.Cfg.Get().ModelDiscovery.IgnoreCertErrors {
		key = "insecure|" + key
	}

	p.clientMu.RLock()
	c := p.clients[key]
	p.clientMu.RUnlock()
	if c != nil {
		return c
	}

	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if c := p.clients[key]; c != nil {
		return c
	}
	tr := &http.Transport{
		DisableCompression:    true, // pass bodies byte-for-byte, keep Content-Encoding
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if p.Cfg.Get().ModelDiscovery.IgnoreCertErrors {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(parsed)
		}
	}
	c = &http.Client{Timeout: 15 * time.Minute, Transport: tr, CheckRedirect: protocol.NoRedirect}
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	p.clients[key] = c
	return c
}

// ---------------------------------------------------------------------------
// oauth client_credentials token cache

type oauthToken struct {
	token  string
	expiry time.Time
}

type oauthCache struct {
	mu    sync.Mutex
	toks  map[string]oauthToken
	oauth map[string]*oauthToken
}

var oc = oauthCache{toks: map[string]oauthToken{}, oauth: map[string]*oauthToken{}}

func (p *Proxy) oauthBearer(ctx context.Context, up *config.Upstream) (string, error) {
	if up.OAuth == nil || up.OAuth.TokenURL == "" {
		return "", fmt.Errorf("upstream %s: oauth not configured", up.ID)
	}
	identity, _ := json.Marshal(up.OAuth)
	cacheKey := fmt.Sprintf("%x", sha256.Sum256(identity))
	oc.mu.Lock()
	t, ok := oc.toks[cacheKey]
	oc.mu.Unlock()
	if ok && time.Now().Before(t.expiry.Add(-30*time.Second)) {
		return t.token, nil
	}
	// fetch fresh token (client_credentials)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {up.OAuth.ClientID},
		"client_secret": {up.OAuth.ClientSecret},
	}
	if up.OAuth.Scope != "" {
		form.Set("scope", up.OAuth.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", up.OAuth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tr := &http.Transport{}
	if p.Cfg.Get().ModelDiscovery.IgnoreCertErrors {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: tr, CheckRedirect: protocol.NoRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth token fetch HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("oauth token parse failed: %s", truncate(string(body), 200))
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 300
	}
	oc.mu.Lock()
	if len(oc.toks) >= 2048 {
		for k := range oc.toks {
			delete(oc.toks, k)
			break
		}
	}
	oc.toks[cacheKey] = oauthToken{token: tok.AccessToken, expiry: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	oc.mu.Unlock()
	return tok.AccessToken, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
