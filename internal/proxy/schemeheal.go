package proxy

import (
	"strings"
	"time"

	"aisense/internal/config"
)

// Some upstreams are configured with the wrong URL scheme — an https:// base
// whose server actually speaks plain HTTP. Every request then dies in the
// transport with "server gave HTTP response to HTTPS client" before any
// policy rule can react, and the walk burns attempts on it forever. When the
// transport reports exactly that mismatch, the request is retried once over
// the opposite scheme and the working scheme is remembered for later
// requests. Only the scheme is remembered (never a full URL), so an operator
// who edits the base URL is always respected, and the memory expires so a
// fixed server is re-probed on its configured scheme.

const schemeHealTTL = time.Hour

type schemeHealEntry struct {
	scheme string
	at     time.Time
}

// schemeMismatchError reports whether the transport error proves the URL
// scheme and the server's actual protocol disagree.
func schemeMismatchError(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "server gave HTTP response to HTTPS client") ||
		strings.Contains(text, "server gave HTTPS response to HTTP client") ||
		strings.Contains(text, "tls: first record does not look like a TLS handshake")
}

// swappedScheme returns the base URL with the other scheme, or "" when the
// base does not start with either scheme.
func swappedScheme(base string) string {
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return "http://" + base[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		return "https://" + base[len("http://"):]
	}
	return ""
}

func (p *Proxy) rememberSchemeHeal(upstreamID, base string) {
	scheme := ""
	if swapped := swappedScheme(base); swapped != "" {
		if strings.HasPrefix(strings.ToLower(base), "https://") {
			scheme = "http"
		} else {
			scheme = "https"
		}
	}
	if scheme == "" {
		return
	}
	p.schemeHealMu.Lock()
	defer p.schemeHealMu.Unlock()
	if p.schemeHeals == nil {
		p.schemeHeals = map[string]schemeHealEntry{}
	}
	p.schemeHeals[upstreamID] = schemeHealEntry{scheme: scheme, at: time.Now()}
}

func (p *Proxy) healedScheme(upstreamID string) (string, bool) {
	p.schemeHealMu.Lock()
	defer p.schemeHealMu.Unlock()
	entry, ok := p.schemeHeals[upstreamID]
	if !ok || time.Since(entry.at) > schemeHealTTL {
		return "", false
	}
	return entry.scheme, true
}

// applySchemeHeal returns the upstream to use: a shallow copy with the
// remembered working scheme when the configured base still carries the
// opposite one, otherwise the upstream unchanged.
func (p *Proxy) applySchemeHeal(up *config.Upstream) *config.Upstream {
	if up == nil || up.BaseURL == "" {
		return up
	}
	scheme, ok := p.healedScheme(up.ID)
	if !ok {
		return up
	}
	healed := swappedScheme(up.BaseURL)
	if healed == "" || !strings.HasPrefix(strings.ToLower(healed), scheme+"://") {
		return up
	}
	copy := *up
	copy.BaseURL = healed
	return &copy
}

// healSchemeAttempt reacts to a scheme-mismatch transport failure: remember
// the working scheme and return a copy of the upstream over it.
func (p *Proxy) healSchemeAttempt(up *config.Upstream) *config.Upstream {
	if up == nil || up.BaseURL == "" {
		return nil
	}
	healed := swappedScheme(up.BaseURL)
	if healed == "" {
		return nil
	}
	p.rememberSchemeHeal(up.ID, up.BaseURL)
	copy := *up
	copy.BaseURL = healed
	return &copy
}
