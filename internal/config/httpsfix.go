package config

import (
	"net/url"
	"path"
	"strings"
)

// Plain-http-on-https-port markers returned by common frontends. nginx and
// HAProxy emit "The plain HTTP request was sent to HTTPS port"; other
// gateways emit "Client sent an HTTP request to an HTTPS server."
var plainHTTPMarkers = []string{
	"plain http request was sent to https port", // nginx / haproxy / envoy
	"http request was sent to https",            // nginx variant
	"sent an http request to an https",          // "Client sent an HTTP request to an HTTPS server."
	"http request to an https server",
	"http request to https server",
}

// IsPlainHTTPOnHTTPS reports whether a response body says the request was
// plain HTTP sent to an HTTPS port.
func IsPlainHTTPOnHTTPS(body string) bool {
	b := strings.ToLower(body)
	for _, m := range plainHTTPMarkers {
		if strings.Contains(b, m) {
			return true
		}
	}
	return false
}

// NormalizeAPIKeys trims and de-duplicates a key pool while preserving order.
func NormalizeAPIKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// MergeUpstreamKeys appends unique source keys to dst and returns the number
// added.
func MergeUpstreamKeys(dst, src *Upstream) int {
	before := len(NormalizeAPIKeys(dst.APIKeys))
	dst.APIKeys = NormalizeAPIKeys(append(append([]string(nil), dst.APIKeys...), src.APIKeys...))
	if len(src.KeyBlockedModels) > 0 {
		dst.KeyBlockedModels = CloneModelAliases(dst.KeyBlockedModels)
		if dst.KeyBlockedModels == nil {
			dst.KeyBlockedModels = map[string][]string{}
		}
		for hash, models := range src.KeyBlockedModels {
			dst.KeyBlockedModels[hash] = NormalizeAPIKeys(append(dst.KeyBlockedModels[hash], models...))
		}
	}
	return len(dst.APIKeys) - before
}

func normalizedEndpointParts(baseURL string) (scheme, authority, cleanPath string, ok bool) {
	u, err := url.Parse(NormalizeBaseURL(baseURL))
	if err != nil || u.Scheme == "" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", "", false
	}
	authority = strings.ToLower(u.Hostname())
	if strings.Contains(authority, ":") {
		authority = "[" + authority + "]"
	}
	if port := u.Port(); port != "" {
		authority += ":" + port
	}
	cleanPath = path.Clean("/" + strings.TrimSpace(u.EscapedPath()))
	if cleanPath == "/." {
		cleanPath = "/"
	}
	cleanPath = strings.TrimRight(cleanPath, "/")
	if cleanPath == "" {
		cleanPath = "/"
	}
	return scheme, authority, cleanPath, true
}

// ValidateBaseURL accepts only absolute HTTP(S) endpoint URLs without query
// strings or fragments. Base URLs are routing roots, not individual request
// URLs.
func ValidateBaseURL(baseURL string) error {
	u, err := url.Parse(NormalizeBaseURL(baseURL))
	if err != nil || u.Hostname() == "" {
		if err == nil {
			err = errInvalidAbsolute
		}
		return &url.Error{Op: "parse", URL: baseURL, Err: err}
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return &url.Error{Op: "parse", URL: baseURL, Err: errInvalidScheme}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return &url.Error{Op: "parse", URL: baseURL, Err: errQueryOrFragment}
	}
	return nil
}

type baseURLError string

func (e baseURLError) Error() string { return string(e) }

const (
	errInvalidScheme   baseURLError = "scheme must be http or https"
	errQueryOrFragment baseURLError = "query strings and fragments are not allowed"
	errInvalidAbsolute baseURLError = "absolute host is required"
)

// ExactEndpointIdentity identifies duplicates including their protocol.
func ExactEndpointIdentity(up *Upstream) string {
	if up == nil {
		return ""
	}
	scheme, authority, cleanPath, ok := normalizedEndpointParts(up.BaseURL)
	if !ok {
		return ""
	}
	return strings.ToLower(up.Type) + "|" + scheme + "|" + authority + "|" + cleanPath
}

// ProtocolEndpointIdentity identifies HTTP/HTTPS twins while retaining the
// upstream type, explicit port, and normalized path.
func ProtocolEndpointIdentity(up *Upstream) string {
	if up == nil {
		return ""
	}
	_, authority, cleanPath, ok := normalizedEndpointParts(up.BaseURL)
	if !ok {
		return ""
	}
	return strings.ToLower(up.Type) + "|" + authority + "|" + cleanPath
}

// ConsolidateExactUpstreams keeps the first exact endpoint, appends all
// unique keys, and removes the duplicate rows.
func ConsolidateExactUpstreams(list []*Upstream) ([]*Upstream, int, int) {
	out := make([]*Upstream, 0, len(list))
	seen := map[string]*Upstream{}
	merged, keysAdded := 0, 0
	for _, up := range list {
		if up == nil {
			continue
		}
		up.APIKeys = NormalizeAPIKeys(up.APIKeys)
		identity := ExactEndpointIdentity(up)
		if identity == "" {
			out = append(out, up)
			continue
		}
		if winner := seen[identity]; winner != nil {
			keysAdded += MergeUpstreamKeys(winner, up)
			merged++
			continue
		}
		seen[identity] = up
		out = append(out, up)
	}
	return out, merged, keysAdded
}

// MergeProtocolWinner removes every protocol twin of winner after moving its
// keys into winner. Non-key settings always remain those of winner.
func MergeProtocolWinner(list []*Upstream, winner *Upstream) ([]*Upstream, int, int) {
	identity := ProtocolEndpointIdentity(winner)
	if identity == "" {
		return list, 0, 0
	}
	out := make([]*Upstream, 0, len(list))
	merged, keysAdded := 0, 0
	for _, up := range list {
		if up == nil || up.ID == winner.ID || ProtocolEndpointIdentity(up) != identity {
			out = append(out, up)
			continue
		}
		keysAdded += MergeUpstreamKeys(winner, up)
		merged++
	}
	return out, merged, keysAdded
}

// NormalizeBaseURL extracts a plain http(s):// URL from a possibly wrapped
// value (OmniRoute paste format `@url:`https://host:port/v1“, stray quotes,
// backticks, brackets). Returns the trimmed input when no scheme is found.
func NormalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`'\"<>(){}")
	s = strings.TrimLeft(s, "[]")
	for _, scheme := range []string{"https://", "http://"} {
		if i := strings.Index(s, scheme); i >= 0 {
			s = s[i:]
			break
		}
	}
	stop := len(s)
	brackets := 0
	for i, r := range s {
		if r == '[' {
			brackets++
		}
		if r == ']' && brackets > 0 {
			brackets--
			continue
		}
		if r == '`' || r == '\'' || r == '"' || r == ' ' || r == ',' || r == ';' ||
			r == ')' || r == ']' || r == '}' {
			stop = i
			break
		}
	}
	s = s[:stop]
	s = strings.TrimRight(s, "/")
	return s
}

// HTTPSBaseFor rewrites an http:// base URL to https:// (path preserved).
// Returns "" when the URL does not parse or is not plain http.
func HTTPSBaseFor(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") {
		return ""
	}
	parsed.Scheme = "https"
	return strings.TrimRight(parsed.String(), "/")
}

// OppositeProtocolBase rewrites http↔https while preserving host, port and
// path. It returns an empty string for unsupported or invalid schemes.
func OppositeProtocolBase(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "https"
	case "https":
		parsed.Scheme = "http"
	default:
		return ""
	}
	return strings.TrimRight(parsed.String(), "/")
}
